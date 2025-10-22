/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUTHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ingress

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	networkingv1informers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	networkinglisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	syncToken = "token"
)

// Controller is the controller implementation for Ingress resources
type Controller struct {
	clientset        kubernetes.Interface
	ingressClassName string
	isDefaultClass   atomic.Bool
	proxy            *Proxy

	ingressLister networkinglisters.IngressLister
	svcLister     corelisters.ServiceLister
	secretLister  corelisters.SecretLister

	ingressSynced cache.InformerSynced
	svcSynced     cache.InformerSynced
	secretSynced  cache.InformerSynced

	workqueue workqueue.TypedRateLimitingInterface[string]
}

// NewController returns a new ingress controller
func NewController(clientset kubernetes.Interface,
	ingressClassName string,
	proxy *Proxy,
	ingressInformer networkingv1informers.IngressInformer,
	svcInformer corev1informers.ServiceInformer,
	secretInformer corev1informers.SecretInformer,
	ingressClassInformer networkingv1informers.IngressClassInformer,
) (*Controller, error) {

	controller := &Controller{
		clientset:        clientset,
		proxy:            proxy,
		ingressClassName: ingressClassName,
		ingressLister:    ingressInformer.Lister(),
		svcLister:        svcInformer.Lister(),
		secretLister:     secretInformer.Lister(),
		ingressSynced:    ingressInformer.Informer().HasSynced,
		svcSynced:        svcInformer.Informer().HasSynced,
		secretSynced:     secretInformer.Informer().HasSynced,
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "ingress"},
		),
	}

	klog.Info("Setting up event handlers")
	// Set up an event handler for when Ingress resources change
	_, err := ingressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueIngress,
		UpdateFunc: func(old, new interface{}) {
			// We only care about changes to spec or deletion timestamp
			oldIng := old.(*networkingv1.Ingress)
			newIng := new.(*networkingv1.Ingress)
			if !reflect.DeepEqual(oldIng.Spec, newIng.Spec) ||
				!oldIng.DeletionTimestamp.Equal(newIng.DeletionTimestamp) {
				controller.enqueueIngress(new)
			}
		},
		DeleteFunc: controller.enqueueIngress,
	})
	if err != nil {
		return nil, fmt.Errorf("fail to add handler to ingress informer: %w", err)
	}

	// Set up an event handler for when Service resources change.
	// We need to trigger a sync for any Ingress that references this Service.
	_, err = svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handleServiceChange,
		UpdateFunc: func(old, new interface{}) { controller.handleServiceChange(new) },
		DeleteFunc: controller.handleServiceChange,
	})
	if err != nil {
		return nil, fmt.Errorf("fail to add handler to service informer: %w", err)
	}

	// TODO: make this better by watching only the relevant IngressClass
	_, err = ingressClassInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ingressClass := obj.(*networkingv1.IngressClass)
			if ingressClass.Name == controller.ingressClassName {
				if ingressClass.Annotations[networkingv1.AnnotationIsDefaultIngressClass] == "true" {
					controller.isDefaultClass.Store(true)
					controller.workqueue.Add(syncToken)
				} else {
					controller.isDefaultClass.Store(false)
				}
			}
		},
		UpdateFunc: func(old, new interface{}) {
			ingressClass := new.(*networkingv1.IngressClass)
			if ingressClass.Name == controller.ingressClassName {
				if ingressClass.Annotations[networkingv1.AnnotationIsDefaultIngressClass] == "true" {
					controller.isDefaultClass.Store(true)
				} else {
					controller.isDefaultClass.Store(false)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {},
	})
	if err != nil {
		return nil, fmt.Errorf("fail to add handler to ingressClass informer: %w", err)
	}

	return controller, nil
}

// Run will set up the event handlers for types we are interested in, as well
// as start processing components for the specified number of workers.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer c.workqueue.ShutDown()

	klog.Info("Starting Ingress controller")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.ingressSynced, c.svcSynced, c.secretSynced) {
		klog.Fatal("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// Launch workers to process Ingress resources
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	klog.Info("Started workers")
	<-ctx.Done()
	klog.Info("Shutting down workers")
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	// collapse the items in case there are multiple because we just
	// reconcile the entire state in each sync
	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func() error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We must call Forget if we do not want
		// this item related (best effort).
		defer c.workqueue.Done(key)

		// Run the syncHandler, passing it the key
		if err := c.reconcileProxyConfig(ctx); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing: %s, requeuing", err.Error())
		}

		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(key)
		klog.V(4).Infof("Successfully synced")
		return nil
	}()

	if err != nil {
		klog.Error(err)
	}

	return true
}

// enqueueIngress takes an Ingress resource and converts it into a
// key string which is then put onto the work queue.
func (c *Controller) enqueueIngress(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		klog.Error(err)
		return
	}
	klog.V(4).Infof("enqueuing ingress %s", key)
	c.workqueue.Add(syncToken) // we just sync everything
}

