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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockBackend is a simple http.Handler that writes its name
type mockBackend struct {
	name string
}

func (m *mockBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(m.name))
}

func TestProxyServeHTTP(t *testing.T) {
	// Create Proxy and test handlers
	proxy := NewProxy("", "")
	defaultHandler := &mockBackend{name: "default-backend"}
	fooPrefixHandler := &mockBackend{name: "foo-prefix-backend"}
	fooExactHandler := &mockBackend{name: "foo-exact-backend"}
	wildcardHandler := &mockBackend{name: "wildcard-backend"}

	// Create the host router for "foo.example.com"
	fooMux := http.NewServeMux()
	fooMux.Handle("/foo/", fooPrefixHandler)     // Prefix match
	fooMux.Handle("/foo-exact", fooExactHandler) // Exact match

	wildcardMux := http.NewServeMux()
	wildcardMux.Handle("/", wildcardHandler)

	// create and apply the configuration
	config := &ProxyConfiguration{
		hostRules: map[string]*http.ServeMux{
			"foo.example.com": fooMux,
			"*.example.com":   wildcardMux,
		},
		tlsCerts:       make(map[string]*tls.Certificate),
		defaultBackend: defaultHandler,
	}
	proxy.UpdateConfig(config)

	testCases := []struct {
		name         string
		host         string
		path         string
		expectedCode int
		expectedBody string
	}{
		{
			name:         "Prefix match",
			host:         "foo.example.com",
			path:         "/foo/bar/baz",
			expectedCode: http.StatusOK,
			expectedBody: "foo-prefix-backend",
		},
		{
			name:         "Prefix match root",
			host:         "foo.example.com",
			path:         "/foo/",
			expectedCode: http.StatusOK,
			expectedBody: "foo-prefix-backend",
		},
		{
			name:         "Exact match",
			host:         "foo.example.com",
			path:         "/foo-exact",
			expectedCode: http.StatusOK,
			expectedBody: "foo-exact-backend",
		},
		{
			name:         "Exact match with subpath (should fail)",
			host:         "foo.example.com",
			path:         "/foo-exact/sub",
			expectedCode: http.StatusNotFound,
			expectedBody: "404 page not found\n",
		},
		{
			name:         "Host mismatch (should use wildcard)",
			host:         "bar.example.com",
			path:         "/foo/",
			expectedCode: http.StatusOK,
			expectedBody: "wildcard-backend",
		},
		{
			name:         "Path mismatch (should use ServeMux 404)",
			host:         "foo.example.com",
			path:         "/bar/",
			expectedCode: http.StatusNotFound,
			expectedBody: "404 page not found\n",
		},
		{
			name:         "Wildcard match",
			host:         "another.example.com",
			path:         "/any/path",
			expectedCode: http.StatusOK,
			expectedBody: "wildcard-backend",
		},
		{
			name:         "Wildcard does not match base domain",
			host:         "example.com",
			path:         "/",
			expectedCode: http.StatusOK,
			expectedBody: "default-backend",
		},
		{
			name:         "Exact match has priority over wildcard",
			host:         "foo.example.com",
			path:         "/foo/",
			expectedCode: http.StatusOK,
			expectedBody: "foo-prefix-backend",
		},
		{
			name:         "Wildcard does not match subdomain of subdomain",
			host:         "sub.wild.example.com",
			path:         "/",
			expectedCode: http.StatusOK,
			expectedBody: "default-backend",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+tc.host+tc.path, nil)
			rr := httptest.NewRecorder()

			proxy.ServeHTTP(rr, req)

			if rr.Code != tc.expectedCode {
				t.Errorf("Expected status code %d, got %d", tc.expectedCode, rr.Code)
			}
			if rr.Body.String() != tc.expectedBody {
				t.Errorf("Expected body %q, got %q", tc.expectedBody, rr.Body.String())
			}
		})
	}
}
func TestProxyServeHTTPNoDefault(t *testing.T) {
	// Test case where no host matches and no default backend is set
	proxy := NewProxy("", "")
	config := &ProxyConfiguration{
		hostRules: make(map[string]*http.ServeMux),
		tlsCerts:  make(map[string]*tls.Certificate),
		// defaultBackend is nil
	}
	proxy.UpdateConfig(config)

	req := httptest.NewRequest("GET", "http://missing.example.com/", nil)
	rr := httptest.NewRecorder()

	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("Expected status code 404, got %d", rr.Code)
	}
}

