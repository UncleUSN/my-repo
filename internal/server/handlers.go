package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/UncleUSN/my-repo/internal/api"
	"github.com/UncleUSN/my-repo/internal/checksum"
	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/guard"
	"github.com/UncleUSN/my-repo/internal/jobs"
	"github.com/UncleUSN/my-repo/internal/ledger"
	"github.com/UncleUSN/my-repo/internal/paths"
	"github.com/UncleUSN/my-repo/internal/store"
)

// httpServer обслуживает API одного узла.
type httpServer struct {
	cfg     *config.Config
	log     *slog.Logger
	store   *store.Store
	guard   *guard.Guard
	engine  *jobs.Engine
	uploads *Manager
	version string
}

func (s *httpServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/v1/whoami", s.handleWhoAmI)
	protected.HandleFunc("POST /api/v1/uploads", s.handleCreateUpload)
	protected.HandleFunc("GET /api/v1/uploads/{id}", s.handleUploadStatus)
	protected.HandleFunc("PUT /api/v1/uploads/{id}", s.handleUploadWrite)
	protected.HandleFunc("POST /api/v1/uploads/{id}/commit", s.handleUploadCommit)
	protected.HandleFunc("GET /api/v1/fs/list", s.handleList)
	protected.HandleFunc("GET /api/v1/fs/checksum", s.handleChecksum)
	protected.HandleFunc("GET /api/v1/fs/download", s.handleDownload)
	protected.HandleFunc("GET /api/v1/jobs", s.handleJobs)
	protected.HandleFunc("POST /api/v1/jobs/{name}/run", s.handleJobRun)
	protected.HandleFunc("GET /api/v1/deliveries", s.handleDeliveries)
	protected.HandleFunc("POST /api/v1/scenarios/reload", s.handleReload)
	protected.HandleFunc("GET /api/v1/transfers", s.handleTransfers)
	protected.HandleFunc("GET /api/v1/transfers/{id}", s.handleTransfer)
	mux.Handle("/api/", s.auth(protected))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec)
				writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "internal error"})
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

func (s *httpServer) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := bearer(r)
		want := ""
		if s.engine != nil {
			if cfg := s.engine.Config(); cfg != nil {
				want = cfg.Node.Token
			}
		}
		if want == "" && s.cfg != nil {
			want = s.cfg.Node.Token
		}
		if !tokenOK(got, want) {
			reason := "token mismatch"
			if strings.TrimSpace(got) == "" {
				reason = "missing header"
			}
			s.log.Warn("отказ в доступе",
				"reason", reason,
				"remote", r.RemoteAddr,
				"got_fp", config.TokenFingerprint(got),
				"want_fp", config.TokenFingerprint(want),
				"got_len", len(strings.TrimSpace(got)),
				"want_len", len(strings.TrimSpace(want)),
			)
			writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *httpServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Status: "ok", Node: s.cfg.Node.ID, Version: s.version})
}

func (s *httpServer) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.WhoAmI{Node: s.cfg.Node.ID})
}

func (s *httpServer) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	var req api.CreateUploadRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid json"})
		return
	}
	if req.Alias != "" {
		full, err := s.expandAlias(req.Alias, req.Path)
		if err != nil {
			writeHandlerError(w, err)
			return
		}
		req.Path = full
		req.Alias = ""
	}
	st, err := s.uploads.Create(req)
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *httpServer) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.uploads.Status(r.PathValue("id"))
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *httpServer) handleUploadWrite(w http.ResponseWriter, r *http.Request) {
	offset, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Offset")), 10, 64)
	if err != nil || offset < 0 {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "X-Offset header is required"})
		return
	}
	buf, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxChunk+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "read body: " + err.Error()})
		return
	}
	newOffset, err := s.uploads.Write(r.PathValue("id"), offset, buf)
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.UploadStatus{ID: r.PathValue("id"), Offset: newOffset})
}

func (s *httpServer) handleUploadCommit(w http.ResponseWriter, r *http.Request) {
	st, err := s.uploads.Commit(r.PathValue("id"))
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	now := time.Now().UTC()
	s.record(store.Transfer{
		Direction:  "in",
		LocalPath:  st.Path,
		Size:       st.Size,
		SHA256:     st.SHA256,
		Status:     "success",
		StartedAt:  now,
		FinishedAt: now,
	})
	s.log.Info("файл принят", "path", st.Path, "size", st.Size, "sha256", st.SHA256)
	writeJSON(w, http.StatusOK, st)
}

func (s *httpServer) handleList(w http.ResponseWriter, r *http.Request) {
	p, err := s.localPath(r.URL.Query().Get("path"))
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	info, err := os.Stat(p)
	if err != nil {
		writeOSError(w, err)
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "path is not a directory"})
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1" || r.URL.Query().Get("recursive") == "true"
	entries, err := listDir(p, recursive)
	if err != nil {
		var se *statusError
		if errors.As(err, &se) {
			writeHandlerError(w, err)
			return
		}
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ListResponse{Path: p, Entries: entries})
}

