// Package config читает YAML-конфиг агента.
// Относительные пути считаются от каталога файла конфига, а не от текущего
// каталога процесса: служба Windows стартует из System32, и «./data» иначе
// уехала бы не туда.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/UncleUSN/my-repo/internal/paths"
)

// Duration читает в YAML значения вроде «2s» и «5m».
type Duration struct {
	time.Duration
}

// D собирает Duration из кода (тесты, значения по умолчанию).
func D(d time.Duration) Duration { return Duration{Duration: d} }

// UnmarshalYAML принимает строку time.ParseDuration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// Config — файл узла. Сценарии могут лежать отдельно (node.scenarios).
type Config struct {
	Node  Node   `yaml:"node"`
	Peers []Peer `yaml:"peers"`
	Jobs  []Job  `yaml:"jobs"`
	// Scenarios — загруженные сценарии (из этого файла и/или внешнего).
	// В самом YAML узла их тоже можно описать, но для новой схемы лучше отдельный файл.
	Scenarios []Scenario `yaml:"scenarios"`

	// Path — абсолютный путь файла, из которого прочитали конфиг.
	Path string `yaml:"-"`
	// ScenariosFile — откуда прочитаны внешние сценарии.
	ScenariosFile string `yaml:"-"`

	// fileDir — каталог YAML-файла. Не из файла.
	fileDir string
}

// Node — этот агент.
type Node struct {
	ID           string   `yaml:"id"`
	Listen       string   `yaml:"listen"`
	Token        string   `yaml:"token"`
	DataDir      string   `yaml:"data_dir"`
	AllowedRoots []string `yaml:"allowed_roots"`
	LogLevel     string   `yaml:"log_level"`
	TLS          TLS      `yaml:"tls"`
	// Inbound — именованные папки, в которые другие узлы кладут файлы по alias.
	Inbound []Inbound `yaml:"inbound"`
	// Scenarios — файл или каталог scenarios.d. Пусто — только jobs/scenarios из этого файла.
	Scenarios string `yaml:"scenarios"`
}

// TLS — файлы сертификата сервера.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Peer — другой узел. Токен можно задать строкой, переменной ${NAME} или файлом token_file.
type Peer struct {
	ID            string `yaml:"id"`
	URL           string `yaml:"url"`
	Token         string `yaml:"token"`
	TokenFile     string `yaml:"token_file"`
	CAFile        string `yaml:"ca_file"`
	TLSSkipVerify bool   `yaml:"tls_skip_verify"`
}

// Job — расписание, наблюдение за каталогом или ручной запуск.
type Job struct {
	Name        string      `yaml:"name"`
	Schedule    string      `yaml:"schedule"`
	Watch       string      `yaml:"watch"`
	Pattern     string      `yaml:"pattern"`
	Recursive   bool        `yaml:"recursive"`
	Settle      Duration    `yaml:"settle"`
	Interval    Duration    `yaml:"interval"`
	Source      Source      `yaml:"source"`
	Destination Destination `yaml:"destination"`
	OnSuccess   string      `yaml:"on_success"`
	ArchiveDir  string      `yaml:"archive_dir"`
}

// Source — локальный файл или каталог (для расписания и ручного запуска).
type Source struct {
	Path string `yaml:"path"`
}

// Destination — куда класть файл. Path разбирает узел-получатель, не отправитель.
type Destination struct {
	Node string `yaml:"node"`
	Path string `yaml:"path"`
}

var nodeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// CronParser принимает и обычные 5 полей, и 6 полей с секундами, и @every.
func CronParser() cron.Parser {
	return cron.NewParser(
		cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)
}

