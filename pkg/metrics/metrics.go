package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collectors groups all application metrics.
type Collectors struct {
	HTTPRequestsTotal             *prometheus.CounterVec
	HTTPRequestDurationSeconds    *prometheus.HistogramVec
	PaymentsTotal                 *prometheus.CounterVec
	PaymentLatencySeconds         *prometheus.HistogramVec
	PSPAttemptsTotal              *prometheus.CounterVec
	PSPRetryAttemptsTotal         *prometheus.CounterVec
	PSPCircuitState               *prometheus.GaugeVec
	WebhookDeliveryAttemptsTotal  *prometheus.CounterVec
	WebhookDeliveryLatencySeconds prometheus.Histogram
	OutboxPending                 prometheus.Gauge
	IdempotencyCacheResultsTotal  *prometheus.CounterVec
}

var (
	once          sync.Once
	registry      *prometheus.Registry
	collectorsSet *Collectors
)

func initRegistry() {
	registry = prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)

	collectorsSet = &Collectors{
		HTTPRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "payflow_http_requests_total",
				Help: "Total HTTP requests by route, method, and status code.",
			},
			[]string{"route", "method", "status"},
		),
		HTTPRequestDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "payflow_http_request_duration_seconds",
				Help:    "HTTP request duration in seconds by route and method.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"route", "method"},
		),
		PaymentsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "payflow_payments_total",
				Help: "Total payment transitions by terminal status.",
			},
			[]string{"status"},
		),
		PaymentLatencySeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "payflow_payment_latency_seconds",
				Help:    "PSP payment operation latency in seconds.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"psp"},
		),
		PSPAttemptsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "payflow_psp_attempts_total",
				Help: "Total PSP attempts by provider, operation, and outcome.",
			},
			[]string{"psp", "op", "outcome"},
		),
		PSPRetryAttemptsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "payflow_psp_retry_attempts_total",
				Help: "Retry engine failed attempts by outcome.",
			},
			[]string{"outcome"},
		),
		PSPCircuitState: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "payflow_psp_circuit_state",
				Help: "Current circuit state per PSP (0=closed, 1=half-open, 2=open).",
			},
			[]string{"psp"},
		),
		WebhookDeliveryAttemptsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "payflow_webhook_delivery_attempts_total",
				Help: "Total webhook delivery attempts by outcome.",
			},
			[]string{"outcome"},
		),
		WebhookDeliveryLatencySeconds: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "payflow_webhook_delivery_latency_seconds",
				Help:    "Webhook delivery latency in seconds.",
				Buckets: prometheus.DefBuckets,
			},
		),
		OutboxPending: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "payflow_outbox_pending",
				Help: "Current number of pending outbox events.",
			},
		),
		IdempotencyCacheResultsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "payflow_idempotency_cache_total",
				Help: "Idempotency cache lookup and lease outcomes.",
			},
			[]string{"result"},
		),
	}

	registry.MustRegister(
		collectorsSet.HTTPRequestsTotal,
		collectorsSet.HTTPRequestDurationSeconds,
		collectorsSet.PaymentsTotal,
		collectorsSet.PaymentLatencySeconds,
		collectorsSet.PSPAttemptsTotal,
		collectorsSet.PSPRetryAttemptsTotal,
		collectorsSet.PSPCircuitState,
		collectorsSet.WebhookDeliveryAttemptsTotal,
		collectorsSet.WebhookDeliveryLatencySeconds,
		collectorsSet.OutboxPending,
		collectorsSet.IdempotencyCacheResultsTotal,
	)
}

// Registry returns the singleton Prometheus registry.
func Registry() *prometheus.Registry {
	once.Do(initRegistry)
	return registry
}

// C returns the singleton set of metrics collectors.
func C() *Collectors {
	once.Do(initRegistry)
	return collectorsSet
}

// Handler returns the HTTP handler to expose /metrics.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry(), promhttp.HandlerOpts{})
}
