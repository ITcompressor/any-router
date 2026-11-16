// Package logging — простой leveled-логгер any-router в stderr.
// Формат: 2026-10-13T15:30:00Z INFO  сообщение
// Уровни: DEBUG, INFO, WARN, ERROR. Уровень — из конфига (ANY_ROUTER_LOG_LEVEL).
package logging

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Level — уровень логирования.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	}
	return "?"
}

// ParseLevel разбирает строку уровня; неизвестное значение — INFO.
func ParseLevel(s string) Level {
	switch s {
	case "debug", "DEBUG":
		return LevelDebug
	case "warn", "WARN":
		return LevelWarn
	case "error", "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

// Logger пишет уровневые сообщения в writer (по умолчанию stderr).
type Logger struct {
	mu  sync.Mutex
	w   io.Writer
	lvl Level
}

// New создаёт логгер по строке уровня (см. ParseLevel).
func New(level string) *Logger {
	return &Logger{w: os.Stderr, lvl: ParseLevel(level)}
}

// NewWriter — логгер с указанным writer (для тестов).
func NewWriter(w io.Writer, level string) *Logger {
	return &Logger{w: w, lvl: ParseLevel(level)}
}

func (l *Logger) logf(lvl Level, format string, args ...any) {
	if lvl < l.lvl {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%s %-5s %s\n", now, lvl, fmt.Sprintf(format, args...))
}

// Debugf пишет сообщение уровня DEBUG.
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }

// Infof пишет сообщение уровня INFO.
func (l *Logger) Infof(format string, args ...any) { l.logf(LevelInfo, format, args...) }

// Warnf пишет сообщение уровня WARN.
func (l *Logger) Warnf(format string, args ...any) { l.logf(LevelWarn, format, args...) }

// Errorf пишет сообщение уровня ERROR.
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }
