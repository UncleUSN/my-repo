// Ротация своего файла журнала, без CGO. Идея та же, что у lumberjack:
// при превышении размера alerts.log становится alerts.log.1, старые сдвигаются.
package monitoring

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Rotator дописывает в один файл и режет его по размеру и возрасту копий.
type Rotator struct {
	Path       string
	MaxBytes   int64
	MaxBackups int
	MaxAge     time.Duration

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Write безопасен для одновременных вызовов.
func (r *Rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensure(); err != nil {
		return 0, err
	}
	if r.MaxBytes > 0 && r.size > 0 && r.size+int64(len(p)) > r.MaxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close закрывает текущий файл.
func (r *Rotator) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.close()
}

func (r *Rotator) ensure() error {
	if r.f != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
		return err
	}
	r.dropExpired()
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f = f
	r.size = st.Size()
	return nil
}

func (r *Rotator) rotate() error {
	if err := r.close(); err != nil {
		return err
	}
	r.dropExpired()
	keep := r.MaxBackups
	if keep < 1 {
		_ = os.Remove(r.Path)
		r.size = 0
		return r.ensure()
	}
	_ = os.Remove(r.Path + "." + strconv.Itoa(keep))
	for i := keep - 1; i >= 1; i-- {
		src := r.Path + "." + strconv.Itoa(i)
		dst := r.Path + "." + strconv.Itoa(i+1)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		_ = os.Remove(dst)
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	if _, err := os.Stat(r.Path); err == nil {
		_ = os.Remove(r.Path + ".1")
		if err := os.Rename(r.Path, r.Path+".1"); err != nil {
			return err
		}
	}
	r.size = 0
	return r.ensure()
}

func (r *Rotator) dropExpired() {
	if r.MaxAge <= 0 {
		return
	}
	limit := time.Now().Add(-r.MaxAge)
	for i := 1; i <= r.MaxBackups+2; i++ {
		name := r.Path + "." + strconv.Itoa(i)
		st, err := os.Stat(name)
		if err != nil {
			continue
		}
		if st.ModTime().Before(limit) {
			_ = os.Remove(name)
		}
	}
}

func (r *Rotator) close() error {
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
