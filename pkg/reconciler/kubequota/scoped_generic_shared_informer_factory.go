/*
Copyright 2022 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubequota

import (
	"sync"

	"github.com/kcp-dev/logicalcluster/v2"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clusters"
	"k8s.io/controller-manager/pkg/informerfactory"

	"github.com/kcp-dev/kcp/pkg/indexers"
)

// scopingGenericSharedInformerFactory wraps an informerfactory.InformerFactory and centralizes informer event handling
// for a resource across potentially multiple controllers for multiple logical clusters.
type scopingGenericSharedInformerFactory struct {
	delegatingEventHandler *delegatingEventHandler
	factory                informerfactory.InformerFactory
}

// newScopingGenericSharedInformerFactory returns a new scopingGenericSharedInformerFactory.
func newScopingGenericSharedInformerFactory(factory informerfactory.InformerFactory) *scopingGenericSharedInformerFactory {
	return &scopingGenericSharedInformerFactory{
		delegatingEventHandler: newDelegatingEventHandler(),
		factory:                factory,
	}
}

// ForCluster returns a scopedGenericSharedInformerFactory scoped to clusterName.
func (f *scopingGenericSharedInformerFactory) ForCluster(clusterName logicalcluster.Name) *scopedGenericSharedInformerFactory {
	return &scopedGenericSharedInformerFactory{
		clusterName: clusterName,
		delegate:    f.factory,

		informers: map[schema.GroupVersionResource]*scopedGenericInformer{},

		delegatingEventHandler: f.delegatingEventHandler,
	}
}

// scopedGenericSharedInformerFactory wraps an informerfactory.InformerFactory and produces instances of
// informers.GenericInformer that are scoped to a single logical cluster.
type scopedGenericSharedInformerFactory struct {
	clusterName logicalcluster.Name
	delegate    informerfactory.InformerFactory

	lock      sync.RWMutex
	informers map[schema.GroupVersionResource]*scopedGenericInformer

	delegatingEventHandler *delegatingEventHandler
}

// Start starts the underlying informer factory.
func (f *scopedGenericSharedInformerFactory) Start(stop <-chan struct{}) {
	f.delegate.Start(stop)
}

// ForResource returns a generic informer implementation that is scoped to a single logical cluster.
func (f *scopedGenericSharedInformerFactory) ForResource(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
	var informer *scopedGenericInformer

	f.lock.RLock()
	informer = f.informers[resource]
	f.lock.RUnlock()

	if informer != nil {
		return informer, nil
	}

	f.lock.Lock()
	defer f.lock.Unlock()

	informer = f.informers[resource]
	if informer != nil {
		return informer, nil
	}

	delegate, err := f.delegate.ForResource(resource)
	if err != nil {
		return nil, err
	}

	informer = &scopedGenericInformer{
		delegate:               delegate,
		clusterName:            f.clusterName,
		resource:               resource.GroupResource(),
		delegatingEventHandler: f.delegatingEventHandler,
	}

	f.informers[resource] = informer

	return informer, nil
}

// scopedGenericInformer wraps an informers.GenericInformer and produces instances of cache.GenericLister that are
// scoped to a single logical cluster.
type scopedGenericInformer struct {
	delegate               informers.GenericInformer
	clusterName            logicalcluster.Name
	resource               schema.GroupResource
	delegatingEventHandler *delegatingEventHandler
}

// Informer invokes Informer() on the underlying informers.GenericInformer.
func (s *scopedGenericInformer) Informer() cache.SharedIndexInformer {
	return &delegatingInformer{
		clusterName:            s.clusterName,
		SharedIndexInformer:    s.delegate.Informer(),
		delegatingEventHandler: s.delegatingEventHandler,
	}
}

// Lister returns an implementation of cache.GenericLister that is scoped to a single logical cluster.
func (s *scopedGenericInformer) Lister() cache.GenericLister {
	return &scopedGenericLister{
		indexer:     s.delegate.Informer().GetIndexer(),
		clusterName: s.clusterName,
		resource:    s.resource,
	}
}

// scopedGenericLister wraps a cache.Indexer to implement a cache.GenericLister that is scoped to a single logical
// cluster.
type scopedGenericLister struct {
	indexer     cache.Indexer
	clusterName logicalcluster.Name
	resource    schema.GroupResource
}

// List returns all instances from the cache.Indexer scoped to a single logical cluster and matching selector.
func (s *scopedGenericLister) List(selector labels.Selector) (ret []runtime.Object, err error) {
	err = listByIndex(s.indexer, indexers.ByLogicalCluster, s.clusterName.String(), selector, func(obj interface{}) {
		ret = append(ret, obj.(runtime.Object))
	})
	return ret, err
}

// ByNamespace returns an implementation of cache.GenericNamespaceLister that is scoped to a single logical cluster.
func (s *scopedGenericLister) ByNamespace(namespace string) cache.GenericNamespaceLister {
	return &scopedGenericNamespaceLister{
		indexer:     s.indexer,
		clusterName: s.clusterName,
		namespace:   namespace,
		resource:    s.resource,
	}
}

// Get returns the runtime.Object from the cache.Indexer identified by name, from the appropriate logical cluster.
func (s *scopedGenericLister) Get(name string) (runtime.Object, error) {
	key := clusters.ToClusterAwareKey(s.clusterName, name)
	obj, exists, err := s.indexer.GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(s.resource, name)
	}
	return obj.(runtime.Object), nil
}

// scopedGenericNamespaceLister wraps a cache.Indexer to implement a cache.GenericNamespaceLister that is scoped to a
// single logical cluster.
type scopedGenericNamespaceLister struct {
	indexer     cache.Indexer
	clusterName logicalcluster.Name
	namespace   string
	resource    schema.GroupResource
}

// List lists all instances from the cache.Indexer scoped to a single logical cluster and namespace, and matching
// selector.
func (s *scopedGenericNamespaceLister) List(selector labels.Selector) (ret []runtime.Object, err error) {
	// To support e.g. quota for cluster-scoped resources, we've hacked the k8s quota to use namespace="" when
	// checking quota for cluster-scoped resources. But because all the upstream quota code is written to only
	// support namespace-scoped resources, we have to hack the "namespace lister" to support returning all items
	// when its namespace is "".
	var indexName, indexValue string
	if s.namespace == "" {
		indexName = indexers.ByLogicalCluster
		indexValue = s.clusterName.String()
	} else {
		indexName = indexers.ByLogicalClusterAndNamespace
		indexValue = clusters.ToClusterAwareKey(s.clusterName, s.namespace)
	}
	err = listByIndex(s.indexer, indexName, indexValue, selector, func(obj interface{}) {
		ret = append(ret, obj.(runtime.Object))
	})
	return ret, err
}

// Get returns the runtime.Object from the cache.Indexer identified by name, from the appropriate logical cluster and
// namespace.
func (s *scopedGenericNamespaceLister) Get(name string) (runtime.Object, error) {
	obj, exists, err := s.indexer.GetByKey(s.namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(s.resource, name)
	}
	return obj.(runtime.Object), nil
}