// handleServiceChange finds all Ingress objects that reference a given Service
// and enqueues them for reconciliation.
func (c *Controller) handleServiceChange(obj interface{}) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		klog.Warningf("Expected Service but got %#v", obj)
		return
	}

	// List all Ingresses in all namespaces (or just svc.Namespace if preferred)
	ingresses, err := c.ingressLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list Ingresses: %v", err)
		return
	}

	for _, ingress := range ingresses {
		// Check if Ingress is in the same namespace
		if ingress.Namespace != svc.Namespace {
			continue
		}

		if !c.isIngressForUs(ingress) {
			continue
		}

		// Check default backend
		if ingress.Spec.DefaultBackend != nil && ingress.Spec.DefaultBackend.Service.Name == svc.Name {
			c.enqueueIngress(ingress)
			return // we sync all ingresses only once
		}

		// Check rules
		for _, rule := range ingress.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service.Name == svc.Name {
					c.enqueueIngress(ingress)
					return // we sync all ingresses only once
				}
			}
		}
	}
}

// isIngressForUs checks if an Ingress belongs to this controller
func (c *Controller) isIngressForUs(ingress *networkingv1.Ingress) bool {
	if ingress.Spec.IngressClassName == nil {
		// We only handle Ingresses that explicitly name us
		// except if we are the default class
		if c.isDefaultClass.Load() {
			return true
		}
		return false
	}
	return *ingress.Spec.IngressClassName == c.ingressClassName
}

// reconcileProxyConfig is the core logic. It builds the *entire*
// proxy configuration from scratch and updates the proxy.
func (c *Controller) reconcileProxyConfig(ctx context.Context) error {
	klog.Info("Starting full reconciliation of proxy config")
	start := time.Now()
	defer func() {
		// Use klog.V(2) for "info" level verbosity
		klog.V(2).Infof("Completed reconciliation of proxy config in %v", time.Since(start))
	}()

	newConfig := &ProxyConfiguration{
		hostRules: make(map[string]*http.ServeMux),
		tlsCerts:  make(map[string]*tls.Certificate),
	}

	// Filter for Ingresses that belong to us
	ingresses, err := c.ingressLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list ingresses: %w", err)
	}

	var ourIngresses []*networkingv1.Ingress
	for _, ingress := range ingresses {
		if c.isIngressForUs(ingress) {
			ourIngresses = append(ourIngresses, ingress)
		}
	}

	// Process all our Ingresses
	for _, ingress := range ourIngresses {
		// Process TLS configuration
		for _, tlsSpec := range ingress.Spec.TLS {
			secret, err := c.getTLSSecret(ingress.Namespace, tlsSpec.SecretName)
			if err != nil {
				klog.Errorf("Failed to get secret %s/%s for ingress %s/%s: %v",
					ingress.Namespace, tlsSpec.SecretName, ingress.Namespace, ingress.Name, err)
				continue // Skip this TLS spec
			}

			// Load the cert
			cert, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
			if err != nil {
				klog.Errorf("Failed to parse keypair from secret %s/%s: %v",
					ingress.Namespace, tlsSpec.SecretName, err)
				continue
			}

			// Add the cert for all hosts it's configured for
			for _, host := range tlsSpec.Hosts {
				newConfig.tlsCerts[host] = &cert
				klog.V(4).Infof("Loaded TLS cert for host: %s", host)
			}
		}

		// Process Default Backend
		if ingress.Spec.DefaultBackend != nil {
			proxy, err := c.createBackendProxy(ingress.Namespace, ingress.Spec.DefaultBackend)
			if err != nil {
				klog.Errorf("Failed to create default backend proxy for ingress %s/%s: %v",
					ingress.Namespace, ingress.Name, err)
			} else {
				// Note: This will be overwritten by the last Ingress to define it.
				newConfig.defaultBackend = proxy
			}
		}

		// Process Rules
		for _, rule := range ingress.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}

			// Get or create the router for this host
			host := rule.Host
			if host == "" {
				host = "*"
			}
			mux, ok := newConfig.hostRules[host]
			if !ok {
				mux = http.NewServeMux()
			}

			var pathsAdded int
			for _, path := range rule.HTTP.Paths {
				proxy, err := c.createBackendProxy(ingress.Namespace, &path.Backend)
				if err != nil {
					klog.Errorf("Failed to create backend proxy for ingress %s/%s, host %s, path %s: %v",
						ingress.Namespace, ingress.Name, rule.Host, path.Path, err)
					continue
				}

				pathType := networkingv1.PathTypePrefix // Default if not set
				if path.PathType != nil {
					pathType = *path.PathType
				}

				pathString := path.Path
				var instrumentedProxy http.Handler

				switch pathType {
				case networkingv1.PathTypeExact:
					// http.ServeMux treats paths *without* trailing slash as exact
					// (unless it's just "/")
					if pathString != "/" && strings.HasSuffix(pathString, "/") {
						pathString = strings.TrimRight(pathString, "/")
					}
					instrumentedProxy = c.proxy.instrumentHandler(proxy)
					mux.Handle(pathString, instrumentedProxy)
					pathsAdded++
					klog.V(4).Infof("Mapped [Exact] route: %s%s -> %s", host, pathString, path.Backend.Service.Name)

				case networkingv1.PathTypePrefix:
					// http.ServeMux treats paths *with* trailing slash as prefix.
					// It also redirects requests from /path to /path/ if a handler for /path/ exists,
					// but this redirect does not happen for requests with a body.
					// To handle all cases for a prefix path, we register handlers for the path
					// with and without a trailing slash.
					instrumentedProxy = c.proxy.instrumentHandler(proxy)

					withSlash := pathString
					if !strings.HasSuffix(withSlash, "/") {
						withSlash += "/"
					}
					mux.Handle(withSlash, instrumentedProxy)
					pathsAdded++
					klog.V(4).Infof("Mapped [Prefix] route: %s%s -> %s", host, withSlash, path.Backend.Service.Name)

					withoutSlash := strings.TrimSuffix(pathString, "/")
					// Don't register "" for root path, and don't re-register if they are the same.
					if withoutSlash != "" && withoutSlash != withSlash {
						mux.Handle(withoutSlash, instrumentedProxy)
						pathsAdded++
						klog.V(4).Infof("Mapped [Prefix] as exact route: %s%s -> %s", host, withoutSlash, path.Backend.Service.Name)
					}

				case networkingv1.PathTypeImplementationSpecific:
					klog.Warningf("Skipping path %s/%s: PathTypeImplementationSpecific is not supported",
						host, pathString)
					continue

				default:
					klog.Errorf("Skipping path %s%s: Unknown PathType %s",
						rule.Host, pathString, pathType)
					continue
				}
			}
			if pathsAdded > 0 {
				klog.V(4).Infof("Added %d paths for host %s", pathsAdded, host)
				newConfig.hostRules[host] = mux
			}
		}
	}

	// Atomically update the proxy's active configuration
	c.proxy.UpdateConfig(newConfig)

	return nil
}

