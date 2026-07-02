// Package logx provides structured logging configured from the agent config,
// plus helpers to keep secret tokens out of logs.
package logx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mirai-agent/mirai-agent/internal/config"
)

// Setup builds a slog.Logger from the log config. When path is empty logs go to
// stderr (captured by the service journal); otherwise to the file (appended).
func Setup(cfg config.LogConfig) (*slog.Logger, io.Closer, error) {
	level := parseLevel(cfg.Level)
	var w io.Writer = os.Stderr
	var closer io.Closer

	if strings.TrimSpace(cfg.Path) != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
			return nil, nil, fmt.Errorf("create log dir: %w", err)
		}
		f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", cfg.Path, err)
		}
		w = f
		closer = f
	}

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler), closer, nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// TokenTag returns a short, non-reversible correlation tag for a secret token,
// safe to include in logs. Never log the raw token.
func TokenTag(token string) string {
	if token == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(token))
	return "tok_" + hex.EncodeToString(sum[:])[:8]
}
