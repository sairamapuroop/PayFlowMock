package logger

import (
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Setup configures the package-global zerolog default logger (JSON to stdout).
func Setup(serviceName string) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", serviceName).
		Logger()
}
