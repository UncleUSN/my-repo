// Package events — внутренняя шина событий доставки.
// Сейчас её читает только кольцевой буфер. Следующим шагом на неё
// сажается Prometheus и внешний сбор метрик: подписчик не должен блокировать отправку.
package events

import (
	"sync"
	"time"
)

const (
	Queued    = "queued"
	Started   = "started"
	Delivered = "delivered"
	Failed    = "failed"
	Retried   = "retried"
)

// Event — одно изменение состояния доставки.
// Поля стабильны: их будет забирать и API статуса, и будущие метрики.
type Event struct {
	Type        string        `json:"type"`
	Time        time.Time     `json:"time"`
	Scenario    string        `json:"scenario,omitempty"`
	File        string        `json:"file,omitempty"`
	Size        int64         `json:"size,omitempty"`
	SHA256      string        `json:"sha256,omitempty"`
	Peer        string        `json:"peer,omitempty"`
	Destination string        `json:"destination,omitempty"`
	Attempt     int           `json:"attempt,omitempty"`
	Duration    time.Duration `json:"duration,omitempty"`
	Error       string        `json:"error,omitempty"`
}

// Bus рассылает события подписчикам и помнит последние записи.
type Bus struct {
	mu    sync.Mutex
	subs  []chan Event
	ring  []Event
	limit int
}

// New создаёт шину с кольцом на 200 событий.
func New() *Bus {
	return &Bus{limit: 200}
}

// Publish не блокируется. Медленному подписчику событие пропускается.
func (b *Bus) Publish(ev Event) {
	if b == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ring = append(b.ring, ev)
	if len(b.ring) > b.limit {
		b.ring = b.ring[len(b.ring)-b.limit:]
	}
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe возвращает канал и функцию отписки.
func (b *Bus) Subscribe(buf int) (<-chan Event, func()) {
	if buf < 1 {
		buf = 16
	}
	ch := make(chan Event, buf)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			for i, sub := range b.subs {
				if sub == ch {
					b.subs = append(b.subs[:i], b.subs[i+1:]...)
					break
				}
			}
		})
	}
}

// Recent возвращает последние события, новые в конце.
func (b *Bus) Recent(n int) []Event {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.ring) {
		n = len(b.ring)
	}
	out := make([]Event, n)
	copy(out, b.ring[len(b.ring)-n:])
	return out
}
