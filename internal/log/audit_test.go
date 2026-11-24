package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestAuditFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	a := AuditFor(zerolog.New(&buf), "host/1", "", "web")
	AuditBegin(a, "install").Send()
	AuditEnd(a, "install", nil).Send()
	AuditEnd(AuditFor(zerolog.New(&buf), "host/1", "apps", "web"), "upgrade", errors.New("boom")).Send()
	want := []map[string]any{
		{"by": "host/1", "instance": "web", "event": "install", "phase": "begin"},
		{"by": "host/1", "instance": "web", "event": "install", "phase": "done"},
		{"by": "host/1", "project": "apps", "instance": "web", "event": "upgrade", "phase": "failed", "error": "boom"},
	}
	var got []map[string]any
	for line := range strings.Lines(buf.String()) {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("record %q: %v", line, err)
		}
		got = append(got, r)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records %v, want %v", got, want)
	}
}

func TestProcess(t *testing.T) {
	t.Parallel()
	host, pid, ok := strings.Cut(Process(), "/")
	if !ok || host == "" || pid != strconv.Itoa(os.Getpid()) {
		t.Errorf("Process() = %q, want host/%d", Process(), os.Getpid())
	}
}
