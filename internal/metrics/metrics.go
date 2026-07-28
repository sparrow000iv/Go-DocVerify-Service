// Package metrics defines the Prometheus collectors exposed by the service
// on /metrics and scraped by the Prometheus deployment in deploy/k8s.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTPRequests counts every HTTP request by method, route and status.
	HTTPRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docverify_http_requests_total",
			Help: "Total number of HTTP requests processed, by method, route and status code.",
		},
		[]string{"method", "route", "status"},
	)

	// HTTPDuration records request latency so p50/p95/p99 can be graphed.
	HTTPDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "docverify_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by method and route.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "route"},
	)

	// GRPCRequests counts gRPC calls by method and resulting status code.
	GRPCRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docverify_grpc_requests_total",
			Help: "Total number of gRPC requests processed, by method and status code.",
		},
		[]string{"method", "code"},
	)

	// GRPCDuration records gRPC handler latency.
	GRPCDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "docverify_grpc_request_duration_seconds",
			Help:    "gRPC handler latency in seconds, by method.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method"},
	)

	// DocumentsTotal counts verification outcomes, by terminal status.
	DocumentsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docverify_documents_verified_total",
			Help: "Total number of documents that reached a terminal status.",
		},
		[]string{"status"},
	)

	// DocumentsStored is a gauge of documents currently held in the store.
	DocumentsStored = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "docverify_documents_stored",
			Help: "Number of documents currently held in the store.",
		},
	)
)

// ObserveHTTP records one HTTP request's outcome and latency.
func ObserveHTTP(method, route string, status int, started time.Time) {
	HTTPRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	HTTPDuration.WithLabelValues(method, route).Observe(time.Since(started).Seconds())
}

// ObserveGRPC records one gRPC call's outcome and latency.
func ObserveGRPC(method, code string, started time.Time) {
	GRPCRequests.WithLabelValues(method, code).Inc()
	GRPCDuration.WithLabelValues(method).Observe(time.Since(started).Seconds())
}
