package monitoring

import (
	"time"

	"github.com/UncleUSN/my-repo/internal/events"
)

// Overdue следит, что пара «файл, назначение» не зависла дольше таймаута.
// Сигнал delivery_overdue публикуется один раз, пока доставка не завершится.
type Overdue struct {
	Timeout time.Duration
	now     func() time.Time
	open    map[string]*track
}

type track struct {
	ev      events.Event
	since   time.Time
	alerted bool
}

// NewOverdue создаёт детектор. timeout <= 0 выключает его.
func NewOverdue(timeout time.Duration) *Overdue {
	return &Overdue{
		Timeout: timeout,
		now:     time.Now,
		open:    map[string]*track{},
	}
}

func trackKey(ev events.Event) string {
	id := ev.SHA256
	if id == "" {
		id = ev.File
	}
	return ev.Scenario + "\x00" + id + "\x00" + ev.Peer + "\x00" + ev.Destination
}

func terminal(eventType string) bool {
	switch eventType {
	case events.Delivered, events.Failed, events.DeliveryFailed:
		return true
	default:
		return false
	}
}

func inflight(eventType string) bool {
	switch eventType {
	case events.Queued, events.Started, events.Retried:
		return true
	default:
		return false
	}
}

// Note обновляет состояние по событию шины.
func (o *Overdue) Note(ev events.Event) {
	if o == nil || o.Timeout <= 0 {
		return
	}
	key := trackKey(ev)
	if key == "\x00\x00\x00" || ev.Scenario == "" && ev.File == "" {
		return
	}
	if terminal(ev.Type) {
		delete(o.open, key)
		return
	}
	if !inflight(ev.Type) {
		return
	}
	if _, ok := o.open[key]; ok {
		return
	}
	since := ev.Time
	if since.IsZero() {
		since = o.now()
	}
	o.open[key] = &track{ev: ev, since: since}
}

// Due возвращает просроченные доставки и помечает их, чтобы не повторять сигнал.
func (o *Overdue) Due(now time.Time) []events.Event {
	if o == nil || o.Timeout <= 0 {
		return nil
	}
	var out []events.Event
	for _, tr := range o.open {
		if tr.alerted || now.Sub(tr.since) < o.Timeout {
			continue
		}
		tr.alerted = true
		ev := tr.ev
		ev.Type = events.DeliveryOverdue
		ev.Time = now
		ev.Error = "delivery is still not finished"
		ev.Duration = now.Sub(tr.since)
		out = append(out, ev)
	}
	return out
}
