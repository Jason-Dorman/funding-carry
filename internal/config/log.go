package config

import (
	"errors"
	"io"
	"log/slog"
	"strings"
)

var (
	errUnknownLogFormat = errors.New("expected json or text")
	errUnknownLogLevel  = errors.New("expected debug, info, warn or error")
)

// Log output formats. Containers log JSON so Compose/Loki can parse it; a human
// running a binary directly gets text.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// LogConfig is the logging half of every binary's configuration.
type LogConfig struct {
	Level  slog.Level
	Format string
}

// NewLogger builds the process logger. Writer is a parameter so tests can capture
// output instead of writing to stdout.
func NewLogger(cfg LogConfig, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Level}
	if cfg.Format == FormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func (l *loader) logConfig() LogConfig {
	format := strings.ToLower(l.String("LOG_FORMAT", FormatJSON))
	if format != FormatJSON && format != FormatText {
		l.reject("LOG_FORMAT", format, errUnknownLogFormat)
	}

	raw := l.String("LOG_LEVEL", "info")
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		l.reject("LOG_LEVEL", raw, errUnknownLogLevel)
	}

	return LogConfig{Level: level, Format: format}
}
