// Package logger builds the structured slog logger used across the collector,
// writing to both stderr and an optional log file.
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
// "error"). When file is non-empty, logs are tee'd to that file (parent dirs
// are created). The returned io.Closer closes the file (a no-op when no file is
// configured); callers should defer its Close.
func New(level, file string) (*slog.Logger, io.Closer, error) {
	writers := []io.Writer{os.Stderr}
	var closer io.Closer = noopCloser{}

	if strings.TrimSpace(file) != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return nil, nil, fmt.Errorf("create log dir: %w", err)
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		writers = append(writers, f)
		closer = f
	}

	h := slog.NewJSONHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level: parseLevel(level),
	})
	return slog.New(h), closer, nil
}

func parseLevel(s string) slog.Level {
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
