package server

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/UncleUSN/my-repo/internal/api"
	"github.com/UncleUSN/my-repo/internal/checksum"
	"github.com/UncleUSN/my-repo/internal/fsutil"
	"github.com/UncleUSN/my-repo/internal/guard"
	"github.com/UncleUSN/my-repo/internal/store"
)

// maxChunk — потолок одного PUT. Клиент по умолчанию шлёт 1 МиБ.
const maxChunk = 8 << 20

const sessionTTL = 48 * time.Hour

type statusError struct {
	code   int
	msg    string
	offset *int64
}

func (e *statusError) Error() string { return e.msg }

type sessionMeta struct {
	ID      string    `json:"id"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	Offset  int64     `json:"offset"`
	Created time.Time `json:"created"`
}

type session struct {
	mu       sync.Mutex
	meta     sessionMeta
	f        *os.File
	part     string
	metaPath string
}

// Manager хранит незавершённые загрузки в data_dir/uploads,
// чтобы докачку можно было продолжить после повторного запроса и даже после
// перезапуска агента.
type Manager struct {
	dir   string
	guard *guard.Guard
	mu    sync.Mutex
	byID  map[string]*session
	locks map[string]*sync.Mutex
}

func newManager(dir string, g *guard.Guard) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		dir:   dir,
		guard: g,
		byID:  map[string]*session{},
		locks: map[string]*sync.Mutex{},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Create(req api.CreateUploadRequest) (api.UploadStatus, error) {
	sha := strings.ToLower(strings.TrimSpace(req.SHA256))
	if !checksum.ValidSHA256(sha) {
		return api.UploadStatus{}, &statusError{code: 400, msg: "sha256 must be 64 hex characters"}
	}
	if req.Size < 0 {
		return api.UploadStatus{}, &statusError{code: 400, msg: "size must be >= 0"}
	}
	resolved, err := m.resolve(req.Path)
	if err != nil {
		return api.UploadStatus{}, err
	}
	var status api.UploadStatus
	err = m.withPath(resolved, func() error {
		if sum, size, ok, err := hashExisting(resolved); err != nil {
			return err
		} else if ok && checksum.Equal(sum, sha) && size == req.Size {
			status = api.UploadStatus{Complete: true, Path: resolved, Size: size, SHA256: sum, Offset: size}
			return nil
		} else if ok && !req.Overwrite {
			return &statusError{code: 409, msg: "file exists and overwrite is false"}
		}

		if existing := m.find(resolved, sha, req.Size); existing != nil {
			existing.mu.Lock()
			status = api.UploadStatus{
				ID: existing.meta.ID, Offset: existing.meta.Offset,
				Size: existing.meta.Size, SHA256: existing.meta.SHA256, Path: resolved,
			}
			existing.mu.Unlock()
			m.dropExcept(resolved, existing.meta.ID)
			return nil
		}
		m.dropExcept(resolved, "")

		id := store.NewID()
		part := filepath.Join(m.dir, id+".part")
		metaPath := filepath.Join(m.dir, id+".json")
		f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		s := &session{
			meta: sessionMeta{
				ID: id, Path: resolved, Size: req.Size, SHA256: sha, Created: time.Now().UTC(),
			},
			f: f, part: part, metaPath: metaPath,
		}
		if err := s.saveMeta(); err != nil {
			f.Close()
			_ = os.Remove(part)
			return err
		}
		m.mu.Lock()
		m.byID[id] = s
		m.mu.Unlock()
		status = api.UploadStatus{ID: id, Offset: 0, Size: req.Size, SHA256: sha, Path: resolved}
		return nil
	})
	return status, err
}

func (m *Manager) Write(id string, offset int64, buf []byte) (int64, error) {
	if len(buf) > maxChunk {
		return 0, &statusError{code: 400, msg: "chunk is larger than 8MiB"}
	}
	s, err := m.session(id)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return 0, &statusError{code: 409, msg: "upload is already finished"}
	}
	if offset != s.meta.Offset {
		off := s.meta.Offset
		return s.meta.Offset, &statusError{code: 409, msg: "offset mismatch", offset: &off}
	}
	if s.meta.Offset+int64(len(buf)) > s.meta.Size {
		return s.meta.Offset, &statusError{code: 400, msg: "upload exceeds declared size"}
	}
	if _, err := s.f.Write(buf); err != nil {
		return s.meta.Offset, err
	}
	if err := s.f.Sync(); err != nil {
		return s.meta.Offset, err
	}
	s.meta.Offset += int64(len(buf))
	if err := s.saveMeta(); err != nil {
		return s.meta.Offset, err
	}
	return s.meta.Offset, nil
}

func (m *Manager) Status(id string) (api.UploadStatus, error) {
	s, err := m.session(id)
	if err != nil {
		return api.UploadStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return api.UploadStatus{
		ID: s.meta.ID, Offset: s.meta.Offset, Size: s.meta.Size,
		SHA256: s.meta.SHA256, Path: s.meta.Path,
	}, nil
}

func (m *Manager) Commit(id string) (api.UploadStatus, error) {
	s, err := m.session(id)
	if err != nil {
		return api.UploadStatus{}, err
	}
	// Сначала блокируем путь назначения, потом сессию — тот же порядок, что в Create.
	var status api.UploadStatus
	err = m.withPath(s.meta.Path, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.f == nil {
			return &statusError{code: 409, msg: "upload is already finished"}
		}
		if s.meta.Offset != s.meta.Size {
			return &statusError{code: 400, msg: fmt.Sprintf("upload incomplete: got %d of %d bytes", s.meta.Offset, s.meta.Size)}
		}
		if _, err := s.f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		sum, n, err := checksum.Reader(s.f)
		if err != nil {
			return err
		}
		if n != s.meta.Size || !checksum.Equal(sum, s.meta.SHA256) {
			s.abortLocked()
			m.forget(id)
			return &statusError{code: 400, msg: "checksum mismatch"}
		}
		if err := s.f.Close(); err != nil {
			return err
		}
		s.f = nil
		if err := fsutil.Install(s.part, s.meta.Path); err != nil {
			// Файл на диске ещё цел. Открываем его снова, чтобы повторный commit не потерял сессию.
			reopened, oerr := os.OpenFile(s.part, os.O_RDWR, 0o644)
			if oerr == nil {
				_, _ = reopened.Seek(s.meta.Offset, io.SeekStart)
				s.f = reopened
			}
			return err
		}
		_ = os.Remove(s.part)
		_ = os.Remove(s.metaPath)
		m.forget(id)
		status = api.UploadStatus{
			ID: id, Complete: true, Offset: n, Size: n, SHA256: sum, Path: s.meta.Path,
		}
		return nil
	})
	return status, err
}

func (m *Manager) resolve(p string) (string, error) {
	resolved, err := m.guard.Resolve(p)
	if err != nil {
		code := 400
		if strings.Contains(err.Error(), "outside") || strings.Contains(err.Error(), "escapes") || strings.Contains(err.Error(), "symlink") {
			code = 403
		}
		return "", &statusError{code: code, msg: err.Error()}
	}
	return resolved, nil
}

func hashExisting(path string) (string, int64, bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}
	if st.IsDir() {
		return "", 0, false, &statusError{code: 400, msg: "destination is a directory"}
	}
	sum, size, err := checksum.File(path)
	if err != nil {
		return "", 0, false, err
	}
	return sum, size, true, nil
}

func (m *Manager) session(id string) (*session, error) {
	m.mu.Lock()
	s := m.byID[id]
	m.mu.Unlock()
	if s == nil {
		return nil, &statusError{code: 404, msg: "upload not found"}
	}
	return s, nil
}

func (m *Manager) find(path, sha string, size int64) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.byID {
		if s.meta.Path == path && checksum.Equal(s.meta.SHA256, sha) && s.meta.Size == size {
			return s
		}
	}
	return nil
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.byID, id)
	m.mu.Unlock()
}

func (m *Manager) dropExcept(path, keep string) {
	m.mu.Lock()
	var victims []*session
	for id, s := range m.byID {
		if s.meta.Path == path && id != keep {
			victims = append(victims, s)
			delete(m.byID, id)
		}
	}
	m.mu.Unlock()
	for _, s := range victims {
		s.mu.Lock()
		s.abortLocked()
		s.mu.Unlock()
	}
}

func (m *Manager) withPath(path string, fn func() error) error {
	m.mu.Lock()
	lk := m.locks[path]
	if lk == nil {
		lk = &sync.Mutex{}
		m.locks[path] = lk
	}
	m.mu.Unlock()
	lk.Lock()
	defer lk.Unlock()
	return fn()
}

func (m *Manager) load() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		metaPath := filepath.Join(m.dir, e.Name())
		b, err := os.ReadFile(metaPath)
		if err != nil {
			return err
		}
		var meta sessionMeta
		if err := jsonUnmarshal(b, &meta); err != nil {
			_ = os.Remove(metaPath)
			continue
		}
		part := filepath.Join(m.dir, meta.ID+".part")
		if time.Since(meta.Created) > sessionTTL {
			_ = os.Remove(metaPath)
			_ = os.Remove(part)
			continue
		}
		f, err := os.OpenFile(part, os.O_RDWR, 0o644)
		if err != nil {
			_ = os.Remove(metaPath)
			continue
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			continue
		}
		meta.Offset = st.Size()
		if meta.Offset > meta.Size {
			f.Close()
			_ = os.Remove(part)
			_ = os.Remove(metaPath)
			continue
		}
		if _, err := f.Seek(meta.Offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		s := &session{meta: meta, f: f, part: part, metaPath: metaPath}
		_ = s.saveMeta()
		m.byID[meta.ID] = s
	}
	return nil
}

func (s *session) saveMeta() error {
	return fsutil.WriteAtomic(s.metaPath, mustJSON(s.meta), 0o644)
}

func (s *session) abortLocked() {
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	_ = os.Remove(s.part)
	_ = os.Remove(s.metaPath)
}
