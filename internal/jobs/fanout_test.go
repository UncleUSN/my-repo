package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/UncleUSN/my-repo/internal/client"
	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/guard"
	"github.com/UncleUSN/my-repo/internal/ledger"
)

func TestFanoutRetriesThenMovesSource(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "out")
	destA := filepath.Join(root, "a")
	destB := filepath.Join(root, "b")
	archive := filepath.Join(root, "sent")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello fanout")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := guard.New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Node: config.Node{ID: "n", DataDir: filepath.Join(root, "data"), AllowedRoots: []string{root}},
		Scenarios: []config.Scenario{{
			Name: "fan",
			Sources: []config.SourceSpec{{
				Path: src, Include: []string{"*.txt"}, Exclude: []string{"*.tmp"},
			}},
			Destinations: []config.DestSpec{
				{Peer: "local", Path: destA + string(os.PathSeparator)},
				{Peer: "flaky", Path: destB + string(os.PathSeparator)},
			},
			OnSuccess:     "move",
			SuccessPolicy: "all",
			ArchiveDir:    archive,
			Retry: config.RetryPolicy{
				MaxAttempts:    3,
				InitialBackoff: config.D(time.Millisecond),
				MaxBackoff:     config.D(5 * time.Millisecond),
			},
		}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(cfg, nil, g, log)
	t.Cleanup(eng.Stop)
	hits := 0
	eng.hook = func(_ context.Context, peer, local, remote, _ string) (client.Result, error, bool) {
		if peer != "flaky" {
			return client.Result{}, nil, false
		}
		hits++
		if hits == 1 {
			return client.Result{}, errors.New("peer down"), true
		}
		if err := os.MkdirAll(filepath.Dir(remote), 0o755); err != nil {
			return client.Result{}, err, true
		}
		b, err := os.ReadFile(local)
		if err != nil {
			return client.Result{}, err, true
		}
		if err := os.WriteFile(remote, b, 0o644); err != nil {
			return client.Result{}, err, true
		}
		return client.Result{Path: remote, Size: int64(len(b)), SHA256: "x"}, nil, true
	}

	ctx := context.Background()
	if _, err := eng.Run(ctx, "fan"); err == nil {
		t.Fatal("expected partial failure")
	}
	if _, err := os.Stat(filepath.Join(src, "hello.txt")); err != nil {
		t.Fatal("source was removed before every destination succeeded")
	}
	got, err := os.ReadFile(filepath.Join(destA, "hello.txt"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("first destination: %v %q", err, got)
	}
	if _, err := os.Stat(filepath.Join(destB, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("second destination should be missing, err=%v", err)
	}

	time.Sleep(20 * time.Millisecond)
	eng.retryDue(time.Now())
	if _, err := os.Stat(filepath.Join(src, "hello.txt")); !os.IsNotExist(err) {
		t.Fatal("source should be moved after all destinations succeed")
	}
	got, err = os.ReadFile(filepath.Join(destB, "hello.txt"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("second destination after retry: %v %q", err, got)
	}
	if _, err := os.Stat(filepath.Join(archive, "hello.txt")); err != nil {
		t.Fatal(err)
	}
	list, err := eng.Deliveries("fan", ledger.Delivered, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("delivered records: %+v", list)
	}
}

func TestAnyPolicyMovesAfterOneSuccess(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "out")
	destA := filepath.Join(root, "a")
	archive := filepath.Join(root, "sent")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := guard.New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Node: config.Node{ID: "n", DataDir: filepath.Join(root, "data"), AllowedRoots: []string{root}},
		Scenarios: []config.Scenario{{
			Name:    "one",
			Sources: []config.SourceSpec{{Path: src}},
			Destinations: []config.DestSpec{
				{Peer: "local", Path: destA + string(os.PathSeparator)},
				{Peer: "dead", Path: "/ignored"},
			},
			OnSuccess:     "move",
			SuccessPolicy: "any",
			ArchiveDir:    archive,
			Retry:         config.RetryPolicy{MaxAttempts: 1, InitialBackoff: config.D(time.Millisecond), MaxBackoff: config.D(time.Millisecond)},
		}},
	}
	eng := New(cfg, nil, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(eng.Stop)
	eng.hook = func(_ context.Context, peer, _, _, _ string) (client.Result, error, bool) {
		if peer == "dead" {
			return client.Result{}, errors.New("no"), true
		}
		return client.Result{}, nil, false
	}
	if _, err := eng.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(archive, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("source still present")
	}
}