func (s *httpServer) handleChecksum(w http.ResponseWriter, r *http.Request) {
	p, err := s.localPath(r.URL.Query().Get("path"))
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	info, err := os.Stat(p)
	if err != nil {
		writeOSError(w, err)
		return
	}
	if info.IsDir() {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "path is a directory"})
		return
	}
	sum, size, err := checksum.File(p)
	if err != nil {
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ChecksumResponse{Path: p, Size: size, SHA256: sum})
}

func (s *httpServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	p, err := s.localPath(r.URL.Query().Get("path"))
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeOSError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeOSError(w, err)
		return
	}
	if info.IsDir() {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "path is a directory"})
		return
	}
	// ServeContent сам разбирает Range, поэтому докачка get не требует отдельного кода.
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	now := time.Now().UTC()
	s.record(store.Transfer{
		Direction:  "out",
		LocalPath:  p,
		Size:       info.Size(),
		Status:     "success",
		StartedAt:  now,
		FinishedAt: now,
	})
}

func (s *httpServer) expandAlias(alias, rel string) (string, error) {
	in := s.engine.Config().Inbound(alias)
	if in == nil {
		return "", &statusError{code: http.StatusNotFound, msg: "unknown alias " + alias}
	}
	if strings.TrimSpace(rel) == "" {
		return "", &statusError{code: http.StatusBadRequest, msg: "path is required with alias"}
	}
	if paths.IsAbs(rel) {
		return "", &statusError{code: http.StatusBadRequest, msg: "absolute path cannot be combined with alias"}
	}
	return paths.Join(in.Path, rel)
}

func (s *httpServer) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "limit must be a positive integer"})
			return
		}
		limit = n
	}
	if limit > 500 {
		limit = 500
	}
	list, err := s.engine.Deliveries(r.URL.Query().Get("scenario"), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: err.Error()})
		return
	}
	if list == nil {
		list = []ledger.Record{}
	}
	writeJSON(w, http.StatusOK, struct {
		Deliveries []ledger.Record `json:"deliveries"`
	}{Deliveries: list})
}

func (s *httpServer) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Reload(); err != nil {
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "reloaded",
		"scenarios": len(s.engine.Config().EffectiveScenarios()),
	})
}

func (s *httpServer) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.JobsResponse{Jobs: s.engine.Jobs()})
}

func (s *httpServer) handleJobRun(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.Run(r.Context(), r.PathValue("name"))
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "job not found"})
		return
	case errors.Is(err, jobs.ErrBusy):
		writeJSON(w, http.StatusConflict, api.ErrorResponse{Error: "job is already running"})
		return
	}
	body := api.RunResponse{
		Name: res.Name, Transferred: res.Transferred, Failed: res.Failed, Errors: res.Errors,
	}
	if err != nil && res.Transferred == 0 && res.Failed == 0 {
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: err.Error()})
		return
	}
	// Даже если часть файлов не ушла, тело ответа — итог, а не короткая ошибка.
	// Код 500 нужен, чтобы curl -f и CI увидели неуспех.
	code := http.StatusOK
	if res.Failed > 0 {
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, body)
}

func (s *httpServer) handleTransfers(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "limit must be a positive integer"})
			return
		}
		limit = n
	}
	if limit > 500 {
		limit = 500
	}
	writeJSON(w, http.StatusOK, api.TransfersResponse{Transfers: s.store.List(limit)})
}

func (s *httpServer) handleTransfer(w http.ResponseWriter, r *http.Request) {
	t, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "transfer not found"})
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *httpServer) localPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", &statusError{code: http.StatusBadRequest, msg: "path is required"}
	}
	resolved, err := s.guard.Resolve(raw)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "outside") || strings.Contains(err.Error(), "escapes") || strings.Contains(err.Error(), "symlink") {
			code = http.StatusForbidden
		}
		return "", &statusError{code: code, msg: err.Error()}
	}
	return resolved, nil
}

func (s *httpServer) record(t store.Transfer) {
	if err := s.store.Add(t); err != nil {
		s.log.Warn("история", "err", err)
	}
}

func listDir(root string, recursive bool) ([]api.Entry, error) {
	const capEntries = 2000
	var out []api.Entry
	add := func(abs string, d fs.DirEntry) error {
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if recursive && d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, api.Entry{
			Name:    d.Name(),
			Path:    abs,
			Size:    info.Size(),
			IsDir:   d.IsDir(),
			ModTime: info.ModTime(),
		})
		if len(out) > capEntries {
			return &statusError{code: http.StatusBadRequest, msg: "directory listing is too large"}
		}
		return nil
	}
	if !recursive {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if err := add(filepath.Join(root, e.Name()), e); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if abs == root {
			return nil
		}
		if d.IsDir() && d.Type()&os.ModeSymlink != 0 {
			return fs.SkipDir
		}
		return add(abs, d)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func writeOSError(w http.ResponseWriter, err error) {
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "not found"})
		return
	}
	if os.IsPermission(err) {
		writeJSON(w, http.StatusForbidden, api.ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: err.Error()})
}

func writeHandlerError(w http.ResponseWriter, err error) {
	var se *statusError
	if errors.As(err, &se) {
		writeJSON(w, se.code, api.ErrorResponse{Error: se.msg, Offset: se.offset})
		return
	}
	writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