// Load читает файл, подставляет переменные окружения и проверяет структуру.
// Существование сертификатов здесь не требуется: команда send должна работать
// из GitLab CI без локального сервера.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.Path = abs
	cfg.fileDir = filepath.Dir(abs)
	cfg.expandEnv()
	if env := strings.TrimSpace(os.Getenv("FERRY_SCENARIOS")); env != "" {
		cfg.Node.Scenarios = env
	}
	cfg.applyDefaults()
	cfg.resolveRelative()
	if err := cfg.readPeerTokens(); err != nil {
		return nil, err
	}
	inline := cfg.Scenarios
	if cfg.Node.Scenarios != "" {
		cfg.Scenarios = nil
		if err := cfg.LoadScenarios(cfg.Node.Scenarios); err != nil {
			return nil, err
		}
	}
	if len(inline) > 0 {
		cfg.Scenarios = append(inline, cfg.Scenarios...)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) expandEnv() {
	c.Node.ID = os.ExpandEnv(c.Node.ID)
	c.Node.Listen = os.ExpandEnv(c.Node.Listen)
	c.Node.Token = os.ExpandEnv(c.Node.Token)
	c.Node.DataDir = os.ExpandEnv(c.Node.DataDir)
	c.Node.LogLevel = os.ExpandEnv(c.Node.LogLevel)
	c.Node.TLS.CertFile = os.ExpandEnv(c.Node.TLS.CertFile)
	c.Node.TLS.KeyFile = os.ExpandEnv(c.Node.TLS.KeyFile)
	c.Node.Scenarios = os.ExpandEnv(c.Node.Scenarios)
	for i, root := range c.Node.AllowedRoots {
		c.Node.AllowedRoots[i] = os.ExpandEnv(root)
	}
	for i := range c.Node.Inbound {
		c.Node.Inbound[i].Name = os.ExpandEnv(c.Node.Inbound[i].Name)
		c.Node.Inbound[i].Path = os.ExpandEnv(c.Node.Inbound[i].Path)
	}
	for i := range c.Peers {
		c.Peers[i].ID = os.ExpandEnv(c.Peers[i].ID)
		c.Peers[i].URL = os.ExpandEnv(c.Peers[i].URL)
		c.Peers[i].Token = os.ExpandEnv(c.Peers[i].Token)
		c.Peers[i].TokenFile = os.ExpandEnv(c.Peers[i].TokenFile)
		c.Peers[i].CAFile = os.ExpandEnv(c.Peers[i].CAFile)
	}
	for i := range c.Jobs {
		j := &c.Jobs[i]
		j.Name = os.ExpandEnv(j.Name)
		j.Schedule = os.ExpandEnv(j.Schedule)
		j.Watch = os.ExpandEnv(j.Watch)
		j.Pattern = os.ExpandEnv(j.Pattern)
		j.Source.Path = os.ExpandEnv(j.Source.Path)
		j.Destination.Node = os.ExpandEnv(j.Destination.Node)
		j.Destination.Path = os.ExpandEnv(j.Destination.Path)
		j.OnSuccess = os.ExpandEnv(j.OnSuccess)
		j.ArchiveDir = os.ExpandEnv(j.ArchiveDir)
	}
}

func (c *Config) applyDefaults() {
	if c.Node.Listen == "" {
		c.Node.Listen = "0.0.0.0:7420"
	}
	if c.Node.DataDir == "" {
		c.Node.DataDir = "data"
	}
	if c.Node.LogLevel == "" {
		c.Node.LogLevel = "info"
	}
	for i := range c.Jobs {
		j := &c.Jobs[i]
		if j.Pattern == "" {
			j.Pattern = "*"
		}
		if j.Settle.Duration == 0 {
			j.Settle.Duration = 2 * time.Second
		}
		if j.Interval.Duration == 0 {
			j.Interval.Duration = 5 * time.Second
		}
		if j.OnSuccess == "" {
			j.OnSuccess = "none"
		}
	}
	for i := range c.Scenarios {
		c.applyScenarioDefaults(&c.Scenarios[i])
	}
}

func (c *Config) resolveRelative() {
	c.Node.DataDir = c.resolve(c.Node.DataDir)
	c.Node.TLS.CertFile = c.resolve(c.Node.TLS.CertFile)
	c.Node.TLS.KeyFile = c.resolve(c.Node.TLS.KeyFile)
	for i, root := range c.Node.AllowedRoots {
		c.Node.AllowedRoots[i] = c.resolve(root)
	}
	c.Node.Scenarios = c.resolve(c.Node.Scenarios)
	for i := range c.Node.Inbound {
		c.Node.Inbound[i].Path = c.resolve(c.Node.Inbound[i].Path)
	}
	for i := range c.Peers {
		c.Peers[i].CAFile = c.resolve(c.Peers[i].CAFile)
		c.Peers[i].TokenFile = c.resolve(c.Peers[i].TokenFile)
	}
	for i := range c.Scenarios {
		resolveScenarioPaths(&c.Scenarios[i], c.fileDir, c)
	}
	for i := range c.Jobs {
		j := &c.Jobs[i]
		j.Watch = c.resolve(j.Watch)
		j.Source.Path = c.resolve(j.Source.Path)
		j.ArchiveDir = c.resolve(j.ArchiveDir)
		// destination.path принадлежит другому узлу, его нельзя приклеивать к локальному каталогу.
		if c.IsLocal(j.Destination.Node) {
			j.Destination.Path = c.resolve(j.Destination.Path)
		}
	}
}

