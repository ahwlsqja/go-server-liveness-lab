// Package logger provides structured logging using zerolog.
package logger

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

// New creates a new zerolog.Logger with console output.
// For production, you might want to use JSON output instead.
func New(debug bool) zerolog.Logger {
	// ConsoleWriter for human-readable output
	output := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	}

	level := zerolog.InfoLevel
	if debug {
		level = zerolog.DebugLevel
	}

	return zerolog.New(output).
		Level(level).
		With().
		Timestamp().
		Caller().
		Logger()
}

// NewJSON creates a JSON logger for structured output.
// Use this when you need machine-parseable logs.
func NewJSON(debug bool) zerolog.Logger {
	level := zerolog.InfoLevel
	if debug {
		level = zerolog.DebugLevel
	}

	return zerolog.New(os.Stdout).
		Level(level).
		With().
		Timestamp().
		Logger()
}
