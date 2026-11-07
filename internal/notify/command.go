package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// command runs a program with no shell; the Payload JSON is its stdin.
type command struct {
	argv []string
}

func newCommand(line string) (*command, error) {
	argv := strings.Fields(line)
	if len(argv) == 0 {
		return nil, errors.New("empty")
	}
	return &command{argv: argv}, nil
}

// stderrLimit keeps a chatty program's output out of the log.
const stderrLimit = 512

func (c *command) Notify(ctx context.Context, b Batch) error {
	in, err := json.Marshal(newPayload(b))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...)
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, truncate(msg, stderrLimit))
		}
		return err
	}
	return nil
}
