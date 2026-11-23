package log

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Rotating appends to path, shifting it to path.1 … path.keep past maxSize (<= 0: never); keep 0 keeps none.
// Processes sharing one file may race on a rotation and merge two files; that is accepted, not locked.
type Rotating struct {
	path    string
	maxSize int64
	keep    int

	mu   sync.Mutex
	f    *os.File
	size int64
}

func NewRotating(path string, maxSize int64, keep int) *Rotating {
	return &Rotating{path: path, maxSize: maxSize, keep: max(keep, 0)}
}

// Write syncs each record, since the records that matter most come just before a crash.
func (r *Rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.follow(); err != nil {
		return 0, err
	}
	if r.maxSize > 0 && r.size > 0 && r.size+int64(len(p)) > r.maxSize {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	if err == nil {
		err = r.f.Sync()
	}
	return n, err
}

func (r *Rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// follow reopens the path once another process has rotated the file away, and takes the
// file's size, not this process's share of it. A path it can't read stays on the open file.
func (r *Rotating) follow() error {
	if r.f != nil {
		st, err := os.Stat(r.path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if own, ferr := r.f.Stat(); err == nil && ferr == nil && os.SameFile(st, own) {
			r.size = st.Size()
			return nil
		}
		_ = r.f.Close() // every record in it is already synced
		r.f = nil
	}
	return r.open()
}

func (r *Rotating) open() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o750); err != nil {
		return err
	}
	created := true
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if errors.Is(err, fs.ErrExist) {
		created = false
		f, err = os.OpenFile(r.path, os.O_APPEND|os.O_WRONLY, 0)
	}
	if err != nil {
		return err
	}
	if created {
		err = chownToDir(f, filepath.Dir(r.path))
	}
	var st fs.FileInfo
	if err == nil {
		st, err = f.Stat()
	}
	if err != nil {
		return errors.Join(err, f.Close())
	}
	r.f, r.size = f, st.Size()
	return nil
}

// chownToDir gives a file root created its directory's owner, so a root process
// beside an unprivileged one (incus exec next to serve) can't lock it out.
func chownToDir(f *os.File, dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return f.Chown(int(sys.Uid), int(sys.Gid))
}

func (r *Rotating) rotate() error {
	err := r.f.Close()
	r.f = nil
	if err != nil {
		return err
	}
	if r.keep == 0 {
		err = os.Remove(r.path)
	} else {
		for i := r.keep - 1; i >= 1 && err == nil; i-- {
			err = r.rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
		}
		if err == nil {
			err = r.rename(r.path, r.path+".1")
		}
	}
	if err != nil {
		return err
	}
	return r.open()
}

func (r *Rotating) rename(from, to string) error {
	if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