func TestProxyWithRealListeners(t *testing.T) {
	// Generate test certificates
	host := "secure.example.com"
	cert, err := generateSelfSignedCert(host)
	if err != nil {
		t.Fatalf("Failed to generate test certificate: %v", err)
	}

	// Create proxy manually without using NewProxy constructor
	proxy := &Proxy{
		activeConfig: &ProxyConfiguration{
			hostRules: make(map[string]*http.ServeMux),
			tlsCerts:  make(map[string]*tls.Certificate),
		},
	}

	// Configure the proxy
	mux := http.NewServeMux()
	mux.Handle("/test", &mockBackend{name: "secure-backend"})
	mux.Handle("/", &mockBackend{name: "default-backend"})

	config := &ProxyConfiguration{
		hostRules: map[string]*http.ServeMux{
			host: mux,
		},
		tlsCerts: map[string]*tls.Certificate{
			host: cert,
		},
	}
	proxy.UpdateConfig(config)

	// Create listeners manually to get actual addresses
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create HTTP listener: %v", err)
	}
	defer httpListener.Close()

	httpsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create HTTPS listener: %v", err)
	}
	defer httpsListener.Close()

	// Get actual addresses with allocated ports
	httpAddr := httpListener.Addr().String()
	httpsAddr := httpsListener.Addr().String()

	// Create HTTP server
	proxy.httpServer = &http.Server{
		Addr:    httpAddr,
		Handler: proxy,
	}

	// Create HTTPS server with TLS config
	proxy.httpsServer = &http.Server{
		Addr:    httpsAddr,
		Handler: proxy,
		TLSConfig: &tls.Config{
			GetCertificate: proxy.getCertificate,
		},
	}

	// Start servers with context
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 2)

	// Start HTTP server
	go func() {
		if err := proxy.httpServer.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()

	// Start HTTPS server
	go func() {
		if err := proxy.httpsServer.ServeTLS(httpsListener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()

	// Wait for servers to be ready
	time.Sleep(200 * time.Millisecond)

	// Test HTTP request
	t.Run("HTTP request", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://"+httpAddr+"/test", nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Host = host

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("HTTP request failed: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if string(body) != "secure-backend" {
			t.Errorf("Expected body 'secure-backend', got %q", string(body))
		}

		req, err = http.NewRequest("GET", "http://"+httpAddr+"/random", nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Host = host

		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("HTTP request failed: %v", err)
		}
		defer resp.Body.Close()

		body, _ = io.ReadAll(resp.Body)
		if string(body) != "default-backend" {
			t.Errorf("Expected body 'default-backend', got %q", string(body))
		}
	})

	// Test HTTPS request with certificate verification disabled
	t.Run("HTTPS request", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
					ServerName:         host,
				},
			},
		}

		req, err := http.NewRequest("GET", "https://"+httpsAddr+"/test", nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Host = host

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("HTTPS request failed: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if string(body) != "secure-backend" {
			t.Errorf("Expected body 'secure-backend', got %q", string(body))
		}
	})

	// Test HTTPS with SNI
	t.Run("HTTPS with SNI", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
					ServerName:         host,
				},
			},
		}

		req, _ := http.NewRequest("GET", "https://"+httpsAddr+"/test", nil)
		req.Host = host

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("HTTPS SNI request failed: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if string(body) != "secure-backend" {
			t.Errorf("Expected body 'secure-backend', got %q", string(body))
		}
	})

	// Stop the proxy
	cancel()

	// Shutdown both servers
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()

	if err := proxy.httpServer.Shutdown(shutdownCtx); err != nil {
		t.Errorf("HTTP server shutdown error: %v", err)
	}
	if err := proxy.httpsServer.Shutdown(shutdownCtx); err != nil {
		t.Errorf("HTTPS server shutdown error: %v", err)
	}

	// Check for any errors during server execution
	select {
	case err := <-errChan:
		t.Errorf("Server error: %v", err)
	default:
		// No errors
	}
}

