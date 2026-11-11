package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadExampleWindows(t *testing.T) {
	cfg, err := Load(filepath.Join(repoRoot(t), "examples", "ferry.windows.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.ID != "win" {
		t.Fatalf("id %s", cfg.Node.ID)
	}
	if cfg.Peers[0].URL != "https://127.0.0.1:7421" {
		t.Fatalf("peer url %s", cfg.Peers[0].URL)
	}
	if len(cfg.Scenarios) != 1 || cfg.Scenarios[0].Name != "watch-to-wsl" {
		t.Fatalf("scenarios %+v", cfg.Scenarios)
	}
	sc := cfg.Scenarios[0]
	if !sc.Watch || sc.Sources[0].Path != `C:\ferry\outbox` {
		t.Fatalf("source rewritten: %+v", sc.Sources)
	}
	if len(sc.Destinations) != 2 || sc.Destinations[0].Alias != "inbox" || sc.Destinations[1].Alias != "archive" {
		t.Fatalf("dest %+v", sc.Destinations)
	}
	if sc.Settle.Duration != 2*time.Second {
		t.Fatalf("settle %s", sc.Settle.Duration)
	}
	if cfg.Inbound("inbox") == nil || cfg.Inbound("archive") == nil {
		t.Fatal("inbound aliases missing")
	}
}

func TestLoadExampleWSLExpandsHost(t *testing.T) {
	t.Setenv("WIN_HOST", "172.31.0.1")
	cfg, err := Load(filepath.Join(repoRoot(t), "examples", "ferry.wsl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].URL != "https://172.31.0.1:7420" {
		t.Fatalf("url %s", cfg.Peers[0].URL)
	}
	if len(cfg.Scenarios) != 1 {
		t.Fatalf("scenarios %+v", cfg.Scenarios)
	}
	var raw string
	for _, d := range cfg.Scenarios[0].Destinations {
		if d.Path != "" {
			raw = d.Path
		}
	}
	if raw != `C:/ferry/archive/` {
		t.Fatalf("windows dest must stay untouched, got %q", raw)
	}
}

func TestMissingEnvHost(t *testing.T) {
	t.Setenv("WIN_HOST", "")
	_, err := Load(filepath.Join(repoRoot(t), "examples", "ferry.wsl.yaml"))
	if err == nil {
		t.Fatal("expected error for empty WIN_HOST")
	}
}

func TestRelativeDataDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ferry.yaml")
	body := []byte(`
node:
  id: a
  listen: "127.0.0.1:1"
  token: "12345678"
peers:
  - id: b
    url: "https://127.0.0.1:2"
    token: "abcdefghi"
    tls_skip_verify: true
jobs:
  - name: push
    source:
      path: "out/file.txt"
    destination:
      node: b
      path: "/data/file.txt"
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.DataDir != filepath.Join(dir, "data") {
		t.Fatalf("data dir %s", cfg.Node.DataDir)
	}
	if cfg.Jobs[0].Source.Path != filepath.Join(dir, "out", "file.txt") && cfg.Jobs[0].Source.Path != filepath.Join(dir, "out/file.txt") {
		// filepath.Join чистит разделитель под текущую ОС.
		if filepath.Clean(cfg.Jobs[0].Source.Path) != filepath.Clean(filepath.Join(dir, "out", "file.txt")) {
			t.Fatalf("source %s", cfg.Jobs[0].Source.Path)
		}
	}
	if cfg.Jobs[0].Destination.Path != "/data/file.txt" {
		t.Fatalf("remote dest rewritten: %s", cfg.Jobs[0].Destination.Path)
	}
}

func TestScenariosFileFanoutAndTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "wsl.token")
	if err := os.WriteFile(tokenPath, []byte("  peer-token-xyz \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scPath := filepath.Join(dir, "scenarios.yaml")
	sc := []byte(`
scenarios:
  - name: fan
    watch: true
    schedule: "15 2 * * *"
    settle: 1s
    sources:
      - path: out
        include: ["*.txt"]
        exclude: ["*.tmp"]
      - path: reports
        include: ["*.csv"]
    destinations:
      - peer: wsl
        alias: inbox
      - peer: wsl
        alias: archive
      - peer: local
        path: copy/
    on_success: move
    success_policy: all
    archive_dir: sent
    retry:
      max_attempts: 4
      initial_backoff: 10ms
      max_backoff: 1s
`)
	if err := os.WriteFile(scPath, sc, 0o644); err != nil {
		t.Fatal(err)
	}
	main := []byte(`
node:
  id: win
  listen: "127.0.0.1:7420"
  token: "12345678"
  scenarios: scenarios.yaml
  inbound:
    - name: inbox
      path: in
peers:
  - id: wsl
    url: "https://127.0.0.1:7421"
    token_file: wsl.token
    tls_skip_verify: true
`)
	path := filepath.Join(dir, "ferry.yaml")
	if err := os.WriteFile(path, main, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].Token != "peer-token-xyz" {
		t.Fatalf("token %q", cfg.Peers[0].Token)
	}
	if len(cfg.Scenarios) != 1 || len(cfg.Scenarios[0].Sources) != 2 || len(cfg.Scenarios[0].Destinations) != 3 {
		t.Fatalf("scenario shape %+v", cfg.Scenarios)
	}
	if !cfg.Scenarios[0].Watch || cfg.Scenarios[0].Schedule == "" {
		t.Fatal("expected both triggers")
	}
	if cfg.Scenarios[0].Retry.MaxAttempts != 4 {
		t.Fatal(cfg.Scenarios[0].Retry.MaxAttempts)
	}
	if cfg.Scenarios[0].Sources[0].Path != filepath.Join(dir, "out") && filepath.Clean(cfg.Scenarios[0].Sources[0].Path) != filepath.Clean(filepath.Join(dir, "out")) {
		t.Fatalf("source path %s", cfg.Scenarios[0].Sources[0].Path)
	}
	// Локальный путь назначения приклеен к каталогу сценария, чужой alias — нет.
	if cfg.Scenarios[0].Destinations[0].Alias != "inbox" || cfg.Scenarios[0].Destinations[0].Path != "" {
		t.Fatalf("remote dest %+v", cfg.Scenarios[0].Destinations[0])
	}
	eff := cfg.EffectiveScenarios()
	if len(eff) != 1 || eff[0].SuccessPolicy != "all" {
		t.Fatalf("effective %+v", eff)
	}
}

func TestScenarioValidation(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`
node:
  id: a
  listen: "127.0.0.1:1"
  token: "12345678"
peers:
  - id: b
    url: "https://127.0.0.1:2"
    token: "abcdefghi"
    tls_skip_verify: true
scenarios:
  - name: bad
    sources:
      - path: out
    destinations:
      - peer: missing
        path: /tmp
`)
	path := filepath.Join(dir, "ferry.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown peer was accepted")
	}
}

func TestOldJobStillLoads(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`
node:
  id: a
  listen: "127.0.0.1:1"
  token: "12345678"
peers:
  - id: b
    url: "https://127.0.0.1:2"
    token: "abcdefghi"
    tls_skip_verify: true
jobs:
  - name: push
    watch: "C:\\ferry\\outbox"
    destination:
      node: b
      path: "/data/"
`)
	path := filepath.Join(dir, "ferry.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.EffectiveScenarios()
	if len(sc) != 1 || !sc[0].Watch || sc[0].Sources[0].Path != `C:\ferry\outbox` {
		t.Fatalf("%+v", sc)
	}
	if sc[0].Destinations[0].Path != "/data/" {
		t.Fatal(sc[0].Destinations[0].Path)
	}
	if sc[0].Retry.MaxAttempts != 5 {
		t.Fatal(sc[0].Retry.MaxAttempts)
	}
}

func TestUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ferry.yaml")
	body := []byte("node:\n  id: a\n  listen: \"127.0.0.1:1\"\n  tokn: secret\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
