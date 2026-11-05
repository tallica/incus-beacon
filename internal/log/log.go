// Package log builds the zerolog logger from the log flags.
package log

import (
	"fmt"
	"io"
	"os"
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
