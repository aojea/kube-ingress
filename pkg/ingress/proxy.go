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

package ingress

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// ProxyConfiguration holds the routing and TLS state
type ProxyConfiguration struct {
	// hostRules maps a hostname (e.g., "foo.example.com") to its path router
	hostRules map[string]*http.ServeMux

	// tlsCerts maps a hostname to its TLS certificate
	tlsCerts map[string]*tls.Certificate

	// defaultBackend is the proxy to use when no host/path matches
	defaultBackend http.Handler
}

// Proxy is the reverse proxy server
type Proxy struct {
	httpAddr  string
	httpsAddr string

	// mu protects the activeConfig
	mu sync.RWMutex
	// activeConfig holds the configuration being served
	activeConfig *ProxyConfiguration

	httpServer  *http.Server
	httpsServer *http.Server
}

// NewProxy creates a new proxy server
func NewProxy(httpAddr, httpsAddr string) *Proxy {
	p := &Proxy{
		httpAddr:  httpAddr,
		httpsAddr: httpsAddr,
		// Initialize with empty config so it doesn't nil panic
		activeConfig: &ProxyConfiguration{
			hostRules: make(map[string]*http.ServeMux),
			tlsCerts:  make(map[string]*tls.Certificate),
		},
	}

	// The main router (ServeHTTP) is the proxy object itself
	p.httpServer = &http.Server{
		Addr:    httpAddr,
		Handler: p,
	}
	p.httpsServer = &http.Server{
		Addr:    httpsAddr,
		Handler: p,
		// Use GetCertificate to enable dynamic SNI-based cert loading
		TLSConfig: &tls.Config{
			GetCertificate: p.getCertificate,
		},
	}
	return p
}

// Run starts the HTTP and HTTPS listeners
func (p *Proxy) Run(ctx context.Context) {
	klog.Infof("Starting HTTP listener on %s", p.httpAddr)
	go func() {
		if err := p.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Fatalf("Failed to start HTTP server: %v", err)
		}
	}()

	klog.Infof("Starting HTTPS listener on %s", p.httpsAddr)
	go func() {
		if err := p.httpsServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Fatalf("Failed to start HTTPS server: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	klog.Info("Shutting down proxy servers...")

	// Create a shutdown context with a deadline
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Shutdown both servers
	p.httpServer.Shutdown(shutdownCtx)  //nolint:errcheck
	p.httpsServer.Shutdown(shutdownCtx) //nolint:errcheck

	klog.Info("Proxy servers shut down gracefully")
}

// UpdateConfig atomically swaps the active proxy configuration
func (p *Proxy) UpdateConfig(newConfig *ProxyConfiguration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.activeConfig = newConfig
	configLastReloadTime.SetToCurrentTime()
	klog.V(4).Info("Proxy configuration updated")
}

// getCertificate is called by the TLS listener (SNI) to find the
// correct certificate for a given hostname.
func (p *Proxy) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Find the cert for the requested server name
	if cert, ok := p.activeConfig.tlsCerts[hello.ServerName]; ok {
		return cert, nil
	}
	// Fallback: Check for a wildcard cert
	// (e.g., if hello.ServerName is "foo.bar.com", check for "*.bar.com")
	// Try wildcard host match
	labels := strings.SplitN(hello.ServerName, ".", 2)
	if len(labels) == 2 {
		wildcardHost := "*." + labels[1]
		if cert, ok := p.activeConfig.tlsCerts[wildcardHost]; ok {
			return cert, nil
		}
	}
	klog.Warningf("No TLS certificate found for host: %s", hello.ServerName)
	tlsCertificateErrors.Inc()
	return nil, errors.New("no certificate configured for this domain")
}

// ServeHTTP is the main entry point for all incoming requests (HTTP & HTTPS)
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Try exact host match
	if mux, ok := p.activeConfig.hostRules[r.Host]; ok {
		mux.ServeHTTP(w, r)
		return
	}

	// Try wildcard host match
	labels := strings.SplitN(r.Host, ".", 2)
	if len(labels) == 2 {
		wildcardHost := "*." + labels[1]
		if mux, ok := p.activeConfig.hostRules[wildcardHost]; ok {
			mux.ServeHTTP(w, r)
			return
		}
	}

	// Try all hosts
	if mux, ok := p.activeConfig.hostRules["*"]; ok {
		mux.ServeHTTP(w, r)
		return
	}

	// No host match, try the default backend
	if p.activeConfig.defaultBackend != nil {
		p.activeConfig.defaultBackend.ServeHTTP(w, r)
		return
	}

	// No host match and no default backend
	klog.Warningf("No host rule or default backend for host: %s", r.Host)
	http.NotFound(w, r)
}

type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode int
}

func (w *responseWriterWrapper) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (p *Proxy) instrumentHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Create a response writer wrapper to capture the status code
		// We initialize statusCode to 200, as WriteHeader is not always called
		wrapper := &responseWriterWrapper{ResponseWriter: w, statusCode: http.StatusOK}

		// Record start time
		start := time.Now()

		// Call the actual handler
		handler.ServeHTTP(wrapper, r)

		// Record duration
		duration := time.Since(start).Seconds()
		httpRequestsDuration.Observe(duration)

		// Record count with status code
		httpRequestsTotal.WithLabelValues(strconv.Itoa(wrapper.statusCode)).Inc()
	})
}
