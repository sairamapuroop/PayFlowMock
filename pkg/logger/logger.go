package logger

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
)

// Setup configures the package-global zerolog default logger (JSON to stdout).
func Setup(serviceName string) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	level := zerolog.InfoLevel
	if rawLevel := strings.TrimSpace(os.Getenv("LOG_LEVEL")); rawLevel != "" {
		parsed, err := zerolog.ParseLevel(strings.ToLower(rawLevel))
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "invalid LOG_LEVEL %q, defaulting to info\n", rawLevel)
		} else {
			level = parsed
		}
	}
	zerolog.SetGlobalLevel(level)

	base := zerolog.New(os.Stdout).Level(level).With().
		Timestamp().
		Str("service", serviceName).
		Logger()
	log.Logger = base
	zerolog.DefaultContextLogger = &base
}

// WithTrace returns a logger enriched with trace/span IDs from the context.
func WithTrace(ctx context.Context) zerolog.Logger {
	l := log.Ctx(ctx)
	if l == nil {
		l = &log.Logger
	}

	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return *l
	}

	enriched := l.With()
	if spanCtx.HasTraceID() {
		enriched = enriched.Str("trace_id", spanCtx.TraceID().String())
	}
	if spanCtx.HasSpanID() {
		enriched = enriched.Str("span_id", spanCtx.SpanID().String())
	}
	return enriched.Logger()
}