func (c *Config) resolve(p string) string {
	if p == "" || paths.IsAbs(p) || c.fileDir == "" {
		return p
	}
	return filepath.Join(c.fileDir, p)
}

// Validate проверяет структуру, не трогая файловую систему.
func (c *Config) Validate() error {
	if c.Node.ID != "" && !nodeNameRE.MatchString(c.Node.ID) {
		return fmt.Errorf("node.id %q: use letters, digits, '.', '_' or '-'", c.Node.ID)
	}
	if _, _, err := net.SplitHostPort(c.Node.Listen); err != nil {
		return fmt.Errorf("node.listen %q: %w", c.Node.Listen, err)
	}
	seenPeer := map[string]bool{}
	for _, p := range c.Peers {
		if !nodeNameRE.MatchString(p.ID) {
			return fmt.Errorf("peer id %q is invalid", p.ID)
		}
		if seenPeer[p.ID] {
			return fmt.Errorf("duplicate peer id %q", p.ID)
		}
		seenPeer[p.ID] = true
		if p.ID == c.Node.ID {
			return fmt.Errorf("peer id %q matches node.id", p.ID)
		}
		u, err := url.Parse(p.URL)
		if err != nil {
			return fmt.Errorf("peer %s: %w", p.ID, err)
		}
		if u.Scheme != "https" {
			return fmt.Errorf("peer %s: URL must start with https:// (got %q)", p.ID, p.URL)
		}
		if u.Hostname() == "" {
			return fmt.Errorf("peer %q: URL %q has an empty host. If the config uses ${WIN_HOST}, export it or run examples/start-wsl.sh", p.ID, p.URL)
		}
		if strings.TrimSpace(p.Token) == "" && strings.TrimSpace(p.TokenFile) == "" {
			return fmt.Errorf("peer %s: token is empty (set token or token_file)", p.ID)
		}
		if strings.TrimSpace(p.Token) == "" {
			return fmt.Errorf("peer %s: token is empty", p.ID)
		}
		if !p.TLSSkipVerify && strings.TrimSpace(p.CAFile) == "" {
			return fmt.Errorf("peer %s: set ca_file or tls_skip_verify", p.ID)
		}
	}
	seenJob := map[string]bool{}
	for _, j := range c.Jobs {
		if !nodeNameRE.MatchString(j.Name) {
			return fmt.Errorf("job name %q is invalid", j.Name)
		}
		if seenJob[j.Name] {
			return fmt.Errorf("duplicate job name %q", j.Name)
		}
		seenJob[j.Name] = true
		modes := 0
		if j.Watch != "" {
			modes++
		}
		if j.Schedule != "" {
			modes++
		}
		if j.Source.Path != "" && j.Watch == "" && j.Schedule == "" {
			modes++
		}
		if j.Watch != "" && j.Schedule != "" {
			return fmt.Errorf("job %s: set either watch or schedule, not both", j.Name)
		}
		if j.Watch != "" && j.Source.Path != "" {
			return fmt.Errorf("job %s: watch already sets the source directory, remove source.path", j.Name)
		}
		if j.Schedule != "" && j.Source.Path == "" {
			return fmt.Errorf("job %s: schedule requires source.path", j.Name)
		}
		if modes == 0 {
			return fmt.Errorf("job %s: set schedule, watch, or source.path", j.Name)
		}
		if strings.TrimSpace(j.Destination.Path) == "" {
			return fmt.Errorf("job %s: destination.path is empty", j.Name)
		}
		if !c.IsLocal(j.Destination.Node) {
			if _, ok := c.Peer(j.Destination.Node); !ok {
				return fmt.Errorf("job %s: unknown destination node %q", j.Name, j.Destination.Node)
			}
		}
		switch j.OnSuccess {
		case "none", "delete", "move":
		default:
			return fmt.Errorf("job %s: on_success must be none, delete, or move", j.Name)
		}
		if j.OnSuccess == "move" && j.ArchiveDir == "" {
			return fmt.Errorf("job %s: on_success move requires archive_dir", j.Name)
		}
		if j.Schedule != "" {
			if _, err := CronParser().Parse(j.Schedule); err != nil {
				return fmt.Errorf("job %s: schedule %q: %w", j.Name, j.Schedule, err)
			}
		}
		if j.Interval.Duration < 0 || j.Settle.Duration < 0 {
			return fmt.Errorf("job %s: interval and settle must be >= 0", j.Name)
		}
	}
	return c.validateScenarios()
}

