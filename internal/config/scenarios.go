package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/UncleUSN/my-repo/internal/paths"
)

// Inbound — именованная входящая папка этого узла.
// Отправитель указывает alias, а не сырой путь. Путь всё равно проверяется по allowed_roots.
type Inbound struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Scenario — сценарий обмена: несколько источников и несколько назначений.
type Scenario struct {
	Name          string       `yaml:"name"`
	Schedule      string       `yaml:"schedule"`
	Watch         bool         `yaml:"watch"`
	Sources       []SourceSpec `yaml:"sources"`
	Destinations  []DestSpec   `yaml:"destinations"`
	Settle        Duration     `yaml:"settle"`
	Interval      Duration     `yaml:"interval"`
	OnSuccess     string       `yaml:"on_success"`
	SuccessPolicy string       `yaml:"success_policy"`
	ArchiveDir    string       `yaml:"archive_dir"`
	Retry         RetryPolicy  `yaml:"retry"`
}

// SourceSpec — локальная папка или файл.
type SourceSpec struct {
	Path      string   `yaml:"path"`
	Include   []string `yaml:"include"`
	Exclude   []string `yaml:"exclude"`
	Recursive bool     `yaml:"recursive"`
}

// DestSpec — одно назначение. Peer — имя из реестра peers, local или id этого узла.
// Alias — входящая папка на узле-получателе. Path — каталог или файл; вместе с alias это подкаталог.
type DestSpec struct {
	Peer  string `yaml:"peer"`
	Path  string `yaml:"path"`
	Alias string `yaml:"alias"`
}

// RetryPolicy — независимые повторы каждого назначения.
type RetryPolicy struct {
	MaxAttempts    int      `yaml:"max_attempts"`
	InitialBackoff Duration `yaml:"initial_backoff"`
	MaxBackoff     Duration `yaml:"max_backoff"`
}

type scenarioDocument struct {
	Scenarios []Scenario `yaml:"scenarios"`
}

// Kind описывает, чем сценарий запускается.
func (s Scenario) Kind() string {
	switch {
	case s.Watch && s.Schedule != "":
		return "watch+schedule"
	case s.Watch:
		return "watch"
	case s.Schedule != "":
		return "schedule"
	default:
		return "manual"
	}
}

// Key — стабильный идентификатор назначения в журнале доставки.
func (d DestSpec) Key() string {
	return d.Peer + "|" + d.Alias + "|" + d.Path
}

func (c *Config) applyScenarioDefaults(s *Scenario) {
	if s.Settle.Duration == 0 {
		s.Settle.Duration = 2 * time.Second
	}
	if s.Interval.Duration == 0 {
		s.Interval.Duration = 5 * time.Second
	}
	if s.OnSuccess == "" {
		s.OnSuccess = "none"
	}
	if s.SuccessPolicy == "" {
		s.SuccessPolicy = "all"
	}
	if s.Retry.MaxAttempts == 0 {
		s.Retry.MaxAttempts = 5
	}
	if s.Retry.InitialBackoff.Duration == 0 {
		s.Retry.InitialBackoff.Duration = 2 * time.Second
	}
	if s.Retry.MaxBackoff.Duration == 0 {
		s.Retry.MaxBackoff.Duration = time.Minute
	}
	for i := range s.Sources {
		if len(s.Sources[i].Include) == 0 {
			s.Sources[i].Include = []string{"*"}
		}
	}
}

// JobsToScenarios переводит старый формат jobs в сценарии с одним источником и одним назначением.
func JobsToScenarios(jobs []Job) []Scenario {
	out := make([]Scenario, 0, len(jobs))
	for _, j := range jobs {
		srcPath := j.Source.Path
		watch := false
		if j.Watch != "" {
			srcPath = j.Watch
			watch = true
		}
		pat := j.Pattern
		if pat == "" {
			pat = "*"
		}
		sc := Scenario{
			Name:     j.Name,
			Schedule: j.Schedule,
			Watch:    watch,
			Sources: []SourceSpec{{
				Path:      srcPath,
				Include:   []string{pat},
				Recursive: j.Recursive,
			}},
			Destinations: []DestSpec{{
				Peer: j.Destination.Node,
				Path: j.Destination.Path,
			}},
			Settle:        j.Settle,
			Interval:      j.Interval,
			OnSuccess:     j.OnSuccess,
			SuccessPolicy: "all",
			ArchiveDir:    j.ArchiveDir,
		}
		out = append(out, sc)
	}
	return out
}

// EffectiveScenarios — новые сценарии плюс старые jobs. Имена не должны пересекаться.
func (c *Config) EffectiveScenarios() []Scenario {
	if c == nil {
		return nil
	}
	out := make([]Scenario, 0, len(c.Scenarios)+len(c.Jobs))
	out = append(out, c.Scenarios...)
	out = append(out, JobsToScenarios(c.Jobs)...)
	for i := range out {
		c.applyScenarioDefaults(&out[i])
	}
	return out
}

// LoadScenarios читает файл или каталог scenarios.d и заменяет список сценариев.
// Старые jobs в основном конфиге не трогает.
func (c *Config) LoadScenarios(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	path = c.resolve(path)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("scenarios %s: %w", path, err)
	}
	var loaded []Scenario
	if info.IsDir() {
		loaded, err = loadScenarioDir(path)
	} else {
		loaded, err = readScenarioFile(path)
	}
	if err != nil {
		return err
	}
	base := filepath.Dir(path)
	if info.IsDir() {
		base = path
	}
	for i := range loaded {
		resolveScenarioPaths(&loaded[i], base, c)
		c.applyScenarioDefaults(&loaded[i])
	}
	c.Scenarios = loaded
	c.ScenariosFile = path
	return nil
}

