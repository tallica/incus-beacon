package log

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
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
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
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

func (r *Rotating) open() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return errors.Join(err, f.Close())
	}
	r.f, r.size = f, st.Size()
	return nil
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
