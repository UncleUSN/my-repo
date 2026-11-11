package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/UncleUSN/my-repo/internal/checksum"
	"github.com/UncleUSN/my-repo/internal/client"
	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/events"
	"github.com/UncleUSN/my-repo/internal/ledger"
	"github.com/UncleUSN/my-repo/internal/paths"
	"github.com/UncleUSN/my-repo/internal/store"
)

func (e *Engine) snapshotScenarios() []config.Scenario {
	e.mu.Lock()
	cfg := e.cfg
	e.mu.Unlock()
	if cfg == nil {
		return nil
	}
	return cfg.EffectiveScenarios()
}

func (e *Engine) find(name string) (config.Scenario, bool) {
	for _, sc := range e.snapshotScenarios() {
		if sc.Name == name {
			return sc, true
		}
	}
	return config.Scenario{}, false
}

// Run выполняет сценарий один раз: все подходящие файлы сразу, без ожидания settle.
func (e *Engine) Run(ctx context.Context, name string) (RunResult, error) {
	sc, ok := e.find(name)
	if !ok {
		return RunResult{}, ErrNotFound
	}
	if !e.markRunning(name) {
		return RunResult{Name: name}, ErrBusy
	}
	defer e.clearRunning(name)

	res := RunResult{Name: name}
	files, err := e.gather(sc)
	if err != nil {
		e.finishStatus(name, "error", err.Error())
		return res, err
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			res.Errors = append(res.Errors, err.Error())
			res.Failed++
			break
		}
		if err := e.dispatch(ctx, sc, f); err != nil {
			res.Errors = append(res.Errors, f.Rel+": "+err.Error())
			res.Failed++
			e.log.Error("передача", "scenario", name, "file", f.Abs, "err", err)
			continue
		}
		res.Transferred++
	}
	status := "success"
	errText := ""
	var ret error
	if res.Failed > 0 {
		status = "error"
		errText = joinErr(res.Errors)
		ret = fmt.Errorf("%s", errText)
	}
	e.finishStatus(name, status, errText)
	return res, ret
}

func (e *Engine) watchLoop(name string) {
	defer e.wg.Done()
	sc, ok := e.find(name)
	if !ok {
		return
	}
	state := e.loadWatch(name)
	interval := sc.Interval.Duration
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.workerCtx.Done():
			return
		case <-ticker.C:
			e.pollWatch(name, state)
		}
	}
}

func (e *Engine) pollWatch(name string, state *watchState) {
	sc, ok := e.find(name)
	if !ok {
		return
	}
	if !e.markRunning(name) {
		return
	}
	defer e.clearRunning(name)

	files, err := e.gather(sc)
	if err != nil {
		e.log.Warn("обход сценария", "scenario", name, "err", err)
		e.publish(events.Event{Type: events.WatcherError, Scenario: name, Error: err.Error()})
		e.finishStatus(name, "error", err.Error())
		return
	}
	seen := map[string]bool{}
	dirty := false
	var failed, sent int
	for _, f := range files {
		if e.workerCtx.Err() != nil {
			return
		}
		seen[f.Rel] = true
		prev := state.Files[f.Rel]
		size := f.Size
		mod := f.Mod.UnixNano()
		if prev.Size != size || prev.Mod != mod || prev.Since.IsZero() {
			state.Files[f.Rel] = watchFile{Size: size, Mod: mod, Since: time.Now()}
			dirty = true
			continue
		}
		if time.Since(prev.Since) < sc.Settle.Duration {
			continue
		}
		if prev.Sent && prev.SentSize == size && prev.SentMod == mod {
			continue
		}
		if err := e.dispatch(e.workerCtx, sc, f); err != nil {
			failed++
			e.log.Error("передача из каталога наблюдения", "scenario", name, "file", f.Abs, "err", err)
			continue
		}
		sent++
		if sc.OnSuccess == "delete" || sc.OnSuccess == "move" {
			delete(state.Files, f.Rel)
		} else {
			prev.Sent = true
			prev.SentSize = size
			prev.SentMod = mod
			state.Files[f.Rel] = prev
		}
		dirty = true
	}
	for rel := range state.Files {
		if !seen[rel] {
			delete(state.Files, rel)
			dirty = true
		}
	}
	if dirty {
		if err := e.saveWatch(name, state); err != nil {
			e.log.Warn("не удалось сохранить состояние наблюдения", "scenario", name, "err", err)
		}
	}
	if failed > 0 {
		e.finishStatus(name, "error", fmt.Sprintf("failed %d", failed))
		return
	}
	if sent > 0 {
		e.finishStatus(name, "success", "")
	}
}

