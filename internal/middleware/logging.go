package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/sairamapuroop/PayFlowMock/pkg/logger"
)

const merchantIDHeader = "X-Merchant-ID"

// Logging logs HTTP request/response metadata with route and latency.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		route := routePattern(r)
		method := strings.ToUpper(r.Method)
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}

		logEvt := logger.WithTrace(r.Context()).
			With().
			Str("method", method).
			Str("route", route).
			Int("status", status).
			Str("latency", time.Since(start).String()).
			Int("bytes", ww.BytesWritten()).
			Str("request_id", chimw.GetReqID(r.Context()))

		if merchantID := strings.TrimSpace(r.Header.Get(merchantIDHeader)); merchantID != "" {
			logEvt = logEvt.Str("merchant_id", merchantID)
		}

		l := logEvt.Logger()
		switch {
		case status >= http.StatusInternalServerError:
			l.Error().Msg("http request complete")
		case status >= http.StatusBadRequest:
			l.Warn().Msg("http request complete")
		default:
			l.Info().Msg("http request complete")
		}
	})
}

func routePattern(r *http.Request) string {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return "unknown"
	}
	pattern := strings.TrimSpace(rctx.RoutePattern())
	if pattern == "" {
		return "unknown"
	}
	return pattern
}

func statusLabel(status int) string {
	return strconv.Itoa(status)
}
