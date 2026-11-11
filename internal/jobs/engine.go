// Package jobs исполняет задания: по расписанию, по появлению файла и вручную.
// Наблюдение сделано опросом, а не inotify: так одинаково работает Windows,
// Linux и каталог, который ещё дописывается другим процессом (settle).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/UncleUSN/my-repo/internal/api"
	"github.com/UncleUSN/my-repo/internal/checksum"
	"github.com/UncleUSN/my-repo/internal/client"
	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/fsutil"
	"github.com/UncleUSN/my-repo/internal/guard"
	"github.com/UncleUSN/my-repo/internal/store"
)

// ErrNotFound — нет задания с таким именем.
var ErrNotFound = errors.New("job not found")

// ErrBusy — предыдущий запуск этого задания ещё не закончился.
var ErrBusy = errors.New("job is already running")

// RunResult — итог одного запуска.
type RunResult struct {
	Name        string
	Transferred int
	Failed      int
	Errors      []string
}

type jobStatus struct {
	lastRun    time.Time
	lastStatus string
	lastError  string
}

// Engine исполняет задания одного агента.
type Engine struct {
	cfg     *config.Config
	store   *store.Store
	guard   *guard.Guard
	log     *slog.Logger
	dataDir string

	cron   *cron.Cron
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	running map[string]bool
	status  map[string]*jobStatus
	clients map[string]*client.Client
}

// New создаёт движок. Start запускает расписание и наблюдение.
func New(cfg *config.Config, st *store.Store, g *guard.Guard, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		cfg:     cfg,
		store:   st,
		guard:   g,
		log:     log,
		dataDir: cfg.Node.DataDir,
		running: map[string]bool{},
		status:  map[string]*jobStatus{},
		clients: map[string]*client.Client{},
	}
}

// Start поднимает cron и циклы наблюдения. Останов — через Stop или отмену ctx.
func (e *Engine) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	e.ctx = ctx
	e.cancel = cancel
	e.cron = cron.New(cron.WithParser(config.CronParser()))
	for _, job := range e.cfg.Jobs {
		if job.Schedule != "" {
			name := job.Name
			if _, err := e.cron.AddFunc(job.Schedule, func() {
				if e.ctx.Err() != nil {
					return
				}
				if _, err := e.Run(e.ctx, name); err != nil && !errors.Is(err, ErrBusy) && e.ctx.Err() == nil {
					e.log.Error("задание по расписанию", "job", name, "err", err)
				}
			}); err != nil {
				cancel()
				return fmt.Errorf("job %s: %w", job.Name, err)
			}
		}
		if job.Watch != "" {
			e.wg.Add(1)
			go e.watchLoop(job.Name)
		}
	}
	e.cron.Start()
	return nil
}

// Stop останавливает расписание и дожидается циклов наблюдения.
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	if e.cron != nil {
		<-e.cron.Stop().Done()
	}
	e.wg.Wait()
}

// Jobs возвращает снимок состояний для API.
func (e *Engine) Jobs() []api.JobInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]api.JobInfo, 0, len(e.cfg.Jobs))
	for _, job := range e.cfg.Jobs {
		info := api.JobInfo{
			Name:            job.Name,
			Kind:            job.Kind(),
			Schedule:        job.Schedule,
			Watch:           job.Watch,
			Source:          job.Source.Path,
			DestinationNode: job.Destination.Node,
			DestinationPath: job.Destination.Path,
			Running:         e.running[job.Name],
		}
		if st, ok := e.status[job.Name]; ok && !st.lastRun.IsZero() {
			t := st.lastRun
			info.LastRun = &t
			info.LastStatus = st.lastStatus
			info.LastError = st.lastError
		}
		out = append(out, info)
	}
	return out
}