func readScenarioFile(path string) ([]Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenarios: %w", err)
	}
	dec := yamlDecoder(string(b))
	var doc scenarioDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse scenarios %s: %w", path, err)
	}
	return doc.Scenarios, nil
}

func loadScenarioDir(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(strings.ToLower(n), ".yaml") || strings.HasSuffix(strings.ToLower(n), ".yml") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var all []Scenario
	for _, n := range names {
		part, err := readScenarioFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		all = append(all, part...)
	}
	return all, nil
}

func resolveScenarioPaths(s *Scenario, base string, c *Config) {
	s.ArchiveDir = resolveAgainst(base, s.ArchiveDir)
	for i := range s.Sources {
		s.Sources[i].Path = resolveAgainst(base, s.Sources[i].Path)
	}
	for i := range s.Destinations {
		d := &s.Destinations[i]
		d.Path = expandPath(d.Path)
		// Чужой путь и alias не приклеиваем к локальному каталогу.
		if d.Alias != "" || c == nil || !c.IsLocal(d.Peer) {
			continue
		}
		d.Path = resolveAgainst(base, d.Path)
	}
}

func resolveAgainst(base, p string) string {
	p = expandPath(p)
	if p == "" || pathsIsAbs(p) {
		return p
	}
	if base == "" {
		return p
	}
	return filepath.Join(base, p)
}

func pathsIsAbs(p string) bool {
	return paths.IsAbs(p)
}

func yamlDecoder(text string) *yaml.Decoder {
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	return dec
}

func (c *Config) validateScenarios() error {
	seen := map[string]bool{}
	for _, j := range c.Jobs {
		seen[j.Name] = true
	}
	for _, s := range c.Scenarios {
		if !nodeNameRE.MatchString(s.Name) {
			return fmt.Errorf("scenario name %q is invalid", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate scenario name %q", s.Name)
		}
		seen[s.Name] = true
		if len(s.Sources) == 0 {
			return fmt.Errorf("scenario %s: at least one source is required", s.Name)
		}
		if len(s.Destinations) == 0 {
			return fmt.Errorf("scenario %s: at least one destination is required", s.Name)
		}
		if !s.Watch && s.Schedule == "" && len(s.Sources) > 0 {
			// Ручной запуск допустим.
		}
		for i, src := range s.Sources {
			if strings.TrimSpace(src.Path) == "" {
				return fmt.Errorf("scenario %s: source %d path is empty", s.Name, i)
			}
		}
		for i, d := range s.Destinations {
			if strings.TrimSpace(d.Peer) == "" {
				return fmt.Errorf("scenario %s: destination %d peer is empty", s.Name, i)
			}
			if d.Path == "" && d.Alias == "" {
				return fmt.Errorf("scenario %s: destination %d needs path or alias", s.Name, i)
			}
			if !c.IsLocal(d.Peer) {
				if _, ok := c.Peer(d.Peer); !ok {
					return fmt.Errorf("scenario %s: unknown peer %q", s.Name, d.Peer)
				}
			} else if d.Alias != "" && c.Inbound(d.Alias) == nil {
				return fmt.Errorf("scenario %s: local alias %q is not in node.inbound", s.Name, d.Alias)
			}
		}
		switch s.OnSuccess {
		case "none", "keep", "delete", "move":
		default:
			return fmt.Errorf("scenario %s: on_success must be keep, none, delete, or move", s.Name)
		}
		if s.OnSuccess == "move" && s.ArchiveDir == "" {
			return fmt.Errorf("scenario %s: on_success move requires archive_dir", s.Name)
		}
		switch s.SuccessPolicy {
		case "all", "any":
		default:
			return fmt.Errorf("scenario %s: success_policy must be all or any", s.Name)
		}
		if s.Retry.MaxAttempts < 1 {
			return fmt.Errorf("scenario %s: retry.max_attempts must be >= 1", s.Name)
		}
		if s.Schedule != "" {
			if _, err := CronParser().Parse(s.Schedule); err != nil {
				return fmt.Errorf("scenario %s: schedule %q: %w", s.Name, s.Schedule, err)
			}
		}
		if s.Interval.Duration < 0 || s.Settle.Duration < 0 {
			return fmt.Errorf("scenario %s: interval and settle must be >= 0", s.Name)
		}
	}
	seenIn := map[string]bool{}
	for _, in := range c.Node.Inbound {
		if !nodeNameRE.MatchString(in.Name) {
			return fmt.Errorf("inbound name %q is invalid", in.Name)
		}
		if seenIn[in.Name] {
			return fmt.Errorf("duplicate inbound name %q", in.Name)
		}
		seenIn[in.Name] = true
		if strings.TrimSpace(in.Path) == "" {
			return fmt.Errorf("inbound %s: path is empty", in.Name)
		}
	}
	return nil
}

// Inbound ищет входящую папку по имени.
func (c *Config) Inbound(name string) *Inbound {
	for i := range c.Node.Inbound {
		if c.Node.Inbound[i].Name == name {
			return &c.Node.Inbound[i]
		}
	}
	return nil
}

func (c *Config) readPeerTokens() error {
	for i := range c.Peers {
		p := &c.Peers[i]
		if strings.TrimSpace(p.TokenFile) == "" {
			continue
		}
		b, err := os.ReadFile(p.TokenFile)
		if err != nil {
			return fmt.Errorf("peer %s: token_file: %w", p.ID, err)
		}
		tok := cleanToken(string(b))
		if tok == "" {
			return fmt.Errorf("peer %s: token_file %s is empty", p.ID, p.TokenFile)
		}
		p.Token = tok
	}
	return nil
}
