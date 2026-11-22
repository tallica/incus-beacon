// Package log builds the zerolog logger from the log flags, and the audit logger.
package log

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/config"
)

// New colours console output per color; auto colours it only when w is a terminal.
func New(w io.Writer, level, format, color string) (zerolog.Logger, error) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil || lvl == zerolog.NoLevel {
		return zerolog.Nop(), fmt.Errorf("log level %q: unknown", level)
	}
	colored, err := useColor(color, isTerminal(w))
	if err != nil {
		return zerolog.Nop(), err
	}
	switch format {
	case config.FormatJSON:
	case config.FormatConsole:
		w = zerolog.ConsoleWriter{Out: w, TimeFormat: time.TimeOnly, NoColor: !colored}
	default:
		return zerolog.Nop(), fmt.Errorf("log format %q: unknown", format)
	}
	return zerolog.New(w).Level(lvl).With().Timestamp().Logger(), nil
}

// NewAudit logs JSON lines at any level to a Rotating file at path; "" gives a no-op logger.
func NewAudit(path string, maxSize int64, keep int, warn zerolog.Logger) (zerolog.Logger, io.Closer) {
	if path == "" {
		return zerolog.Nop(), closerFunc(func() error { return nil })
	}
	r := NewRotating(path, maxSize, keep)
	w := &warnOnce{w: r, warn: warn.With().Str("file", path).Logger()}
	return zerolog.New(w).Hook(utcTime{}), r
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// utcTime stamps records in UTC without touching zerolog's global time settings, which stderr shares.
type utcTime struct{}

func (utcTime) Run(e *zerolog.Event, _ zerolog.Level, _ string) {
	e.Time(zerolog.TimestampFieldName, time.Now().UTC())
}

type warnOnce struct {
	w    io.Writer
	warn zerolog.Logger
	once sync.Once
}

// Write reports success: zerolog prints every write error to stderr, and io.Writer forbids n < len(p) without one.
func (w *warnOnce) Write(p []byte) (int, error) {
	if _, err := w.w.Write(p); err != nil {
		w.once.Do(func() { w.warn.Warn().Err(err).Msg("audit record not written; later failures are not reported") })
	}
	return len(p), nil
}

func useColor(color string, terminal bool) (bool, error) {
	switch color {
	case config.ColorAuto:
		return terminal, nil
	case config.ColorNever:
		return false, nil
	case config.ColorAlways:
		return true, nil
	}
	return false, fmt.Errorf("log color %q: unknown", color)
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
