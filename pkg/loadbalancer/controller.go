package loadbalancer

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/aojea/kloudflarelb/pkg/cloudflared"
	"github.com/aojea/kloudflarelb/pkg/config"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	controllerName = "kcloudflare-lb-controller"
	maxRetries     = 12
)

// Controller implements a loadbalancer controller that associates a Cloudflare tunnel
// to a Service of LoadBalancer type with the name <service-name>-[<port-name>]-<namespace>.<tunnel-domain>
// https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/install-and-setup/tunnel-guide
//
// If not tunnelID and/or credentials file is specified it uses the TryCloudflare generated URLs
// https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/run-tunnel/trycloudflare

type Controller struct {
	config config.Config

	client clientset.Interface
	queue  workqueue.RateLimitingInterface
	// serviceLister is able to list/get services and is populated by the shared informer passed to
	serviceLister corelisters.ServiceLister
	// servicesSynced returns true if the service shared informer has been synced at least once.
	servicesSynced cache.InformerSynced
	// workerLoopPeriod is the time between worker runs. The workers process the queue of service and pod changes.
	workerLoopPeriod time.Duration

	// track services and associated tunnels
	mu             sync.Mutex
	serviceTracker map[string]ingress
}

func NewController(
	config config.Config,
	client clientset.Interface,
	serviceInformer coreinformers.ServiceInformer) *Controller {

	c := &Controller{
		config:           config,
		client:           client,
		queue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
		workerLoopPeriod: time.Second,
		serviceTracker:   map[string]ingress{},
	}
	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	})
	c.serviceLister = serviceInformer.Lister()
	c.servicesSynced = serviceInformer.Informer().HasSynced
	return c
}

// Run will not return until stopCh is closed. workers determines how many
// endpoints will be handled in parallel.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting controller %s", controllerName)
	defer klog.Infof("Shutting down controller %s", controllerName)

	// Wait for the caches to be synced
	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, c.servicesSynced) {
		return fmt.Errorf("error syncing cache")
	}

	// Start the workers after the repair loop to avoid races
	klog.Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, c.workerLoopPeriod, stopCh)
	}

	<-stopCh
	return nil
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same service
// at the same time.
func (c *Controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	eKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(eKey)

	err := c.syncServices(eKey.(string))
	c.handleErr(err, eKey)

	return true
}

func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	ns, name, keyErr := cache.SplitMetaNamespaceKey(key.(string))
	if keyErr != nil {
		klog.ErrorS(err, "Failed to split meta namespace cache key", "key", key)
	}

	if c.queue.NumRequeues(key) < maxRetries {
		klog.V(2).InfoS("Error syncing service, retrying", "service", klog.KRef(ns, name), "err", err)
		c.queue.AddRateLimited(key)
		return
	}

	klog.Warningf("Dropping service %q out of the queue: %v", key, err)
	c.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (c *Controller) syncServices(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for service %s on namespace %s ", name, namespace)

	defer func() {
		klog.V(4).Infof("Finished syncing service %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	// Get current Service from the cache
	service, err := c.serviceLister.Services(namespace).Get(name)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// service no longer exist or is no longer type loadbalancer
	// release the tunnel if it has one associated and stop tracking the service
	if err != nil || service.Spec.Type != v1.ServiceTypeLoadBalancer {
		// return if we were not tracking this service
		ingress := c.getServiceIngress(key)
		if ingress.hostname == "" || ingress.service == "" {
			return nil
		}
		// clear the service status if the service has mutated
		if err == nil {
			service.Status.LoadBalancer = v1.LoadBalancerStatus{}
			_, errUpdate := c.client.CoreV1().Services(namespace).UpdateStatus(context.TODO(), service, metav1.UpdateOptions{})
			if errUpdate != nil {
				return errUpdate
			}
		}
		klog.Infof("Release Cloudflared Ingress %v for service %s on namespace %s ", ingress, name, namespace)
		c.deleteService(key)
		return nil
	}
	// service is LoadBalancer check if it already has associated an ingress
	// This can happen after the controller restarts
	for _, i := range service.Status.LoadBalancer.Ingress {
		klog.Infof("Update IP %s for service %s on namespace %s ", i.Hostname, name, namespace)
		c.addService(key, ingress{
			hostname: i.Hostname,
			// TODO: support multiport
			service: net.JoinHostPort(service.Spec.ClusterIP, strconv.Itoa(int(service.Spec.Ports[0].Port))),
		})
		return nil
	}
	// assign a tunnel URI to the service
	lbHostname := service.Name + "-" + service.Namespace
	if len(c.config.Domain) > 0 {

	}
	service.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{Hostname: lbHostname}}
	_, err = c.client.CoreV1().Services(namespace).UpdateStatus(context.TODO(), service, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	klog.Infof("Assign Hostname %s for service %s on namespace %s ", lbHostname, name, namespace)
	c.addService(key, ingress{
		hostname: lbHostname,
		// TODO: support multiport
		service: net.JoinHostPort(service.Spec.ClusterIP, strconv.Itoa(int(service.Spec.Ports[0].Port))),
	})
	return nil
}

// handlers

// onServiceUpdate queues the Service for processing.
func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding service %s", key)
	c.queue.Add(key)
}

// onServiceUpdate updates the Service Selector in the cache and queues the Service for processing.
func (c *Controller) onServiceUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*v1.Service)
	newService := newObj.(*v1.Service)

	// don't process resync or objects that are marked for deletion
	if oldService.ResourceVersion == newService.ResourceVersion ||
		!newService.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.queue.Add(key)
	}
}

// onServiceDelete queues the Service for processing.
func (c *Controller) onServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting service %s", key)
	c.queue.Add(key)
}

// service tracker

// ref: https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/configuration/configuration-file/ingress
type ingress struct {
	hostname string // public URI (hostname)
	service  string // internal service URI hostname:port
}

// add or update service
func (c *Controller) addService(key string, ingress ingress) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serviceTracker[key] = ingress
	return
}

func (c *Controller) getServiceIngress(key string) ingress {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serviceTracker[key]
}

func (c *Controller) deleteService(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.serviceTracker, key)

}

// get the tracker map
func (c *Controller) writeConfig() {
	c.mu.Lock()
	defer c.mu.Unlock()
	config := cloudflared.NewFromConfig(c.config)

	// Copy from the original map to the target map
	for _, ingress := range c.serviceTracker {
		config.AddIngress(ingress.hostname, ingress.service)
	}
	config.Write()
}
