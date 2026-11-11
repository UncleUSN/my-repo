// Package store хранит историю передач в одном JSON-файле.
// Для MVP этого достаточно и не тянет CGO/SQLite, поэтому бинарник остаётся статическим.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/UncleUSN/my-repo/internal/fsutil"
)

const defaultLimit = 500

// Transfer — одна передача или попытка передачи.
type Transfer struct {
	ID         string    `json:"id"`
	Job        string    `json:"job,omitempty"`
	Direction  string    `json:"direction"` // in — файл принят, out — файл отправлен
	Peer       string    `json:"peer,omitempty"`
	LocalPath  string    `json:"local_path,omitempty"`
	RemotePath string    `json:"remote_path,omitempty"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256,omitempty"`
	Status     string    `json:"status"` // running, success, error
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Store — потокобезопасная история с ограничением длины.
type Store struct {
	mu    sync.Mutex
	path  string
	limit int
	items []Transfer
}

// Open читает историю с диска. Если файла ещё нет, история пустая.
func Open(path string) (*Store, error) {
	s := &Store{path: path, limit: defaultLimit}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.items); err != nil {
		return nil, fmt.Errorf("history %s: %w", path, err)
	}
	return s, nil
}

// NewID возвращает случайный идентификатор передачи или сессии загрузки.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Add дописывает запись и сохраняет файл.
func (s *Store) Add(t Transfer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now().UTC()
	}
	s.items = append(s.items, t)
	if len(s.items) > s.limit {
		s.items = s.items[len(s.items)-s.limit:]
	}
	return s.saveLocked()
}

// Update изменяет запись по идентификатору.
func (s *Store) Update(id string, fn func(*Transfer)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			fn(&s.items[i])
			return s.saveLocked()
		}
	}
	return fmt.Errorf("transfer %s not found", id)
}

// Get возвращает копию записи.
func (s *Store) Get(id string) (Transfer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.items) - 1; i >= 0; i-- {
		if s.items[i].ID == id {
			return s.items[i], true
		}
	}
	return Transfer{}, false
}

// List возвращает последние записи, новые первыми.
func (s *Store) List(limit int) []Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.items) {
		limit = len(s.items)
	}
	out := make([]Transfer, 0, limit)
	for i := len(s.items) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.items[i])
	}
	return out
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.path, b, 0o644)
}