func TestProxyDynamicConfigUpdate(t *testing.T) {
	proxy := NewProxy("", "")

	// Initial config
	mux1 := http.NewServeMux()
	mux1.Handle("/", &mockBackend{name: "backend-v1"})
	config1 := &ProxyConfiguration{
		hostRules: map[string]*http.ServeMux{
			"test.example.com": mux1,
		},
		tlsCerts: make(map[string]*tls.Certificate),
	}
	proxy.UpdateConfig(config1)

	// Test initial config
	req := httptest.NewRequest("GET", "http://test.example.com/", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Body.String() != "backend-v1" {
		t.Errorf("Expected 'backend-v1', got %q", rr.Body.String())
	}

	// Update config
	mux2 := http.NewServeMux()
	mux2.Handle("/", &mockBackend{name: "backend-v2"})
	config2 := &ProxyConfiguration{
		hostRules: map[string]*http.ServeMux{
			"test.example.com": mux2,
		},
		tlsCerts: make(map[string]*tls.Certificate),
	}
	proxy.UpdateConfig(config2)

	// Test updated config
	req = httptest.NewRequest("GET", "http://test.example.com/", nil)
	rr = httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Body.String() != "backend-v2" {
		t.Errorf("Expected 'backend-v2', got %q", rr.Body.String())
	}
}

func TestProxyGetCertificate(t *testing.T) {
	host1 := "host1.example.com"
	host2 := "host2.example.com"
	wildcardHost := "*.wild.com"

	cert1, err := generateSelfSignedCert(host1)
	if err != nil {
		t.Fatalf("Failed to generate cert1: %v", err)
	}

	cert2, err := generateSelfSignedCert(host2)
	if err != nil {
		t.Fatalf("Failed to generate cert2: %v", err)
	}

	wildcardCert, err := generateSelfSignedCert(wildcardHost)
	if err != nil {
		t.Fatalf("Failed to generate wildcardCert: %v", err)
	}

	proxy := NewProxy("", "")
	config := &ProxyConfiguration{
		hostRules: make(map[string]*http.ServeMux),
		tlsCerts: map[string]*tls.Certificate{
			host1:        cert1,
			host2:        cert2,
			wildcardHost: wildcardCert,
		},
	}
	proxy.UpdateConfig(config)

	testCases := []struct {
		name       string
		serverName string
		expectCert *tls.Certificate
		expectErr  bool
	}{
		{
			name:       "Valid host1",
			serverName: host1,
			expectCert: cert1,
			expectErr:  false,
		},
		{
			name:       "Valid host2",
			serverName: host2,
			expectCert: cert2,
			expectErr:  false,
		},
		{
			name:       "Unknown host",
			serverName: "unknown.example.com",
			expectCert: nil,
			expectErr:  true,
		},
		{
			name:       "Valid wildcard host",
			serverName: "foo.wild.com",
			expectCert: wildcardCert,
			expectErr:  false,
		},
		{
			name:       "Exact match has priority over wildcard",
			serverName: host1,
			expectCert: cert1,
			expectErr:  false,
		},
		{
			name:       "Base domain for wildcard should not match",
			serverName: "wild.com",
			expectCert: nil,
			expectErr:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hello := &tls.ClientHelloInfo{
				ServerName: tc.serverName,
			}

			cert, err := proxy.getCertificate(hello)

			if tc.expectErr && err == nil {
				t.Error("Expected error, got nil")
			}
			if !tc.expectErr && err != nil {
				t.Errorf("Expected no error, got %v", err)
			}
			if tc.expectCert != nil && cert != tc.expectCert {
				t.Error("Expected certificate to match")
			}
			if tc.expectCert == nil && cert != nil {
				t.Error("Expected nil certificate")
			}
		})
	}
}

func TestProxyMultipleHosts(t *testing.T) {
	proxy := NewProxy("", "")

	// Create multiple host configurations
	mux1 := http.NewServeMux()
	mux1.Handle("/", &mockBackend{name: "backend1"})

	mux2 := http.NewServeMux()
	mux2.Handle("/api/", &mockBackend{name: "backend2-api"})
	mux2.Handle("/", &mockBackend{name: "backend2-default"})

	config := &ProxyConfiguration{
		hostRules: map[string]*http.ServeMux{
			"host1.example.com": mux1,
			"host2.example.com": mux2,
		},
		tlsCerts:       make(map[string]*tls.Certificate),
		defaultBackend: &mockBackend{name: "default"},
	}
	proxy.UpdateConfig(config)

	testCases := []struct {
		name         string
		host         string
		path         string
		expectedBody string
	}{
		{"Host1 root", "host1.example.com", "/", "backend1"},
		{"Host1 path", "host1.example.com", "/anything", "backend1"},
		{"Host2 API", "host2.example.com", "/api/v1", "backend2-api"},
		{"Host2 root", "host2.example.com", "/", "backend2-default"},
		{"Unknown host", "unknown.example.com", "/", "default"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+tc.host+tc.path, nil)
			rr := httptest.NewRecorder()
			proxy.ServeHTTP(rr, req)

			if rr.Body.String() != tc.expectedBody {
				t.Errorf("Expected body %q, got %q", tc.expectedBody, rr.Body.String())
			}
		})
	}
}

// Helper function to generate self-signed certificates for testing
func generateSelfSignedCert(host string) (*tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	cert := &tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}

	return cert, nil
}
