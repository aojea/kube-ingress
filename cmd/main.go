/*
Copyright 2025 The Kubernetes Authors.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/aojea/kube-ingress/pkg/ingress"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	// IngressClassName is the name of the IngressClass resource
	IngressClassName = "kube-ingress"
	// IngressClassController is the value of the spec.controller field
	IngressClassController = "k8s.io/kube-ingress"
)

var (
	kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	httpAddr   = flag.String("http-addr", ":8080", "Address for the HTTP listener (e.g., :80)")
	httpsAddr  = flag.String("https-addr", ":8443", "Address for the HTTPS listener (e.g., :443)")
	statusAddr = flag.String("status-addr", ":9090", "Address for the metrics, healthz, and pprof server.")
	ready      atomic.Bool
)

func main() {
	klog.InitFlags(nil)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: kube-ingress [options]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	flag.VisitAll(func(f *flag.Flag) {
		klog.Infof("FLAG: --%s=%q", f.Name, f.Value)
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Defer error handling for the metrics server
	defer runtime.HandleCrash()

	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok")) //nolint:errcheck
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready")) //nolint:errcheck
		}
	})

	// Metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())

	// Pprof endpoints
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	go func() {
		klog.Infof("Starting metrics/healthz/pprof server on %s", *statusAddr)
		err := http.ListenAndServe(*statusAddr, mux)
		// HandleError logs the error but does not panic
		runtime.HandleError(err)
	}()

	var config *rest.Config
	var err error
	if *kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		// creates the in-cluster config
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		klog.Fatalf("can not create client-go configuration: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create clientset: %v", err)
	}

	err = ensureIngressClass(ctx, clientset, IngressClassName, IngressClassController)
	if err != nil {
		klog.Fatalf("Failed to ensure IngressClass: %v", err)
	}

	// Create the proxy server
	proxy := ingress.NewProxy(*httpAddr, *httpsAddr)

	// Create the shared informer factory
	factory := informers.NewSharedInformerFactory(clientset, 0) // 0 = default resync period

	// Obtain references to shared informers
	ingressInformer := factory.Networking().V1().Ingresses()
	svcInformer := factory.Core().V1().Services()
	secretInformer := factory.Core().V1().Secrets()
	ingressClass := factory.Networking().V1().IngressClasses()

	// Create the new controller. This will also set up the event handlers.
	controller, err := ingress.NewController(clientset, IngressClassName, proxy, ingressInformer, svcInformer, secretInformer, ingressClass)
	if err != nil {
		klog.Fatalf("Failed to create controller: %v", err)
	}

	// Start the informers. This must happen *after* event handlers are registered.
	factory.Start(ctx.Done())

	// Run the proxy's HTTP/HTTPS listeners
	go proxy.Run(ctx)

	// Run the controller.
	go controller.Run(ctx, 2)
	ready.Store(true)

	// Wait for the context to be cancelled
	<-ctx.Done()
	klog.Infoln("Shutting down.")
}

// ensureIngressClass checks if the controller's IngressClass exists, and creates it if it does not.
func ensureIngressClass(ctx context.Context, clientset kubernetes.Interface, className, controllerName string) error {
	klog.Infof("Checking for IngressClass '%s'", className)

	_, err := clientset.NetworkingV1().IngressClasses().Get(ctx, className, metav1.GetOptions{})
	if err == nil {
		klog.Infof("IngressClass '%s' already exists", className)
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get IngressClass '%s': %v", className, err)
	}

	klog.Infof("IngressClass '%s' not found, creating...", className)

	ingressClass := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: className,
		},
		Spec: networkingv1.IngressClassSpec{
			Controller: controllerName,
		},
	}

	_, createErr := clientset.NetworkingV1().IngressClasses().Create(ctx, ingressClass, metav1.CreateOptions{})
	if createErr != nil {
		return fmt.Errorf("failed to create IngressClass '%s': %v", className, createErr)
	}

	klog.Infof("Successfully created IngressClass '%s'", className)
	return nil
}
