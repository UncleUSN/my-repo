// Package server — HTTPS-агент: приём файлов, отдача файлов, задания и история.
package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/events"
	"github.com/UncleUSN/my-repo/internal/guard"
	"github.com/UncleUSN/my-repo/internal/jobs"
	"github.com/UncleUSN/my-repo/internal/monitoring"
	"github.com/UncleUSN/my-repo/internal/store"
)

// Agent — запущенный узел.
type Agent struct {
	URL     string
	srv     *http.Server
	engine  *jobs.Engine
	mon     *monitoring.Monitor
	cancel  context.CancelFunc
	once    sync.Once
	served  chan struct{}
	logFile *os.File
	err     error
}

// Done закрывается, когда HTTP-сервер остановился.
func (a *Agent) Done() <-chan struct{} { return a.served }

// Close останавливает задания и HTTP-сервер. Повторный вызов ничего не делает.
func (a *Agent) Close() error {
	if a == nil {
		return nil
	}
	a.once.Do(func() {
		if a.engine != nil && a.engine.Bus() != nil {
			a.engine.Bus().Publish(events.Event{Type: events.ServiceStopped, Time: time.Now()})
		}
		if a.mon != nil {
			_ = a.mon.Close()
		}
		if a.cancel != nil {
			a.cancel()
		}
		if a.engine != nil {
			a.engine.Stop()
		}
		if a.srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a.err = a.srv.Shutdown(ctx)
		}
		if a.served != nil {
			<-a.served
		}
		if a.logFile != nil {
			_ = a.logFile.Close()
		}
	})
	if a.err != nil && !errors.Is(a.err, http.ErrServerClosed) {
		return a.err
	}
	return nil
}

// Start слушает cfg.Node.Listen по TLS и возвращает управление, когда порт уже принят.
// Отмена ctx останавливает агент.
func Start(parent context.Context, cfg *config.Config, log *slog.Logger, version string) (*Agent, error) {
	if err := cfg.PrepareRuntime(); err != nil {
		_ = monitoring.WriteOne(cfg, events.Event{Type: events.ConfigInvalid, Error: err.Error(), Time: time.Now()})
		return nil, err
	}
	var logFile *os.File
	if log == nil {
		log, logFile = newLogger(cfg)
	}
	warnConfig(log, cfg)

	g, err := guard.New(cfg.Node.AllowedRoots)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.Node.DataDir, "transfers.json"))
	if err != nil {
		return nil, err
	}
	up, err := newManager(filepath.Join(cfg.Node.DataDir, "uploads"), g)
	if err != nil {
		return nil, err
	}
	eng := jobs.New(cfg, st, g, log)
	mon, err := monitoring.Start(cfg, eng.Bus())
	if err != nil {
		return nil, err
	}
	if eng.Bus() != nil {
		eng.Bus().Publish(events.Event{Type: events.ServiceStarted, Time: time.Now()})
	}
	s := &httpServer{
		cfg: cfg, log: log, store: st, guard: g, engine: eng, uploads: up, version: version,
	}

	cert, err := tls.LoadX509KeyPair(cfg.Node.TLS.CertFile, cfg.Node.TLS.KeyFile)
	if err != nil {
		_ = mon.Close()
		eng.Stop()
		return nil, fmt.Errorf("tls: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Node.Listen)
	if err != nil {
		_ = mon.Close()
		eng.Stop()
		return nil, fmt.Errorf("listen %s: %w", cfg.Node.Listen, err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})

	ctx, cancel := context.WithCancel(parent)
	if err := eng.Start(ctx); err != nil {
		cancel()
		_ = ln.Close()
		_ = mon.Close()
		eng.Stop()
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, err
	}

	srv := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	agent := &Agent{
		URL:     "https://" + publicAddr(ln.Addr().String()),
		srv:     srv,
		engine:  eng,
		mon:     mon,
		cancel:  cancel,
		served:  make(chan struct{}),
		logFile: logFile,
	}
	go func() {
		err := srv.Serve(tlsLn)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("сервер остановлен", "err", err)
		}
		close(agent.served)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel()
		}
	}()
	go func() {
		<-ctx.Done()
		_ = agent.Close()
	}()
	go func() {
		// SIGHUP перечитывает сценарии и peers, не останавливая порт.
		// На Windows этот сигнал не приходит — там есть POST /api/v1/scenarios/reload.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				if err := eng.Reload(); err != nil {
					log.Error("перечитать сценарии", "err", err)
				}
			}
		}
	}()
	log.Info("агент запущен", "node", cfg.Node.ID, "listen", cfg.Node.Listen, "version", version)
	return agent, nil
}

func publicAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func newLogger(cfg *config.Config) (*slog.Logger, *os.File) {
	var level slog.Level
	switch strings.ToLower(cfg.Node.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	w := io.Writer(os.Stderr)
	var f *os.File
	if cfg.Node.DataDir != "" {
		lf, err := os.OpenFile(filepath.Join(cfg.Node.DataDir, "ferry.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			f = lf
			w = io.MultiWriter(os.Stderr, lf)
		}
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})), f
}

func warnConfig(log *slog.Logger, cfg *config.Config) {
	if len(cfg.Node.Token) < 16 {
		log.Warn("токен короче 16 символов; для сети задайте длинный случайный секрет")
	}
	if strings.Contains(cfg.Node.Token, "change-me") {
		log.Warn("в конфиге оставлен пример токена, замените его до выхода в сеть")
	}
	if len(cfg.Node.AllowedRoots) == 0 {
		log.Warn("allowed_roots пуст: агент примет любой локальный путь")
	}
	for _, p := range cfg.Peers {
		if p.TLSSkipVerify {
			log.Warn("проверка TLS-сертификата отключена", "peer", p.ID, "url", p.URL)
		}
	}
}

func tokenOK(got, want string) bool {
	if want == "" {
		return false
	}
	gb := []byte(got)
	wb := []byte(want)
	if len(gb) != len(wb) {
		subtle.ConstantTimeCompare(wb, wb)
		return false
	}
	return subtle.ConstantTimeCompare(gb, wb) == 1
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	if t := strings.TrimSpace(r.Header.Get("X-Ferry-Token")); t != "" {
		return t
	}
	return ""
}
