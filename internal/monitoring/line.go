package monitoring

import (
	"encoding/json"
	"time"

	"github.com/UncleUSN/my-repo/internal/events"
)

// Line — одна строка alerts.log.
type Line struct {
	Timestamp   string `json:"timestamp"`
	Level       string `json:"level"`
	Event       string `json:"event"`
	Node        string `json:"node,omitempty"`
	Scenario    string `json:"scenario,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Peer        string `json:"peer,omitempty"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
	Error       string `json:"error,omitempty"`
	Dropped     uint64 `json:"dropped,omitempty"`
}

// FormatLine собирает JSON с меткой времени и числовым смещением зоны (+03:00, не Z).
func FormatLine(ev events.Event, node string) ([]byte, error) {
	ts := ev.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	if node == "" {
		node = ev.Node
	}
	line := Line{
		Timestamp:   ts.Format("2006-01-02T15:04:05.000Z07:00"),
		Level:       LevelOf(ev.Type),
		Event:       ev.Type,
		Node:        node,
		Scenario:    ev.Scenario,
		Source:      ev.File,
		Destination: ev.Destination,
		Peer:        ev.Peer,
		Size:        ev.Size,
		SHA256:      ev.SHA256,
		Attempt:     ev.Attempt,
		DurationMs:  ev.Duration.Milliseconds(),
		Error:       ev.Error,
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