// getTLSSecret fetches and validates a TLS secret
func (c *Controller) getTLSSecret(namespace, name string) (*corev1.Secret, error) {
	secret, err := c.secretLister.Secrets(namespace).Get(name)
	if err != nil {
		return nil, err
	}
	if secret.Type != corev1.SecretTypeTLS {
		return nil, fmt.Errorf("secret %s/%s is not of type %s", namespace, name, corev1.SecretTypeTLS)
	}
	if _, ok := secret.Data[corev1.TLSCertKey]; !ok {
		return nil, fmt.Errorf("secret %s/%s is missing %s", namespace, name, corev1.TLSCertKey)
	}
	if _, ok := secret.Data[corev1.TLSPrivateKeyKey]; !ok {
		return nil, fmt.Errorf("secret %s/%s is missing %s", namespace, name, corev1.TLSPrivateKeyKey)
	}
	return secret, nil
}

// createBackendProxy creates a reverse proxy for a given Ingress backend
func (c *Controller) createBackendProxy(namespace string, backend *networkingv1.IngressBackend) (http.Handler, error) {
	if backend.Service == nil {
		return nil, fmt.Errorf("backend is not a service")
	}

	svcName := backend.Service.Name
	svcPort := backend.Service.Port

	// Get the Service
	svc, err := c.svcLister.Services(namespace).Get(svcName)
	if err != nil {
		return nil, fmt.Errorf("failed to get service %s/%s: %w", namespace, svcName, err)
	}

	var targetPort int32
	switch {
	case svcPort.Number != 0:
		targetPort = svcPort.Number
	case svcPort.Name != "":
		// Find the port by name
		found := false
		for _, port := range svc.Spec.Ports {
			if port.Name == svcPort.Name {
				targetPort = port.Port
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("service %s/%s has no port named %s", namespace, svcName, svcPort.Name)
		}
	default:
		return nil, fmt.Errorf("service port not specified for service %s/%s", namespace, svcName)
	}

	// Build the target URL
	// We use the ClusterIP for simple in-cluster proxying
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return nil, fmt.Errorf("service %s/%s has no ClusterIP", namespace, svcName)
	}

	targetURLStr := fmt.Sprintf("http://%s", net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(targetPort))))
	targetURL, err := url.Parse(targetURLStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse target URL %s: %w", targetURLStr, err)
	}

	// Create the reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	return proxy, nil
}
