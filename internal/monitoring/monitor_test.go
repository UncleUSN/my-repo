package monitoring

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/events"
)

func TestLevelFilterAndFormat(t *testing.T) {
	if LevelOf(events.Retried) != LevelWarn || LevelOf(events.DeliveryFailed) != LevelError {
		t.Fatalf("levels %s %s", LevelOf(events.Retried), LevelOf(events.DeliveryFailed))
	}
	if !Allows(LevelWarn, LevelError) || Allows(LevelError, LevelInfo) {
		t.Fatal("filter")
	}
	raw, err := FormatLine(events.Event{
		Type: events.Delivered, Time: time.Date(2026, 11, 11, 12, 0, 0, 0, time.FixedZone("MSK", 3*3600)),
		Node: "win", Scenario: "fan", File: `C:\ferry\a.txt`, Destination: "wsl:inbox/a.txt",
		Peer: "wsl", Size: 4, SHA256: "abcd", Attempt: 1, Duration: 15 * time.Millisecond,
	}, "win")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"timestamp":"2026-11-11T12:00:00.000+03:00"`) {
		t.Fatalf("timestamp %s", raw)
	}
	if !strings.Contains(string(raw), `"level":"info"`) || !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("line %s", raw)
	}
}

func TestFileSinkFiltersAndDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.log")
	bus := events.New()
	cfg := &config.Config{
		Node: config.Node{ID: "win", DataDir: dir},
		Monitoring: config.Monitoring{
			DeliveryTimeout: config.D(time.Hour),
			AlertsLog: config.AlertsLog{
				Path: path, MinLevel: "warn", MaxSizeMB: 1, MaxBackups: 2, MaxAgeDays: 1,
			},
		},
	}
	mon, err := Start(cfg, bus)
	if err != nil {
		t.Fatal(err)
	}
	defer mon.Close()

	sub := bus.Subscribe(1)
	defer sub.Cancel()
	bus.Publish(events.Event{Type: events.Delivered, File: "skip.txt"})
	// Забиваем буфер подписчика и убеждаемся, что Publish не ждёт.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			bus.Publish(events.Event{Type: events.Retried, File: "a.txt", Error: "down", Attempt: i + 1})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked")
	}
	if sub.Dropped() == 0 {
		t.Fatal("expected dropped events")
	}
	deadline := time.Now().Add(2 * time.Second)
	var text string
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), `"event":"retried"`) {
			text = string(b)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(text, `"level":"warn"`) {
		t.Fatalf("log %q", text)
	}
	if strings.Contains(text, `"event":"delivered"`) {
		t.Fatal("info event passed the warn filter")
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.log")
	rot := &Rotator{Path: path, MaxBytes: 80, MaxBackups: 1, MaxAge: time.Hour}
	defer rot.Close()
	line := []byte(strings.Repeat("x", 50) + "\n")
	for i := 0; i < 6; i++ {
		if _, err := rot.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatal("third backup should have been removed")
	}
}

func TestOverdueOnce(t *testing.T) {
	o := NewOverdue(20 * time.Millisecond)
	start := time.Now()
	ev := events.Event{
		Type: events.Started, Time: start, Scenario: "fan", File: "a.txt",
		SHA256: "abc", Peer: "wsl", Destination: "wsl:inbox/a.txt",
	}
	o.Note(ev)
	if got := o.Due(start.Add(10 * time.Millisecond)); len(got) != 0 {
		t.Fatalf("early due %+v", got)
	}
	got := o.Due(start.Add(30 * time.Millisecond))
	if len(got) != 1 || got[0].Type != events.DeliveryOverdue {
		t.Fatalf("due %+v", got)
	}
	if again := o.Due(start.Add(time.Minute)); len(again) != 0 {
		t.Fatal("overdue repeated")
	}
	ev.Type = events.Delivered
	o.Note(ev)
	o.Note(events.Event{
		Type: events.Started, Time: time.Now(), Scenario: "fan", File: "b.txt",
		SHA256: "def", Peer: "wsl", Destination: "wsl:archive/b.txt",
	})
	// Старая доставка закрыта и больше не просрочена, даже если время ушло вперёд.
	if late := o.Due(start.Add(2 * time.Minute)); len(late) != 0 && late[0].File == "a.txt" {
		t.Fatal("delivered file stayed overdue")
	}
}

func TestMonitorWritesOverdue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.log")
	bus := events.New()
	cfg := &config.Config{
		Node: config.Node{ID: "wsl"},
		Monitoring: config.Monitoring{
			DeliveryTimeout: config.D(30 * time.Millisecond),
			AlertsLog:       config.AlertsLog{Path: path, MinLevel: "info", MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1},
		},
	}
	mon, err := Start(cfg, bus)
	if err != nil {
		t.Fatal(err)
	}
	defer mon.Close()
	bus.Publish(events.Event{
		Type: events.Started, Time: time.Now(), Scenario: "back", File: "/tmp/a",
		SHA256: "ff", Peer: "win", Destination: "win:inbox/a",
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), `"event":"delivery_overdue"`) {
			var row Line
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if strings.Contains(line, "delivery_overdue") {
					if json.Unmarshal([]byte(line), &row) != nil || row.Level != "warn" || row.Node != "wsl" {
						t.Fatalf("row %+v line %s", row, line)
					}
				}
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("no overdue in %s", b)
}
