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
	"github.com/UncleUSN/my-repo/internal/events"
	"github.com/UncleUSN/my-repo/internal/fsutil"
	"github.com/UncleUSN/my-repo/internal/guard"
	"github.com/UncleUSN/my-repo/internal/ledger"
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

// Engine исполняет сценарии одного агента.
type Engine struct {
	cfg       *config.Config
	store     *store.Store
	guard     *guard.Guard
	log       *slog.Logger
	dataDir   string
	ledger    *ledger.Store
	ledgerErr error
	bus       *events.Bus

	cron         *cron.Cron
	ctx          context.Context
	cancel       context.CancelFunc
	workerCtx    context.Context
	workerCancel context.CancelFunc
	wg           sync.WaitGroup
	reloadMu     sync.Mutex

	// hook перехватывает отправку в тестах. Третий результат true — вызов обработан.
	hook func(ctx context.Context, peer, local, remote, alias string) (client.Result, error, bool)

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
	e := &Engine{
		cfg:     cfg,
		store:   st,
		guard:   g,
		log:     log,
		dataDir: cfg.Node.DataDir,
		bus:     events.New(),
		running: map[string]bool{},
		status:  map[string]*jobStatus{},
		clients: map[string]*client.Client{},
	}
	if cfg.Node.DataDir != "" {
		led, err := ledger.Open(filepath.Join(cfg.Node.DataDir, "deliveries.db"))
		if err != nil {
			e.ledgerErr = err
			return e
		}
		e.ledger = led
	}
	return e
}

// Bus — шина событий доставки. Подписчик метрик подключится сюда.
func (e *Engine) Bus() *events.Bus { return e.bus }

// Config возвращает текущий конфиг (после reload указатель может смениться).
func (e *Engine) Config() *config.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// Start поднимает cron, наблюдение и повторы. Останов — через Stop или отмену ctx.
func (e *Engine) Start(ctx context.Context) error {
	if e.ledgerErr != nil {
		return e.ledgerErr
	}
	ctx, cancel := context.WithCancel(ctx)
	e.ctx = ctx
	e.cancel = cancel
	return e.startWorkers()
}

func (e *Engine) startWorkers() error {
	if e.ctx == nil {
		return fmt.Errorf("engine is not started")
	}
	ctx, cancel := context.WithCancel(e.ctx)
	e.workerCtx = ctx
	e.workerCancel = cancel
	e.cron = cron.New(cron.WithParser(config.CronParser()))
	scenarios := e.snapshotScenarios()
	for _, sc := range scenarios {
		if sc.Schedule != "" {
			name := sc.Name
			if _, err := e.cron.AddFunc(sc.Schedule, func() {
				if e.workerCtx.Err() != nil {
					return
				}
				if _, err := e.Run(e.workerCtx, name); err != nil && !errors.Is(err, ErrBusy) && e.workerCtx.Err() == nil {
					e.log.Error("сценарий по расписанию", "scenario", name, "err", err)
				}
			}); err != nil {
				cancel()
				return fmt.Errorf("scenario %s: %w", sc.Name, err)
			}
		}
		if sc.Watch {
			e.wg.Add(1)
			go e.watchLoop(sc.Name)
		}
	}
	e.wg.Add(1)
	go e.retryLoop()
	e.cron.Start()
	return nil
}

func (e *Engine) stopWorkers() {
	if e.workerCancel != nil {
		e.workerCancel()
		e.workerCancel = nil
	}
	if e.cron != nil {
		<-e.cron.Stop().Done()
		e.cron = nil
	}
	e.wg.Wait()
}

// Stop останавливает сценарии и закрывает журнал доставки.
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.stopWorkers()
	if e.ledger != nil {
		_ = e.ledger.Close()
		e.ledger = nil
	}
}

// Reload перечитывает конфиг с диска и заново ставит сценарии, не трогая слушающий порт.
func (e *Engine) Reload() error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	cfg := e.Config()
	if cfg == nil || cfg.Path == "" {
		return fmt.Errorf("config path is empty, reload is unavailable")
	}
	fresh, err := config.Load(cfg.Path)
	if err != nil {
		return err
	}
	if err := fresh.PrepareRuntime(); err != nil {
		return err
	}
	e.stopWorkers()
	e.mu.Lock()
	e.cfg = fresh
	e.clients = map[string]*client.Client{}
	e.mu.Unlock()
	e.log.Info("сценарии перечитаны", "scenarios", len(fresh.EffectiveScenarios()), "file", fresh.ScenariosFile)
	return e.startWorkers()
}

// Jobs возвращает снимок сценариев для API. Старые jobs уже превращены в сценарии.
func (e *Engine) Jobs() []api.JobInfo {
	scenarios := e.snapshotScenarios()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]api.JobInfo, 0, len(scenarios))
	for _, sc := range scenarios {
		info := api.JobInfo{
			Name:     sc.Name,
			Kind:     sc.Kind(),
			Schedule: sc.Schedule,
			Running:  e.running[sc.Name],
		}
		if sc.Watch {
			info.Watch = "yes"
		}
		if len(sc.Sources) > 0 {
			info.Source = sc.Sources[0].Path
			if sc.Watch {
				info.Watch = sc.Sources[0].Path
			}
		}
		if len(sc.Destinations) > 0 {
			info.DestinationNode = sc.Destinations[0].Peer
			info.DestinationPath = sc.Destinations[0].Path
			if sc.Destinations[0].Alias != "" {
				info.DestinationPath = "alias:" + sc.Destinations[0].Alias
			}
			if len(sc.Destinations) > 1 {
				info.DestinationPath += fmt.Sprintf(" (+%d)", len(sc.Destinations)-1)
			}
		}
		if st, ok := e.status[sc.Name]; ok && !st.lastRun.IsZero() {
			t := st.lastRun
			info.LastRun = &t
			info.LastStatus = st.lastStatus
			info.LastError = st.lastError
		}
		out = append(out, info)
	}
	return out
}

// Deliveries — снимок журнала для API и CLI.
func (e *Engine) Deliveries(scenario, status string, limit int) ([]ledger.Record, error) {
	if e.ledger == nil {
		return nil, nil
	}
	return e.ledger.List(ledger.ListFilter{Scenario: scenario, Status: status, Limit: limit})
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

func (e *Engine) postDelete(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (e *Engine) postMove(archive, path string) error {
	if archive == "" {
		return fmt.Errorf("archive_dir is empty")
	}
	if err := os.MkdirAll(archive, 0o755); err != nil {
		return err
	}
	dest := uniquePath(filepath.Join(archive, filepath.Base(path)))
	return fsutil.Move(path, dest)
}

func (e *Engine) clientFor(id string) (*client.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cl, ok := e.clients[id]; ok {
		return cl, nil
	}
	cfg := e.cfg
	peer, ok := cfg.Peer(id)
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
