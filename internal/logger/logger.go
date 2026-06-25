// Package logger builds the structured slog logger used across the collector,
// writing to both stderr and an optional log file. The level is held in a
// *slog.LevelVar so it can be changed at runtime (config reload).
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// New returns a JSON slog.Logger at the given level ("debug", "info", "warn",
// "error"), the LevelVar controlling it (call SetLevel to change at runtime),
// and an io.Closer for the optional log file (a no-op when none is configured).
func New(level, file string) (*slog.Logger, *slog.LevelVar, io.Closer, error) {
	writers := []io.Writer{os.Stderr}
	var closer io.Closer = noopCloser{}

	if strings.TrimSpace(file) != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return nil, nil, nil, fmt.Errorf("create log dir: %w", err)
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open log file: %w", err)
		}
		writers = append(writers, f)
		closer = f
	}

	lv := new(slog.LevelVar)
	lv.Set(ParseLevel(level))
	h := slog.NewJSONHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: lv})
	return slog.New(h), lv, closer, nil
}

// ParseLevel maps a level string to a slog.Level (defaults to Info).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
