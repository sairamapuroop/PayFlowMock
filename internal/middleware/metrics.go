package middleware

import (
	"net/http"
	"strings"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/sairamapuroop/PayFlowMock/pkg/metrics"
)

// Metrics records request counters and latencies for HTTP handlers.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		method := strings.ToUpper(r.Method)
		route := routePattern(r)
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}

		c := metrics.C()
		c.HTTPRequestsTotal.WithLabelValues(route, method, statusLabel(status)).Inc()
		c.HTTPRequestDurationSeconds.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
	})
}
