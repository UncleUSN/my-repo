// Package events — внутренняя шина событий доставки.
// Сейчас её читает только кольцевой буфер. Следующим шагом на неё
// сажается Prometheus и внешний сбор метрик: подписчик не должен блокировать отправку.
package events

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	Queued    = "queued"
	Started   = "started"
	Delivered = "delivered"
	Failed    = "failed"
	Retried   = "retried"

	// DeliveryFailed — попытки для пары «файл, назначение» закончились.
	DeliveryFailed = "delivery_failed"
	// DeliveryOverdue — доставка не завершилась за отведённый срок.
	DeliveryOverdue = "delivery_overdue"

	ServiceStarted        = "service_started"
	ServiceStopped        = "service_stopped"
	ScenariosReloaded     = "scenarios_reloaded"
	ScenariosReloadFailed = "scenarios_reload_failed"
	ConfigInvalid         = "config_invalid"
	PeerUnreachable       = "peer_unreachable"
	PeerRecovered         = "peer_recovered"
	WatcherError          = "watcher_error"
	AlertsDropped         = "alerts_dropped"
)

// Event — одно изменение состояния доставки или узла.
// Поля стабильны: их забирают файл alerts.log и, позже, метрики.
type Event struct {
	Type        string        `json:"type"`
	Time        time.Time     `json:"time"`
	Node        string        `json:"node,omitempty"`
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

type subscriber struct {
	ch      chan Event
	dropped atomic.Uint64
}

// Bus рассылает события подписчикам и помнит последние записи.
// Если буфер подписчика полон, событие пропускается и растёт счётчик Dropped.
type Bus struct {
	mu    sync.Mutex
	subs  []*subscriber
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
	for _, sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			sub.dropped.Add(1)
		}
	}
}

// Subscription — подписка на шину. События не ждут медленного читателя.
type Subscription struct {
	Events <-chan Event
	sub    *subscriber
	bus    *Bus
	once   sync.Once
}

// Dropped — сколько событий пропущено, потому что буфер был полон.
func (s *Subscription) Dropped() uint64 {
	if s == nil || s.sub == nil {
		return 0
	}
	return s.sub.dropped.Load()
}

// Cancel отписывается. Повторный вызов ничего не делает.
func (s *Subscription) Cancel() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.bus.remove(s.sub)
	})
}

// Subscribe подписывается на шину. buf — размер буфера; лишние события отбрасываются.
func (b *Bus) Subscribe(buf int) *Subscription {
	if b == nil {
		ch := make(chan Event)
		close(ch)
		return &Subscription{Events: ch}
	}
	if buf < 1 {
		buf = 16
	}
	sub := &subscriber{ch: make(chan Event, buf)}
	b.mu.Lock()
	b.subs = append(b.subs, sub)
	b.mu.Unlock()
	return &Subscription{Events: sub.ch, sub: sub, bus: b}
}

func (b *Bus) remove(sub *subscriber) {
	if b == nil || sub == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, item := range b.subs {
		if item == sub {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			return
		}
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