func (e *Engine) retryLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-e.workerCtx.Done():
			return
		case <-ticker.C:
			e.retryDue(time.Now())
		}
	}
}

func (e *Engine) retryDue(now time.Time) {
	if e.ledger == nil {
		return
	}
	due, err := e.ledger.Due(now)
	if err != nil {
		e.log.Warn("повтор доставки", "err", err)
		return
	}
	// Группируем по сценарию, чтобы не гонять один и тот же файл параллельно с watch.
	seen := map[string]bool{}
	for _, rec := range due {
		key := rec.Scenario + "\x00" + rec.Source
		if seen[key] {
			continue
		}
		seen[key] = true
		sc, ok := e.find(rec.Scenario)
		if !ok {
			continue
		}
		if !e.markRunning(sc.Name) {
			continue
		}
		info, err := fileInfo(rec.Source)
		if err != nil {
			e.clearRunning(sc.Name)
			continue
		}
		info.Rel = rec.Rel
		if err := e.dispatch(e.workerCtx, sc, info); err != nil {
			e.log.Debug("повтор не удался", "scenario", sc.Name, "file", rec.Source, "err", err)
		}
		e.clearRunning(sc.Name)
	}
}

func fileInfo(path string) (File, error) {
	st, err := os.Stat(path)
	if err != nil {
		return File{}, err
	}
	return File{Abs: path, Size: st.Size(), Mod: st.ModTime(), FromDir: true, Rel: filepath.Base(path)}, nil
}

func (e *Engine) gather(sc config.Scenario) ([]File, error) {
	multi := len(sc.Sources) > 1
	var all []File
	for _, src := range sc.Sources {
		root, err := e.guard.Resolve(src.Path)
		if err != nil {
			return nil, err
		}
		files, _, err := collect(root, src.Include, src.Exclude, src.Recursive, sc.ArchiveDir)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(root)
		for _, f := range files {
			if multi {
				f.Rel = base + "/" + f.Rel
			}
			all = append(all, f)
		}
	}
	return all, nil
}

type destOutcome struct {
	ok      bool
	pending bool
	err     string
}

// dispatch рассылает один файл по всем назначениям.
// Пост-действие на источнике — только когда политика (all/any) выполнена
// и ни одно назначение больше не ждёт повтора.
func (e *Engine) dispatch(ctx context.Context, sc config.Scenario, f File) error {
	sum, size, err := checksum.File(f.Abs)
	if err != nil {
		return err
	}
	f.Size = size
	outs := make([]destOutcome, len(sc.Destinations))
	for i, dest := range sc.Destinations {
		outs[i] = e.deliverOne(ctx, sc, dest, f, sum, size)
	}
	if policyDone(sc, outs) {
		if err := e.postScenario(sc, f); err != nil {
			return err
		}
		return nil
	}
	return summarize(sc.Name, outs)
}

