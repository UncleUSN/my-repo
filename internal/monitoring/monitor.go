// Package monitoring пишет события шины в alerts.log и готовит место
// для будущего Prometheus и внешних уведомлений.
// Запись не должна останавливать передачу файлов: подписчик с буфером,
// лишние события отбрасываются и считаются.
package monitoring

import (
	"context"
	"sync"
	"time"

	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/events"
)

// Sink — куда складывать события. Файловый журнал — первая реализация.
type Sink interface {
	Handle(ev events.Event) error
	Close() error
}

// Metrics — будущий сборщик Prometheus. Сейчас интерфейс без реализации.
type Metrics interface {
	Observe(ev events.Event)
}

// Alerter — будущие внешние уведомления (почта, webhook). Сейчас интерфейс без реализации.
type Alerter interface {
	Notify(ctx context.Context, ev events.Event) error
}

// Monitor читает шину, режет просроченные доставки и пишет файл.
type Monitor struct {
	node     string
	min      string
	bus      *events.Bus
	sub      *events.Subscription
	rot      *Rotator
	overdue  *Overdue
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	seenDrop uint64
}

// Start подписывается на шину и начинает писать журнал.
func Start(cfg *config.Config, bus *events.Bus) (*Monitor, error) {
	if cfg == nil || bus == nil {
		return nil, nil
	}
	path := cfg.Monitoring.AlertsLog.Path
	if path == "" {
		return nil, nil
	}
	maxBytes := int64(cfg.Monitoring.AlertsLog.MaxSizeMB) << 20
	if cfg.Monitoring.AlertsLog.MaxSizeMB <= 0 {
		maxBytes = 10 << 20
	}
	age := time.Duration(cfg.Monitoring.AlertsLog.MaxAgeDays) * 24 * time.Hour
	m := &Monitor{
		node: cfg.Node.ID,
		min:  cfg.Monitoring.AlertsLog.MinLevel,
		bus:  bus,
		rot: &Rotator{
			Path:       path,
			MaxBytes:   maxBytes,
			MaxBackups: cfg.Monitoring.AlertsLog.MaxBackups,
			MaxAge:     age,
		},
		overdue: NewOverdue(cfg.Monitoring.DeliveryTimeout.Duration),
		sub:     bus.Subscribe(1024),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if m.rot.MaxBackups <= 0 {
		m.rot.MaxBackups = 5
	}
	go m.loop()
	return m, nil
}

func (m *Monitor) loop() {
	defer close(m.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	if m.overdue != nil && m.overdue.Timeout > 0 && m.overdue.Timeout < time.Second {
		tick.Reset(m.overdue.Timeout / 2)
		if m.overdue.Timeout/2 < 5*time.Millisecond {
			tick.Reset(5 * time.Millisecond)
		}
	}
	for {
		select {
		case <-m.stop:
			m.drain()
			return
		case ev, ok := <-m.sub.Events:
			if !ok {
				return
			}
			m.handle(ev)
		case now := <-tick.C:
			m.emitOverdue(now)
		}
	}
}

func (m *Monitor) drain() {
	for {
		select {
		case ev := <-m.sub.Events:
			m.handle(ev)
		default:
			return
		}
	}
}

func (m *Monitor) handle(ev events.Event) {
	if m.overdue != nil {
		m.overdue.Note(ev)
	}
	m.write(ev)
	m.noteDrops()
}

func (m *Monitor) emitOverdue(now time.Time) {
	if m.overdue == nil || m.bus == nil {
		return
	}
	for _, ev := range m.overdue.Due(now) {
		if ev.Node == "" {
			ev.Node = m.node
		}
		m.bus.Publish(ev)
	}
}

func (m *Monitor) write(ev events.Event) {
	if !Allows(m.min, LevelOf(ev.Type)) {
		return
	}
	raw, err := FormatLine(ev, m.node)
	if err != nil {
		return
	}
	_, _ = m.rot.Write(raw)
}

func (m *Monitor) noteDrops() {
	if m.sub == nil {
		return
	}
	n := m.sub.Dropped()
	if n <= m.seenDrop {
		return
	}
	m.seenDrop = n
	ev := events.Event{
		Type:  events.AlertsDropped,
		Time:  time.Now(),
		Node:  m.node,
		Error: "alert buffer is full, some events were dropped",
	}
	if !Allows(m.min, LevelOf(ev.Type)) {
		return
	}
	raw, err := FormatLine(ev, m.node)
	if err != nil {
		return
	}
	// Вставляем счётчик в уже собранный JSON перед закрывающей скобкой.
	raw = insertDropped(raw, n)
	_, _ = m.rot.Write(raw)
}

func insertDropped(raw []byte, n uint64) []byte {
	if len(raw) < 2 || raw[len(raw)-2] != '}' {
		return raw
	}
	extra := []byte(`,"dropped":` + strconvUint(n))
	out := make([]byte, 0, len(raw)+len(extra))
	out = append(out, raw[:len(raw)-2]...)
	out = append(out, extra...)
	out = append(out, raw[len(raw)-2:]...)
	return out
}

func strconvUint(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Close дописывает буфер и закрывает файл.
func (m *Monitor) Close() error {
	if m == nil {
		return nil
	}
	var err error
	m.once.Do(func() {
		if m.sub != nil {
			m.sub.Cancel()
		}
		close(m.stop)
		<-m.done
		err = m.rot.Close()
	})
	return err
}

// WriteOne пишет одно событие напрямую, когда агент ещё не запущен
// (например, конфиг не прошёл проверку).
func WriteOne(cfg *config.Config, ev events.Event) error {
	if cfg == nil || cfg.Monitoring.AlertsLog.Path == "" {
		return nil
	}
	rot := &Rotator{
		Path:       cfg.Monitoring.AlertsLog.Path,
		MaxBytes:   int64(cfg.Monitoring.AlertsLog.MaxSizeMB) << 20,
		MaxBackups: cfg.Monitoring.AlertsLog.MaxBackups,
		MaxAge:     time.Duration(cfg.Monitoring.AlertsLog.MaxAgeDays) * 24 * time.Hour,
	}
	defer rot.Close()
	if !Allows(cfg.Monitoring.AlertsLog.MinLevel, LevelOf(ev.Type)) {
		return nil
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	raw, err := FormatLine(ev, cfg.Node.ID)
	if err != nil {
		return err
	}
	_, err = rot.Write(raw)
	return err
}
