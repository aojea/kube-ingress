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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_ingress_http_requests_total",
			Help: "Total number of HTTP requests, partitioned by host, path, and status code.",
		},
		[]string{"code"},
	)
	httpRequestsDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kube_ingress_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, partitioned by host and path.",
			Buckets: prometheus.DefBuckets, // Default buckets: .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10
		},
	)
	configLastReloadTime = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "kube_ingress_config_last_reload_successful_timestamp",
			Help: "Timestamp of the last successful configuration reload.",
		},
	)
	tlsCertificateErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "kube_ingress_tls_certificate_errors_total",
			Help: "Total number of TLS certificate errors (e.g., certificate not found for a host).",
		},
	)
)
