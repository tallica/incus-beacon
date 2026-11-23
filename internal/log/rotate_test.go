package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, r *Rotating, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if _, err := r.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRotatingAppendsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing", "dir", "audit.log")
	r := NewRotating(path, 1<<20, 2)
	write(t, r, "one")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = NewRotating(path, 1<<20, 2)
	write(t, r, "two")
	_ = r.Close()
	if got := read(t, path); got != "one\ntwo\n" {
		t.Errorf("file %q", got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", st.Mode().Perm())
	}
}

func owner(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	return sys.Uid, sys.Gid
}

func TestRotatingRootGivesNewFilesTheDirOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chown(dir, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.log")
	r := NewRotating(path, 6, 2)
	write(t, r, "one", "two")
	_ = r.Close()
	for _, p := range []string{path, path + ".1"} {
		if uid, gid := owner(t, p); uid != 65534 || gid != 65534 {
			t.Errorf("%s owned by %d:%d, want 65534:65534", p, uid, gid)
		}
	}

	if err := os.Chown(path, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	r = NewRotating(path, 1<<20, 2)
	write(t, r, "three")
	_ = r.Close()
	if uid, gid := owner(t, path); uid != 1000 || gid != 1000 {
		t.Errorf("existing file owned by %d:%d, want it left at 1000:1000", uid, gid)
	}
}

func TestRotatingRotatesAndKeeps(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	r := NewRotating(path, 8, 2) // "lineN\n" is 6 bytes, so each line lands in a fresh file
	write(t, r, "line1", "line2", "line3", "line4")
	_ = r.Close()
	for file, want := range map[string]string{path: "line4\n", path + ".1": "line3\n", path + ".2": "line2\n"} {
		if got := read(t, file); got != want {
			t.Errorf("%s = %q, want %q", filepath.Base(file), got, want)
		}
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s.3 kept beyond keep=2: %v", filepath.Base(path), err)
	}
}

func TestRotatingKeepZeroKeepsNoHistory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	r := NewRotating(path, 8, 0)
	write(t, r, "line1", "line2")
	_ = r.Close()
	if got := read(t, path); got != "line2\n" {
		t.Errorf("file %q", got)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rotated file kept with keep=0: %v", err)
	}
}

func TestRotatingOversizedRecord(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	r := NewRotating(path, 4, 1)
	first, second := strings.Repeat("a", 10), strings.Repeat("b", 10)
	write(t, r, first, second) // each over the limit: written whole, one per file
	_ = r.Close()
	if got := read(t, path); got != second+"\n" {
		t.Errorf("file %q", got)
	}
	if got := read(t, path+".1"); got != first+"\n" {
		t.Errorf("rotated %q", got)
	}
}

func TestRotatingNoLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	r := NewRotating(path, 0, 2)
	write(t, r, "line1", "line2")
	_ = r.Close()
	if got := read(t, path); got != "line1\nline2\n" {
		t.Errorf("file %q", got)
	}
}

func TestRotatingFollowsAnotherWritersRotation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	a, b := NewRotating(path, 8, 0), NewRotating(path, 1<<20, 0)
	write(t, a, "a1")
	write(t, b, "b1")
	write(t, a, "a2") // rotates, and keep 0 removes the file b holds
	write(t, b, "b2")
	_, _ = a.Close(), b.Close()
	if got := read(t, path); got != "a2\nb2\n" {
		t.Errorf("file %q, want b's record after a's rotation in it", got)
	}
}

func TestRotatingCountsOtherWritersRecords(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	a, b := NewRotating(path, 11, 1), NewRotating(path, 1<<20, 1)
	write(t, a, "a1")
	write(t, b, "b1", "b2")
	write(t, a, "a2") // 9 bytes in the file, of which a wrote 3
	_, _ = a.Close(), b.Close()
	if got := read(t, path); got != "a2\n" {
		t.Errorf("file %q, want a's record past the cap in a new file", got)
	}
	if got := read(t, path+".1"); got != "a1\nb1\nb2\n" {
		t.Errorf("rotated %q", got)
	}
}

func TestRotatingConcurrentWriters(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	r := NewRotating(path, 64, 100)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				_, _ = r.Write([]byte("0123456789\n"))
			}
		})
	}
	wg.Wait()
	_ = r.Close()
	files, _ := filepath.Glob(path + "*")
	lines := 0
	for _, f := range files {
		for l := range strings.Lines(read(t, f)) {
			if l != "0123456789\n" {
				t.Fatalf("torn line %q in %s", l, f)
			}
			lines++
		}
	}
	if lines != 160 {
		t.Errorf("%d lines, want 160", lines)
	}
}

func TestAuditRecords(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	var warn bytes.Buffer
	audit, c := NewAudit(path, 1<<20, 1, zerolog.New(&warn))
	audit.Log().Str("event", "update.outcome").Msg("")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &rec); err != nil {
		t.Fatal(err)
	}
	ts, _ := rec["time"].(string)
	at, err := time.Parse(time.RFC3339, ts)
	if rec["event"] != "update.outcome" || rec["level"] != nil || err != nil || at.Location() != time.UTC {
		t.Errorf("record %v", rec)
	}
	if warn.Len() != 0 {
		t.Errorf("warned %q", warn.String())
	}
}

func TestAuditWarnsOnceAndNeverFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	audit, c := NewAudit(filepath.Join(dir, "file", "audit.log"), 1<<20, 1, zerolog.New(&warn))
	defer c.Close()
	audit.Log().Msg("first")
	audit.Log().Msg("second")
	if n := strings.Count(warn.String(), "\n"); n != 1 || !strings.Contains(warn.String(), "audit record not written") {
		t.Errorf("warnings %q", warn.String())
	}
}

func TestAuditOff(t *testing.T) {
	t.Parallel()
	audit, c := NewAudit("", 1, 1, zerolog.Nop())
	audit.Log().Msg("dropped")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
