package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kardianos/service"

	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/server"
)

// program — адаптер к диспетчеру служб Windows и к systemd.
// Start обязан вернуться быстро: саму работу делает горутина агента.
type program struct {
	configPath string
	version    string
	cancel     context.CancelFunc
	agent      *server.Agent
}

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	cfg, err := config.Load(p.configPath)
	if err != nil {
		cancel()
		return err
	}
	agent, err := server.Start(ctx, cfg, nil, p.version)
	if err != nil {
		cancel()
		return err
	}
	p.agent = agent
	return nil
}

func (p *program) Stop(service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.agent == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- p.agent.Close() }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		return fmt.Errorf("timeout waiting for ferry to stop")
	}
}

func serviceConfig(configPath string) *service.Config {
	abs := configPath
	if a, err := filepath.Abs(configPath); err == nil {
		abs = a
	}
	return &service.Config{
		Name:             "ferry",
		DisplayName:      "Ferry",
		Description:      "Cross-platform file transfer agent",
		Arguments:        []string{"serve", "--config", abs},
		WorkingDirectory: filepath.Dir(abs),
		Option: service.KeyValue{
			"Restart":   "on-failure",
			"StartType": "automatic",
			"OnFailure": "restart",
		},
	}
}

func runService(version, configPath, action string) error {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	prg := &program{configPath: abs, version: version}
	svc, err := service.New(prg, serviceConfig(abs))
	if err != nil {
		if action == "" {
			fmt.Fprintf(os.Stderr, "диспетчер служб недоступен (%v), запускаю в обычном режиме\n", err)
			return runForeground(version, abs)
		}
		return err
	}
	if action != "" {
		return service.Control(svc, action)
	}
	return svc.Run()
}

func runForeground(version, configPath string) error {
	ctx, cancel := signalContext()
	defer cancel()
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	agent, err := server.Start(ctx, cfg, nil, version)
	if err != nil {
		return err
	}
	<-ctx.Done()
	return agent.Close()
}
