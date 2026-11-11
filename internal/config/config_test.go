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
	if cfg.Jobs[0].Kind() != "watch" {
		t.Fatal(cfg.Jobs[0].Kind())
	}
	if cfg.Jobs[0].Watch != `C:\ferry\outbox` {
		t.Fatalf("watch path rewritten: %q", cfg.Jobs[0].Watch)
	}
	if cfg.Jobs[0].Destination.Path != "/home/USER/ferry/inbox/" {
		t.Fatalf("dest path %q", cfg.Jobs[0].Destination.Path)
	}
	if cfg.Jobs[0].Settle.Duration != 2*time.Second {
		t.Fatalf("settle %s", cfg.Jobs[0].Settle.Duration)
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
	if cfg.Jobs[0].Destination.Path != `C:/ferry/inbox/` {
		t.Fatalf("windows dest must stay untouched, got %q", cfg.Jobs[0].Destination.Path)
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
