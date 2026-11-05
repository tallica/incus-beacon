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

// New colours console output only when w is a terminal.
func New(w io.Writer, level, format string) (zerolog.Logger, error) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil || lvl == zerolog.NoLevel {
		return zerolog.Nop(), fmt.Errorf("log level %q: unknown", level)
	}
	switch format {
	case config.FormatJSON:
	case config.FormatConsole:
		w = zerolog.ConsoleWriter{Out: w, TimeFormat: time.TimeOnly, NoColor: !isTerminal(w)}
	default:
		return zerolog.Nop(), fmt.Errorf("log format %q: unknown", format)
	}
	return zerolog.New(w).Level(lvl).With().Timestamp().Logger(), nil
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