// PrepareRuntime проверяет то, без чего нельзя слушать порт на этой машине.
func (c *Config) PrepareRuntime() error {
	c.applyDefaults()
	if c.Node.ID == "" {
		return fmt.Errorf("node.id is required")
	}
	if !nodeNameRE.MatchString(c.Node.ID) {
		return fmt.Errorf("node.id %q is invalid", c.Node.ID)
	}
	if len(strings.TrimSpace(c.Node.Token)) < 8 {
		return fmt.Errorf("node.token must be at least 8 characters")
	}
	if c.Node.TLS.CertFile == "" || c.Node.TLS.KeyFile == "" {
		return fmt.Errorf("node.tls.cert_file and node.tls.key_file are required")
	}
	for _, p := range []string{c.Node.TLS.CertFile, c.Node.TLS.KeyFile} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("tls file %s: %w", p, err)
		}
	}
	var err error
	if c.Node.DataDir, err = localAbs(c.Node.DataDir); err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	if err := os.MkdirAll(c.Node.DataDir, 0o755); err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	for i, root := range c.Node.AllowedRoots {
		abs, err := localAbs(root)
		if err != nil {
			return fmt.Errorf("allowed_roots: %w", err)
		}
		c.Node.AllowedRoots[i] = abs
	}
	for i := range c.Jobs {
		j := &c.Jobs[i]
		if j.Watch != "" {
			if j.Watch, err = localAbs(j.Watch); err != nil {
				return fmt.Errorf("job %s watch: %w", j.Name, err)
			}
		}
		if j.Source.Path != "" {
			if j.Source.Path, err = localAbs(j.Source.Path); err != nil {
				return fmt.Errorf("job %s source: %w", j.Name, err)
			}
		}
		if j.ArchiveDir != "" {
			if j.ArchiveDir, err = localAbs(j.ArchiveDir); err != nil {
				return fmt.Errorf("job %s archive_dir: %w", j.Name, err)
			}
		}
		if c.IsLocal(j.Destination.Node) {
			if j.Destination.Path, err = localAbs(j.Destination.Path); err != nil {
				return fmt.Errorf("job %s destination: %w", j.Name, err)
			}
		}
	}
	for i := range c.Node.Inbound {
		abs, err := localAbs(c.Node.Inbound[i].Path)
		if err != nil {
			return fmt.Errorf("inbound %s: %w", c.Node.Inbound[i].Name, err)
		}
		c.Node.Inbound[i].Path = abs
		if len(c.Node.AllowedRoots) > 0 {
			inside := false
			for _, root := range c.Node.AllowedRoots {
				ok, err := paths.WithinRoot(abs, root)
				if err != nil {
					return err
				}
				if ok {
					inside = true
					break
				}
			}
			if !inside {
				return fmt.Errorf("inbound %s: path %s is outside allowed_roots", c.Node.Inbound[i].Name, abs)
			}
		}
	}
	for i := range c.Scenarios {
		s := &c.Scenarios[i]
		for k := range s.Sources {
			if s.Sources[k].Path, err = localAbs(s.Sources[k].Path); err != nil {
				return fmt.Errorf("scenario %s source: %w", s.Name, err)
			}
		}
		if s.ArchiveDir != "" {
			if s.ArchiveDir, err = localAbs(s.ArchiveDir); err != nil {
				return fmt.Errorf("scenario %s archive_dir: %w", s.Name, err)
			}
		}
		for k := range s.Destinations {
			d := &s.Destinations[k]
			if d.Alias != "" || d.Path == "" || !c.IsLocal(d.Peer) {
				continue
			}
			if d.Path, err = localAbs(d.Path); err != nil {
				return fmt.Errorf("scenario %s destination: %w", s.Name, err)
			}
		}
	}
	return c.Validate()
}

func localAbs(p string) (string, error) {
	native, err := paths.ForLocalOS(p)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(native)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// IsLocal сообщает, что назначение — файловая система этого узла.
func (c *Config) IsLocal(node string) bool {
	return node == "" || node == "local" || node == c.Node.ID
}

// Peer ищет узел по идентификатору.
func (c *Config) Peer(id string) (Peer, bool) {
	for _, p := range c.Peers {
		if p.ID == id {
			return p, true
		}
	}
	return Peer{}, false
}

// Kind возвращает watch, schedule или manual.
func (j Job) Kind() string {
	switch {
	case j.Watch != "":
		return "watch"
	case j.Schedule != "":
		return "schedule"
	default:
		return "manual"
	}
}

// Root — локальный каталог или файл, который задание читает.
func (j Job) Root() string {
	if j.Watch != "" {
		return j.Watch
	}
	return j.Source.Path
}