func (e *Engine) deliverOne(ctx context.Context, sc config.Scenario, dest config.DestSpec, f File, sum string, size int64) destOutcome {
	destKey := dest.Key()
	remote, alias, err := remoteTarget(dest, f)
	if err != nil {
		return destOutcome{err: err.Error()}
	}
	if e.ledger != nil {
		rec, found, err := e.ledger.Get(sc.Name, sum, f.Rel, destKey)
		if err != nil {
			return destOutcome{err: err.Error()}
		}
		if found && rec.Status == ledger.Delivered {
			return destOutcome{ok: true}
		}
		if found && rec.Status == ledger.Retrying && rec.NextRetry.After(time.Now()) {
			return destOutcome{pending: true}
		}
		if found && rec.Status == ledger.Failed && rec.SHA256 == sum {
			return destOutcome{err: rec.LastError}
		}
	}
	attempts := 1
	if e.ledger != nil {
		if prev, found, _ := e.ledger.Get(sc.Name, sum, f.Rel, destKey); found {
			attempts = prev.Attempts + 1
		}
	}
	if attempts == 1 {
		e.publish(events.Event{
			Type: events.Queued, Scenario: sc.Name, File: f.Abs, Size: size, SHA256: sum,
			Peer: dest.Peer, Destination: describeDest(dest, remote),
		})
	}
	e.publish(events.Event{
		Type: events.Started, Scenario: sc.Name, File: f.Abs, Size: size, SHA256: sum,
		Peer: dest.Peer, Destination: describeDest(dest, remote), Attempt: attempts,
	})
	started := time.Now()
	res, sendErr := e.sendTo(ctx, dest, f.Abs, remote, alias)
	dur := time.Since(started)
	rec := ledger.Record{
		Scenario: sc.Name, Source: f.Abs, Rel: f.Rel, SHA256: sum, Size: size,
		Peer: dest.Peer, Alias: dest.Alias, DestPath: remote, DestKey: destKey,
		Attempts: attempts, Updated: time.Now().UTC(), Duration: dur.Milliseconds(),
	}
	if sendErr != nil {
		rec.LastError = sendErr.Error()
		if attempts >= sc.Retry.MaxAttempts {
			rec.Status = ledger.Failed
			_ = e.saveRecord(rec)
			e.publish(events.Event{
				Type: events.Failed, Scenario: sc.Name, File: f.Abs, Size: size, SHA256: sum,
				Peer: dest.Peer, Destination: describeDest(dest, remote), Attempt: attempts,
				Duration: dur, Error: sendErr.Error(),
			})
			e.publish(events.Event{
				Type: events.DeliveryFailed, Scenario: sc.Name, File: f.Abs, Size: size, SHA256: sum,
				Peer: dest.Peer, Destination: describeDest(dest, remote), Attempt: attempts,
				Duration: dur, Error: sendErr.Error(),
			})
			e.notePeer(dest.Peer, sendErr)
			e.noteTransfer(sc, dest, f, remote, sum, size, "error", sendErr.Error())
			return destOutcome{err: sendErr.Error()}
		}
		rec.Status = ledger.Retrying
		rec.NextRetry = time.Now().Add(ledger.Backoff(attempts, sc.Retry.InitialBackoff.Duration, sc.Retry.MaxBackoff.Duration))
		_ = e.saveRecord(rec)
		e.publish(events.Event{
			Type: events.Retried, Scenario: sc.Name, File: f.Abs, Size: size, SHA256: sum,
			Peer: dest.Peer, Destination: describeDest(dest, remote), Attempt: attempts,
			Duration: dur, Error: sendErr.Error(),
		})
		e.notePeer(dest.Peer, sendErr)
		return destOutcome{pending: true, err: sendErr.Error()}
	}
	if res.SHA256 != "" {
		rec.SHA256 = res.SHA256
	}
	if res.Size > 0 {
		rec.Size = res.Size
	}
	rec.Status = ledger.Delivered
	rec.LastError = ""
	_ = e.saveRecord(rec)
	e.publish(events.Event{
		Type: events.Delivered, Scenario: sc.Name, File: f.Abs, Size: rec.Size, SHA256: rec.SHA256,
		Peer: dest.Peer, Destination: describeDest(dest, remote), Attempt: attempts, Duration: dur,
	})
	e.notePeer(dest.Peer, nil)
	e.noteTransfer(sc, dest, f, remote, rec.SHA256, rec.Size, "success", "")
	return destOutcome{ok: true}
}

func (e *Engine) saveRecord(rec ledger.Record) error {
	if e.ledger == nil {
		return nil
	}
	return e.ledger.Put(rec)
}

func (e *Engine) sendTo(ctx context.Context, dest config.DestSpec, local, remote, alias string) (client.Result, error) {
	if e.hook != nil {
		if res, err, handled := e.hook(ctx, dest.Peer, local, remote, alias); handled {
			return res, err
		}
	}
	cfg := e.Config()
	if cfg.IsLocal(dest.Peer) {
		target := remote
		if alias != "" {
			in := cfg.Inbound(alias)
			if in == nil {
				return client.Result{}, fmt.Errorf("unknown inbound alias %q", alias)
			}
			joined, err := paths.Join(in.Path, remote)
			if err != nil {
				return client.Result{}, err
			}
			target = joined
		}
		return e.copyLocal(local, target)
	}
	cl, err := e.clientFor(dest.Peer)
	if err != nil {
		return client.Result{}, err
	}
	e.log.Info("отправка", "file", local, "peer", dest.Peer, "dest", remote, "alias", alias)
	return cl.Upload(ctx, local, remote, alias)
}