// Run выполняет задание один раз: все подходящие файлы прямо сейчас, без ожидания settle.
func (e *Engine) Run(ctx context.Context, name string) (RunResult, error) {
	job, ok := e.find(name)
	if !ok {
		return RunResult{}, ErrNotFound
	}
	if !e.markRunning(name) {
		return RunResult{Name: name}, ErrBusy
	}
	defer e.clearRunning(name)

	res := RunResult{Name: name}
	root, err := e.guard.Resolve(job.Root())
	if err != nil {
		e.finishStatus(name, "error", err.Error())
		return res, err
	}
	files, isDir, err := collect(root, job.Pattern, job.Recursive, job.ArchiveDir)
	if err != nil {
		e.finishStatus(name, "error", err.Error())
		return res, err
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			res.Errors = append(res.Errors, err.Error())
			res.Failed++
			break
		}
		if err := e.deliver(ctx, job, f, isDir); err != nil {
			res.Errors = append(res.Errors, f.Rel+": "+err.Error())
			res.Failed++
			e.log.Error("передача", "job", name, "file", f.Abs, "err", err)
			continue
		}
		if err := e.post(job, f); err != nil {
			res.Errors = append(res.Errors, f.Rel+" post: "+err.Error())
			res.Failed++
			e.log.Error("действие после передачи", "job", name, "file", f.Abs, "err", err)
			continue
		}
		res.Transferred++
	}
	status := "success"
	errText := ""
	var ret error
	if res.Failed > 0 {
		status = "error"
		errText = joinErr(res.Errors)
		ret = errors.New(errText)
	}
	e.finishStatus(name, status, errText)
	return res, ret
}

func (e *Engine) watchLoop(name string) {
	defer e.wg.Done()
	job, ok := e.find(name)
	if !ok {
		return
	}
	state := e.loadWatch(name)
	ticker := time.NewTicker(job.Interval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.pollWatch(job, state)
		}
	}
}

func (e *Engine) pollWatch(job config.Job, state *watchState) {
	if !e.markRunning(job.Name) {
		return
	}
	defer e.clearRunning(job.Name)

	root, err := e.guard.Resolve(job.Watch)
	if err != nil {
		e.log.Warn("каталог наблюдения", "job", job.Name, "err", err)
		e.finishStatus(job.Name, "error", err.Error())
		return
	}
	files, _, err := collect(root, job.Pattern, job.Recursive, job.ArchiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			e.log.Debug("каталог наблюдения ещё не создан", "job", job.Name, "path", job.Watch)
			return
		}
		e.log.Warn("обход каталога наблюдения", "job", job.Name, "err", err)
		e.finishStatus(job.Name, "error", err.Error())
		return
	}
	seen := map[string]bool{}
	dirty := false
	var failed int
	var sent int
	for _, f := range files {
		seen[f.Rel] = true
		prev := state.Files[f.Rel]
		size := f.Size
		mod := f.Mod.UnixNano()
		if prev.Size != size || prev.Mod != mod || prev.Since.IsZero() {
			state.Files[f.Rel] = watchFile{Size: size, Mod: mod, Since: time.Now()}
			dirty = true
			continue
		}
		if time.Since(prev.Since) < job.Settle.Duration {
			continue
		}
		if prev.Sent && prev.SentSize == size && prev.SentMod == mod {
			continue
		}
		if e.ctx.Err() != nil {
			return
		}
		if err := e.deliver(e.ctx, job, f, true); err != nil {
			failed++
			e.log.Error("передача из каталога наблюдения", "job", job.Name, "file", f.Abs, "err", err)
			continue
		}
		if err := e.post(job, f); err != nil {
			failed++
			e.log.Error("действие после передачи", "job", job.Name, "file", f.Abs, "err", err)
			continue
		}
		sent++
		// Файл уехал из каталога — запись о нём больше не нужна.
		if job.OnSuccess == "delete" || job.OnSuccess == "move" {
			delete(state.Files, f.Rel)
		} else {
			prev.Sent = true
			prev.SentSize = size
			prev.SentMod = mod
			state.Files[f.Rel] = prev
		}
		dirty = true
	}
	for rel := range state.Files {
		if !seen[rel] {
			delete(state.Files, rel)
			dirty = true
		}
	}
	if dirty {
		if err := e.saveWatch(job.Name, state); err != nil {
			e.log.Warn("не удалось сохранить состояние наблюдения", "job", job.Name, "err", err)
		}
	}
	if failed > 0 {
		e.finishStatus(job.Name, "error", fmt.Sprintf("failed %d", failed))
		return
	}
	if sent > 0 {
		e.finishStatus(job.Name, "success", "")
	}
}

