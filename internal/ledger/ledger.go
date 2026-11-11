// Package ledger хранит состояние доставки «файл × назначение» в bbolt.
// После перезапуска агент не шлёт уже доставленное ещё раз и продолжает
// незавершённые попытки. Идентичность — SHA-256 содержимого.
package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// Queued — доставка поставлена, байты ещё не уходили.
	Queued = "queued"
	// Started — попытка началась.
	Started = "started"
	// Delivered — назначение подтвердило файл с этим хешем.
	Delivered = "delivered"
	// Retrying — будет повтор после NextRetry.
	Retrying = "retrying"
	// Failed — попытки исчерпаны.
	Failed = "failed"
)

// Record — одна пара «файл, назначение».
type Record struct {
	Scenario  string    `json:"scenario"`
	Source    string    `json:"source"`
	Rel       string    `json:"rel"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	Peer      string    `json:"peer"`
	Alias     string    `json:"alias,omitempty"`
	DestPath  string    `json:"dest_path,omitempty"`
	DestKey   string    `json:"dest_key"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	NextRetry time.Time `json:"next_retry,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Updated   time.Time `json:"updated"`
	Duration  int64     `json:"duration_ms,omitempty"`
}

// Store — встроенная база одного узла.
type Store struct {
	db *bolt.DB
}

var bucket = []byte("deliveries")

// Open открывает или создаёт файл базы.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("delivery store: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close освобождает файл. На Windows без этого каталог данных не удалить.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Key идентифицирует доставку: сценарий, хеш, относительный путь, назначение.
func Key(scenario, sha, rel, destKey string) string {
	return scenario + "\x00" + sha + "\x00" + rel + "\x00" + destKey
}

// DestKey — стабильное имя назначения внутри сценария.
func DestKey(peer, alias, path string) string {
	return peer + "|" + alias + "|" + path
}

// Put записывает состояние.
func (s *Store) Put(rec Record) error {
	if s == nil || s.db == nil {
		return nil
	}
	if rec.Updated.IsZero() {
		rec.Updated = time.Now().UTC()
	}
	if rec.DestKey == "" {
		rec.DestKey = DestKey(rec.Peer, rec.Alias, rec.DestPath)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	key := Key(rec.Scenario, rec.SHA256, rec.Rel, rec.DestKey)
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), raw)
	})
}

// Get возвращает запись, если она есть.
func (s *Store) Get(scenario, sha, rel, destKey string) (Record, bool, error) {
	if s == nil || s.db == nil {
		return Record{}, false, nil
	}
	var rec Record
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get([]byte(Key(scenario, sha, rel, destKey)))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	return rec, found, err
}

// ListFilter ограничивает выборку.
type ListFilter struct {
	Scenario string
	Status   string
	Limit    int
}

// List возвращает записи, новые первыми.
func (s *Store) List(f ListFilter) ([]Record, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var all []Record
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(_, v []byte) error {
			var rec Record
			if err := json.Unmarshal(v, &rec); err != nil {
				return nil
			}
			if f.Scenario != "" && rec.Scenario != f.Scenario {
				return nil
			}
			if f.Status != "" && rec.Status != f.Status {
				return nil
			}
			all = append(all, rec)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	// Новые в начале. Объём — тысячи, не миллионы.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	sortByUpdated(all)
	if f.Limit > 0 && len(all) > f.Limit {
		all = all[:f.Limit]
	}
	return all, nil
}

func sortByUpdated(all []Record) {
	for i := 1; i < len(all); i++ {
		j := i
		for j > 0 && all[j].Updated.After(all[j-1].Updated) {
			all[j], all[j-1] = all[j-1], all[j]
			j--
		}
	}
}

// Due возвращает доставки, которым пора повторить попытку.
func (s *Store) Due(now time.Time) ([]Record, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var out []Record
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(_, v []byte) error {
			var rec Record
			if json.Unmarshal(v, &rec) != nil {
				return nil
			}
			if rec.Status != Retrying {
				return nil
			}
			if rec.NextRetry.After(now) {
				return nil
			}
			out = append(out, rec)
			return nil
		})
	})
	return out, err
}

// Backoff считает паузу перед попыткой attempt (1 — первая неудача).
func Backoff(attempt int, initial, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if initial <= 0 {
		initial = 2 * time.Second
	}
	d := initial
	for i := 1; i < attempt; i++ {
		if d > max/2 {
			return max
		}
		d *= 2
	}
	if max > 0 && d > max {
		return max
	}
	return d
}

// NormalizeStatus приводит пустой статус к понятному значению.
func NormalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case Queued, Started, Delivered, Retrying, Failed:
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return s
	}
}