func (e *Engine) postScenario(sc config.Scenario, f File) error {
	switch sc.OnSuccess {
	case "", "none", "keep":
		return nil
	case "delete":
		return e.postDelete(f.Abs)
	case "move":
		return e.postMove(sc.ArchiveDir, f.Abs)
	default:
		return fmt.Errorf("unknown on_success %q", sc.OnSuccess)
	}
}

func remoteTarget(dest config.DestSpec, f File) (string, string, error) {
	dirMode := f.FromDir || paths.HasTrailingSeparator(dest.Path)
	if dest.Alias != "" {
		rel := f.Rel
		if dest.Path != "" {
			joined, err := paths.Join(dest.Path, f.Rel)
			if err != nil {
				return "", "", err
			}
			rel = joined
		}
		return rel, dest.Alias, nil
	}
	if dirMode {
		p, err := paths.Join(dest.Path, f.Rel)
		return p, "", err
	}
	p, err := paths.Clean(dest.Path)
	return p, "", err
}

func describeDest(dest config.DestSpec, remote string) string {
	if dest.Alias != "" {
		return dest.Peer + ":" + dest.Alias + "/" + remote
	}
	return dest.Peer + ":" + remote
}

func policyDone(sc config.Scenario, outs []destOutcome) bool {
	if len(outs) == 0 {
		return false
	}
	oks, pending := 0, 0
	for _, o := range outs {
		if o.pending {
			pending++
			continue
		}
		if o.ok {
			oks++
		}
	}
	if pending > 0 {
		return false
	}
	switch sc.SuccessPolicy {
	case "any":
		return oks > 0
	default:
		return oks == len(outs)
	}
}

func summarize(name string, outs []destOutcome) error {
	msg := ""
	for _, o := range outs {
		if o.ok {
			continue
		}
		if msg != "" {
			msg += "; "
		}
		if o.pending {
			msg += "pending"
			if o.err != "" {
				msg += ": " + o.err
			}
			continue
		}
		if o.err != "" {
			msg += o.err
		}
	}
	if msg == "" {
		msg = "delivery incomplete"
	}
	return fmt.Errorf("scenario %s: %s", name, msg)
}

// notePeer один раз сообщает, что сосед перестал принимать файлы, и один раз — что снова доступен.
func (e *Engine) notePeer(peer string, err error) {
	if peer == "" {
		return
	}
	e.mu.Lock()
	if e.cfg != nil && e.cfg.IsLocal(peer) {
		e.mu.Unlock()
		return
	}
	down := e.peerDown[peer]
	if err != nil {
		if down {
			e.mu.Unlock()
			return
		}
		e.peerDown[peer] = true
		e.mu.Unlock()
		e.publish(events.Event{Type: events.PeerUnreachable, Peer: peer, Error: err.Error()})
		return
	}
	if !down {
		e.mu.Unlock()
		return
	}
	e.peerDown[peer] = false
	e.mu.Unlock()
	e.publish(events.Event{Type: events.PeerRecovered, Peer: peer})
}

func (e *Engine) publish(ev events.Event) {
	if e.bus != nil {
		e.bus.Publish(ev)
	}
}

func (e *Engine) noteTransfer(sc config.Scenario, dest config.DestSpec, f File, remote, sum string, size int64, status, errText string) {
	if e.store == nil {
		return
	}
	now := time.Now().UTC()
	_ = e.store.Add(store.Transfer{
		Job:        sc.Name,
		Direction:  "out",
		Peer:       dest.Peer,
		LocalPath:  f.Abs,
		RemotePath: describeDest(dest, remote),
		Size:       size,
		SHA256:     sum,
		Status:     status,
		Error:      errText,
		StartedAt:  now,
		FinishedAt: now,
	})
}
