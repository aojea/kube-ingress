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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testIngressClass = "kube-ingress"
	testNamespace    = "test-ns"
)

func newTestService(name, clusterIP string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: corev1.ServiceSpec{
			ClusterIP: clusterIP,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
				{Name: "web", Port: 8080, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func newTestIngress(name, ingressClass, host, path, svcName string, port int32) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			Rules: []networkingv1.IngressRule{
				{
					Host: host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     path,
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: svcName,
											Port: networkingv1.ServiceBackendPort{
												Number: port,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// Generates a self-signed cert/key pair for testing
func newTestTLSSecret(name, host string) (*corev1.Secret, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
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

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}, nil
}

type testController struct {
	controller *Controller
	proxy      *Proxy
	clientset  *fake.Clientset
	factory    informers.SharedInformerFactory
}

func newTestController(t *testing.T, objects ...runtime.Object) *testController {
	clientset := fake.NewSimpleClientset(objects...)
	factory := informers.NewSharedInformerFactory(clientset, 0)
	proxy := NewProxy("", "") // Addrs don't matter for unit testing logic
	ingressInformer := factory.Networking().V1().Ingresses()
	svcInformer := factory.Core().V1().Services()
	secretInformer := factory.Core().V1().Secrets()
	ingressClassInformer := factory.Networking().V1().IngressClasses()

	controller, err := NewController(clientset, testIngressClass, proxy,
		ingressInformer, svcInformer, secretInformer, ingressClassInformer)
	if err != nil {
		t.Fatalf("Failed to create controller: %v", err)
	}

	for _, obj := range objects {
		switch o := obj.(type) {
		case *networkingv1.Ingress:
			ingressInformer.Informer().GetStore().Add(o)
		case *corev1.Service:
			svcInformer.Informer().GetStore().Add(o)
		case *corev1.Secret:
			secretInformer.Informer().GetStore().Add(o)
		}
	}
	return &testController{controller, proxy, clientset, factory}
}

func TestReconcileProxyConfig(t *testing.T) {
	host := "foo.example.com"
	tlsSecret, err := newTestTLSSecret("foo-tls", host)
	if err != nil {
		t.Fatalf("Failed to create test secret: %v", err)
	}
	wildcardTLSSecret, err := newTestTLSSecret("wild-tls", "*.example.com")
	if err != nil {
		t.Fatalf("Failed to create test secret: %v", err)
	}

	testCases := []struct {
		name              string
		checkHost         string
		objects           []runtime.Object
		expectHostRule    bool
		expectTLSCert     bool
		expectDefault     bool
		expectServiceFail bool
	}{
		{
			name:      "valid ingress, service, and secret",
			checkHost: host,
			objects: []runtime.Object{
				func() *networkingv1.Ingress {
					ing := newTestIngress("foo-ing", testIngressClass, host, "/foo", "foo-svc", 80)
					ing.Spec.TLS = []networkingv1.IngressTLS{{
						Hosts:      []string{host},
						SecretName: tlsSecret.Name,
					}}
					return ing
				}(),
				newTestService("foo-svc", "10.0.0.1"),
				tlsSecret,
			},
			expectHostRule: true,
			expectTLSCert:  true,
		},
		{
			name:      "ingress for other class",
			checkHost: host,
			objects: []runtime.Object{
				newTestIngress("foo-ing", "other-class", host, "/foo", "foo-svc", 80),
				newTestService("foo-svc", "10.0.0.1"),
				tlsSecret,
			},
			expectHostRule: false,
			expectTLSCert:  false,
		},
		{
			name:      "ingress with missing service",
			checkHost: host,
			objects: []runtime.Object{
				newTestIngress("foo-ing", testIngressClass, host, "/foo", "missing-svc", 80),
			},
			expectHostRule: false, // Rule fails to create
		},
		{
			name:      "ingress with missing secret",
			checkHost: host,
			objects: []runtime.Object{
				func() *networkingv1.Ingress {
					ing := newTestIngress("foo-ing", testIngressClass, host, "/foo", "foo-svc", 80)
					ing.Spec.TLS = []networkingv1.IngressTLS{{
						Hosts:      []string{host},
						SecretName: "missing-secret",
					}}
					return ing
				}(),
				newTestService("foo-svc", "10.0.0.1"),
			},
			expectHostRule: true,  // Rule still created
			expectTLSCert:  false, // TLS fails
		},
		{
			name:      "ingress with default backend",
			checkHost: host,
			objects: []runtime.Object{
				func() *networkingv1.Ingress {
					ing := newTestIngress("foo-ing", testIngressClass, host, "/foo", "foo-svc", 80)
					ing.Spec.DefaultBackend = &networkingv1.IngressBackend{
						Service: &networkingv1.IngressServiceBackend{
							Name: "default-svc",
							Port: networkingv1.ServiceBackendPort{Number: 8080},
						},
					}
					return ing
				}(),
				newTestService("foo-svc", "10.0.0.1"),
				newTestService("default-svc", "10.0.0.2"),
			},
			expectHostRule: true,
			expectDefault:  true,
		},
		{
			name:      "ingress with wildcard host",
			checkHost: "*.example.com",
			objects: []runtime.Object{
				func() *networkingv1.Ingress {
					ing := newTestIngress("wild-ing", testIngressClass, "*.example.com", "/", "wild-svc", 80)
					ing.Spec.TLS = []networkingv1.IngressTLS{{
						Hosts:      []string{"*.example.com"},
						SecretName: wildcardTLSSecret.Name,
					}}
					return ing
				}(),
				newTestService("wild-svc", "10.0.0.3"),
				wildcardTLSSecret,
			},
			expectHostRule: true,
			expectTLSCert:  true,
		},
		{
			name:      "ingress with invalid service port name",
			checkHost: host,
			objects: []runtime.Object{
				func() *networkingv1.Ingress {
					ing := newTestIngress("foo-ing", testIngressClass, host, "/foo", "foo-svc", 0) // port number is not used
					ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name = "non-existent-port"
					return ing
				}(),
				newTestService("foo-svc", "10.0.0.1"),
			},
			expectHostRule: false,
		},
		{
			name:      "ingress with exact path type",
			checkHost: host,
			objects: []runtime.Object{
				func() *networkingv1.Ingress {
					ing := newTestIngress("foo-ing", testIngressClass, host, "/foo-exact", "foo-svc", 80)
					pathTypeExact := networkingv1.PathTypeExact
					ing.Spec.Rules[0].HTTP.Paths[0].PathType = &pathTypeExact
					return ing
				}(),
				newTestService("foo-svc", "10.0.0.1"),
			},
			expectHostRule: true,
		},
		{
			name:      "ingress with prefix path",
			checkHost: host,
			objects: []runtime.Object{
				newTestIngress("prefix-ing", testIngressClass, host, "/foo", "foo-svc", 80),
				newTestService("foo-svc", "10.0.0.1"),
			},
			expectHostRule: true,
		},
		{
			name:      "ingress with multiple paths",
			checkHost: host,
			objects: []runtime.Object{
				func() *networkingv1.Ingress {
					ing := newTestIngress("multi-path-ing", testIngressClass, host, "/foo", "foo-svc", 80)
					pathTypeExact := networkingv1.PathTypeExact
					ing.Spec.Rules[0].HTTP.Paths = append(ing.Spec.Rules[0].HTTP.Paths, networkingv1.HTTPIngressPath{
						Path:     "/bar",
						PathType: &pathTypeExact,
						Backend: networkingv1.IngressBackend{
							Service: &networkingv1.IngressServiceBackend{
								Name: "bar-svc",
								Port: networkingv1.ServiceBackendPort{Number: 80},
							},
						},
					})
					return ing
				}(),
				newTestService("foo-svc", "10.0.0.1"),
				newTestService("bar-svc", "10.0.0.4"),
			},
			expectHostRule: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestController(t, tc.objects...)

			err := c.controller.reconcileProxyConfig(context.Background())
			if err != nil {
				t.Fatalf("reconcileProxyConfig() returned error: %v", err)
			}

			// Check proxy config state
			config := c.proxy.activeConfig
			if config == nil {
				t.Fatal("proxy config is nil")
			}

			checkHost := host
			if tc.checkHost != "" {
				checkHost = tc.checkHost
			}

			_, hasHostRule := config.hostRules[checkHost]
			if hasHostRule != tc.expectHostRule {
				t.Errorf("Expected host rule presence for %q %v, got %v", checkHost, tc.expectHostRule, hasHostRule)
			}

			_, hasTLSCert := config.tlsCerts[checkHost]
			if hasTLSCert != tc.expectTLSCert {
				t.Errorf("Expected TLS cert presence for %q %v, got %v", checkHost, tc.expectTLSCert, hasTLSCert)
			}

			hasDefault := config.defaultBackend != nil
			if hasDefault != tc.expectDefault {
				t.Errorf("Expected default backend presence %v, got %v", tc.expectDefault, hasDefault)
			}
		})
	}
}

func TestIngressRouting(t *testing.T) {
	host := "foo.example.com"

	// Create mock backends
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend1"))
	}))
	defer backend1.Close()
	u1, err := url.Parse(backend1.URL)
	if err != nil {
		t.Fatalf("Failed to parse backend URL: %v", err)
	}
	h1, p1, err := net.SplitHostPort(u1.Host)
	if err != nil {
		t.Fatalf("Failed to split host port: %v", err)
	}
	port1, err := strconv.Atoi(p1)
	if err != nil {
		t.Fatalf("Failed to parse port: %v", err)
	}

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend2"))
	}))
	defer backend2.Close()
	u2, err := url.Parse(backend2.URL)
	if err != nil {
		t.Fatalf("Failed to parse backend URL: %v", err)
	}
	h2, p2, err := net.SplitHostPort(u2.Host)
	if err != nil {
		t.Fatalf("Failed to split host port: %v", err)
	}
	port2, err := strconv.Atoi(p2)
	if err != nil {
		t.Fatalf("Failed to parse port: %v", err)
	}

	svc1 := newTestService("svc1", h1)
	svc2 := newTestService("svc2", h2)

	type request struct {
		method       string
		host         string
		path         string
		expectedCode int
		expectedBody string
	}

	testCases := []struct {
		name      string
		objects   []runtime.Object
		requests  []request
	}{
		{
			name: "Prefix /",
			objects: []runtime.Object{
				newTestIngress("prefix-root", testIngressClass, host, "/", "svc1", int32(port1)),
				svc1,
			},
			requests: []request{
				{"GET", host, "/", http.StatusOK, "backend1"},
				{"GET", host, "/foo", http.StatusOK, "backend1"},
			},
		},
		{
			name: "Prefix /foo",
			objects: []runtime.Object{
				newTestIngress("prefix-foo", testIngressClass, host, "/foo", "svc1", int32(port1)),
				svc1,
			},
			requests: []request{
				{"GET", host, "/foo", http.StatusOK, "backend1"},
				{"POST", host, "/foo", http.StatusOK, "backend1"},
				{"GET", host, "/foo/", http.StatusOK, "backend1"},
				{"POST", host, "/foo/", http.StatusOK, "backend1"},
				{"GET", host, "/foo/bar", http.StatusOK, "backend1"},
				{"GET", host, "/foobar", http.StatusNotFound, ""},
			},
		},
		{
			name: "Prefix /foo/",
			objects: []runtime.Object{
				newTestIngress("prefix-foo-slash", testIngressClass, host, "/foo/", "svc1", int32(port1)),
				svc1,
			},
			requests: []request{
				{"GET", host, "/foo", http.StatusOK, "backend1"},
				{"POST", host, "/foo", http.StatusOK, "backend1"},
				{"GET", host, "/foo/", http.StatusOK, "backend1"},
				{"POST", host, "/foo/", http.StatusOK, "backend1"},
				{"GET", host, "/foo/bar", http.StatusOK, "backend1"},
			},
		},
		{
			name: "Prefix /aaa/bbb",
			objects: []runtime.Object{
				newTestIngress("prefix-aaa-bbb", testIngressClass, host, "/aaa/bbb", "svc1", int32(port1)),
				svc1,
			},
			requests: []request{
				{"GET", host, "/aaa/bbb", http.StatusOK, "backend1"},
				{"GET", host, "/aaa/bbb/", http.StatusOK, "backend1"},
				{"GET", host, "/aaa/bbb/ccc", http.StatusOK, "backend1"},
				{"GET", host, "/aaa/bb", http.StatusNotFound, ""},
				{"GET", host, "/aaa/bbbxyz", http.StatusNotFound, ""},
			},
		},
		{
			name: "Prefix /, /aaa",
			objects: []runtime.Object{
				newTestIngress("root-ing", testIngressClass, host, "/", "svc1", int32(port1)),
				newTestIngress("aaa-ing", testIngressClass, host, "/aaa", "svc2", int32(port2)),
				svc1,
				svc2,
			},
			requests: []request{
				{"GET", host, "/aaa/ccc", http.StatusOK, "backend2"},
				{"GET", host, "/ccc", http.StatusOK, "backend1"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestController(t, tc.objects...)

			err := c.controller.reconcileProxyConfig(context.Background())
			if err != nil {
				t.Fatalf("reconcileProxyConfig() returned error: %v", err)
			}

			for _, reqTest := range tc.requests {
				t.Run(reqTest.method+" "+reqTest.path, func(t *testing.T) {
					req, err := http.NewRequest(reqTest.method, "http://"+reqTest.host+reqTest.path, nil)
					if err != nil {
						t.Fatalf("Failed to create request: %v", err)
					}
					if reqTest.host != "" {
						req.Host = reqTest.host
					}
					rr := httptest.NewRecorder()
					c.proxy.ServeHTTP(rr, req)

					if rr.Code != reqTest.expectedCode {
						t.Errorf("Request %s %s%s: expected code %d, got %d", reqTest.method, reqTest.host, reqTest.path, reqTest.expectedCode, rr.Code)
					}

					if reqTest.expectedBody != "" {
						body := rr.Body.String()
						if body != reqTest.expectedBody {
							t.Errorf("Request %s %s%s: expected body %q, got %q", reqTest.method, reqTest.host, reqTest.path, reqTest.expectedBody, body)
						}
					}
				})
			}
		})
	}
}