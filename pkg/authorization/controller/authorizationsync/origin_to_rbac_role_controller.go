package authorizationsync

import (
	"fmt"

	"github.com/golang/glog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/rbac"
	rbacclient "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/rbac/internalversion"
	rbacinformers "k8s.io/kubernetes/pkg/client/informers/informers_generated/internalversion/rbac/internalversion"
	rbaclister "k8s.io/kubernetes/pkg/client/listers/rbac/internalversion"
	"k8s.io/kubernetes/pkg/controller"

	authorizationapi "github.com/openshift/origin/pkg/authorization/api"
	origininformers "github.com/openshift/origin/pkg/authorization/generated/informers/internalversion/authorization/internalversion"
	originlister "github.com/openshift/origin/pkg/authorization/generated/listers/authorization/internalversion"
)

type OriginRoleToRBACRoleController struct {
	rbacClient rbacclient.RolesGetter

	rbacLister    rbaclister.RoleLister
	originIndexer cache.Indexer
	originLister  originlister.RoleLister

	genericController
}

func NewOriginToRBACRoleController(rbacRoleInformer rbacinformers.RoleInformer, originPolicyInformer origininformers.PolicyInformer, rbacClient rbacclient.RolesGetter) *OriginRoleToRBACRoleController {
	originIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	c := &OriginRoleToRBACRoleController{
		rbacClient:    rbacClient,
		rbacLister:    rbacRoleInformer.Lister(),
		originIndexer: originIndexer,
		originLister:  originlister.NewRoleLister(originIndexer),

		genericController: genericController{
			name: "OriginRoleToRBACRoleController",
			cachesSynced: func() bool {
				return rbacRoleInformer.Informer().HasSynced() && originPolicyInformer.Informer().HasSynced()
			},
			queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "origin-to-rbac-role"),
		},
	}
	c.genericController.syncFunc = c.syncRole

	rbacRoleInformer.Informer().AddEventHandler(naiveEventHandler(c.queue))
	originPolicyInformer.Informer().AddEventHandler(c.policyEventHandler())

	return c
}

func (c *OriginRoleToRBACRoleController) syncRole(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	rbacRole, rbacErr := c.rbacLister.Roles(namespace).Get(name)
	if !apierrors.IsNotFound(rbacErr) && rbacErr != nil {
		return rbacErr
	}
	originRole, originErr := c.originLister.Roles(namespace).Get(name)
	if !apierrors.IsNotFound(originErr) && originErr != nil {
		return originErr
	}

	// if neither role exists, return
	if apierrors.IsNotFound(rbacErr) && apierrors.IsNotFound(originErr) {
		return nil
	}
	// if the origin role doesn't exist, just delete the rbac role
	if apierrors.IsNotFound(originErr) {
		// orphan on delete to minimize fanout.  We ought to clean the rest via controller too.
		return c.rbacClient.Roles(namespace).Delete(name, nil)
	}

	// convert the origin role to an rbac role and compare the results
	convertedRole := &rbac.Role{}
	if err := authorizationapi.Convert_api_Role_To_rbac_Role(originRole, convertedRole, nil); err != nil {
		return err
	}
	// do a deep copy here since conversion does not guarantee a new object.
	equivalentRole := &rbac.Role{}
	if err := rbac.DeepCopy_rbac_Role(convertedRole, equivalentRole, cloner); err != nil {
		return err
	}

	// if we're missing the rbacRole, create it
	if apierrors.IsNotFound(rbacErr) {
		equivalentRole.ResourceVersion = ""
		_, err := c.rbacClient.Roles(namespace).Create(equivalentRole)
		return err
	}

	// if we might need to update, we need to stomp fields that are never going to match like uid and creation time
	equivalentRole.SelfLink = rbacRole.SelfLink
	equivalentRole.UID = rbacRole.UID
	equivalentRole.ResourceVersion = rbacRole.ResourceVersion
	equivalentRole.CreationTimestamp = rbacRole.CreationTimestamp

	// if they're equal, we have no work to do
	if kapi.Semantic.DeepEqual(equivalentRole, rbacRole) {
		return nil
	}

	glog.V(1).Infof("writing RBAC role %v/%v", namespace, name)
	_, err = c.rbacClient.Roles(namespace).Update(equivalentRole)
	// if the update was invalid, we're probably changing an immutable field or something like that
	// either way, the existing object is wrong.  Delete it and try again.
	if apierrors.IsInvalid(err) {
		c.rbacClient.Roles(namespace).Delete(name, nil)
	}
	return err
}

func (c *OriginRoleToRBACRoleController) policyEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			originContainerObj := obj.(*authorizationapi.Policy)
			for _, originObj := range originContainerObj.Roles {
				c.originIndexer.Add(originObj)
				key, err := controller.KeyFunc(originObj)
				if err != nil {
					utilruntime.HandleError(err)
					continue
				}
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			originContainerObj := cur.(*authorizationapi.Policy)
			for _, originObj := range originContainerObj.Roles {
				c.originIndexer.Add(originObj)
				key, err := controller.KeyFunc(originObj)
				if err != nil {
					utilruntime.HandleError(err)
					continue
				}
				c.queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			originContainerObj, ok := obj.(*authorizationapi.Policy)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
				}
				originContainerObj, ok = tombstone.Obj.(*authorizationapi.Policy)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a runtime.Object %#v", obj))
				}
			}

			for _, originObj := range originContainerObj.Roles {
				c.originIndexer.Add(originObj)
				key, err := controller.KeyFunc(originObj)
				if err != nil {
					utilruntime.HandleError(err)
					continue
				}
				c.queue.Add(key)
			}
		},
	}
}