func (e *Engine) deliver(ctx context.Context, job config.Job, f File, srcIsDir bool) error {
	remote, err := destinationPath(job, srcIsDir, f.Rel)
	if err != nil {
		return err
	}
	id := store.NewID()
	peer := job.Destination.Node
	if err := e.store.Add(store.Transfer{
		ID:         id,
		Job:        job.Name,
		Direction:  "out",
		Peer:       peer,
		LocalPath:  f.Abs,
		RemotePath: remote,
		Status:     "running",
		StartedAt:  time.Now().UTC(),
	}); err != nil {
		e.log.Warn("история", "err", err)
	}
	var res client.Result
	var derr error
	if e.cfg.IsLocal(peer) {
		res, derr = e.copyLocal(f.Abs, remote)
	} else {
		cl, cerr := e.clientFor(peer)
		if cerr != nil {
			derr = cerr
		} else {
			e.log.Info("отправка", "job", job.Name, "file", f.Abs, "peer", peer, "dest", remote)
			res, derr = cl.UploadFile(ctx, f.Abs, remote)
		}
	}
	_ = e.store.Update(id, func(t *store.Transfer) {
		t.FinishedAt = time.Now().UTC()
		t.Size = res.Size
		t.SHA256 = res.SHA256
		if derr != nil {
			t.Status = "error"
			t.Error = derr.Error()
			return
		}
		t.Status = "success"
	})
	return derr
}

func (e *Engine) copyLocal(src, dest string) (client.Result, error) {
	resolved, err := e.guard.Resolve(dest)
	if err != nil {
		return client.Result{}, err
	}
	sum, size, err := checksum.File(src)
	if err != nil {
		return client.Result{}, err
	}
	if st, err := os.Stat(resolved); err == nil && !st.IsDir() {
		have, _, herr := checksum.File(resolved)
		if herr == nil && checksum.Equal(have, sum) {
			return client.Result{Path: resolved, Size: size, SHA256: sum, AlreadyExists: true}, nil
		}
	}
	if err := fsutil.Install(src, resolved); err != nil {
		return client.Result{}, err
	}
	return client.Result{Path: resolved, Size: size, SHA256: sum}, nil
}

func (e *Engine) post(job config.Job, f File) error {
	switch job.OnSuccess {
	case "", "none":
		return nil
	case "delete":
		if err := os.Remove(f.Abs); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "move":
		if job.ArchiveDir == "" {
			return fmt.Errorf("archive_dir is empty")
		}
		if err := os.MkdirAll(job.ArchiveDir, 0o755); err != nil {
			return err
		}
		dest := uniquePath(filepath.Join(job.ArchiveDir, filepath.Base(f.Abs)))
		return fsutil.Move(f.Abs, dest)
	default:
		return fmt.Errorf("unknown on_success %q", job.OnSuccess)
	}
}

func (e *Engine) clientFor(id string) (*client.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cl, ok := e.clients[id]; ok {
		return cl, nil
	}
	peer, ok := e.cfg.Peer(id)
	if !ok {
		return nil, fmt.Errorf("unknown peer %q", id)
	}
	cl, err := client.New(peer)
	if err != nil {
		return nil, err
	}
	e.clients[id] = cl
	return cl, nil
}

func (e *Engine) find(name string) (config.Job, bool) {
	for _, job := range e.cfg.Jobs {
		if job.Name == name {
			return job, true
		}
	}
	return config.Job{}, false
}

func (e *Engine) markRunning(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running[name] {
		return false
	}
	e.running[name] = true
	return true
}

func (e *Engine) clearRunning(name string) {
	e.mu.Lock()
	e.running[name] = false
	e.mu.Unlock()
}

func (e *Engine) finishStatus(name, status, errText string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.status[name]
	if st == nil {
		st = &jobStatus{}
		e.status[name] = st
	}
	st.lastRun = time.Now().UTC()
	st.lastStatus = status
	st.lastError = errText
}

func joinErr(errs []string) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "; "
		}
		out += e
		if len(out) > 2000 {
			return out + "..."
		}
	}
	return out
}

func uniquePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path
	}
	ext := filepath.Ext(path)
	base := path[:len(path)-len(ext)]
	return base + "-" + time.Now().Format("20060102-150405.000") + ext
}

type watchFile struct {
	Size     int64     `json:"size"`
	Mod      int64     `json:"mod"`
	Since    time.Time `json:"since"`
	Sent     bool      `json:"sent"`
	SentSize int64     `json:"sent_size"`
	SentMod  int64     `json:"sent_mod"`
}

type watchState struct {
	Files map[string]watchFile `json:"files"`
}

func (e *Engine) watchPath(name string) string {
	return filepath.Join(e.dataDir, "watch", name+".json")
}

func (e *Engine) loadWatch(name string) *watchState {
	st := &watchState{Files: map[string]watchFile{}}
	b, err := os.ReadFile(e.watchPath(name))
	if err != nil {
		return st
	}
	if json.Unmarshal(b, st) != nil || st.Files == nil {
		return &watchState{Files: map[string]watchFile{}}
	}
	return st
}

func (e *Engine) saveWatch(name string, st *watchState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(e.watchPath(name), b, 0o644)
}
