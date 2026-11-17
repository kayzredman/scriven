package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"scriven/internal/capture"
	"scriven/internal/config"
	"scriven/internal/library"
	"scriven/internal/notify"
)

// Sweep runs a full, rate-limited capture of one target and records any
// difference from the library on the observed branch. It catches what the
// watch missed: changes made while scriven was down, or dropped events.
func (w *Watcher) Sweep(ctx context.Context, t config.Target) error {
	started := time.Now()
	drift, err := w.sweep(ctx, t)
	w.metrics.sweep(t.Key(), started, drift, err)
	return err
}

// sweep does the work of Sweep and returns how many resources differ.
func (w *Watcher) sweep(ctx context.Context, t config.Target) (int, error) {
	w.logf("%s: sweep started", t.Key())
	res, err := capture.Run(ctx, t, capture.Options{Filter: capture.FilterFor(w.cfg), Secrets: w.resolve})
	if err != nil {
		return 0, err
	}
	if !res.Plan.ZeroDiff() {
		return 0, fmt.Errorf("sweep capture failed verification: %s", strings.Join(res.Plan.Changes, "; "))
	}

	w.libMu.Lock()
	defer w.libMu.Unlock()
	base := w.cfg.BaseBranch(t.Env)
	repo, err := library.OpenObserved(ctx, w.cfg.Library, w.token, base)
	if err != nil {
		return 0, err
	}
	defer repo.Close()
	dir := res.Dir
	if !repo.Exists(dir + "/main.tf") {
		return 0, fmt.Errorf("no baseline in the library at %s on %s: run `scriven capture %s` and merge it first", dir, base, t.ID)
	}
	oldMain, _ := os.ReadFile(repo.Path(dir + "/main.tf"))
	if err := os.RemoveAll(repo.Path(dir)); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(repo.Path(dir), 0o755); err != nil {
		return 0, err
	}
	for name, b := range res.Files {
		if err := os.WriteFile(filepath.Join(repo.Path(dir), name), b, 0o644); err != nil {
			return 0, err
		}
	}
	msg := fmt.Sprintf("sweep %s: %d resources, zero diff\n\nFull re-capture by scriven's sweeper found differences from the library.",
		t.Key(), res.Catalog.ResourceCount)
	committed, err := repo.Commit(ctx, dir, msg)
	if err != nil {
		return 0, err
	}
	if !committed {
		w.logf("%s: sweep found the library up to date", t.Key())
		return 0, nil
	}
	if err := repo.PushObserved(ctx); err != nil {
		return 0, err
	}
	pr := w.ensurePR(ctx, base)
	added, changed, removed := compareBlocks(oldMain, res.Files["main.tf"])
	w.logf("%s: sweep recorded %d new, %d changed, %d removed", t.Key(), len(added), len(changed), len(removed))
	facts := map[string]string{"environment": t.Env}
	for k, v := range map[string][]string{"new": added, "changed": changed, "removed": removed} {
		if len(v) > 0 {
			facts[k] = strings.Join(v, ", ")
		}
	}
	if err := w.notify.Send(ctx, notify.Message{
		Title: "Drift found by sweep on " + t.Key(),
		Text: "The scheduled full re-capture differs from the library. Who made these changes is not known: " +
			"they happened while the watcher was not running or its events were missed.",
		Facts: facts,
		Link:  pr.URL,
	}); err != nil {
		w.logf("notification failed: %v", err)
		w.metrics.Add("scriven_notifications_failed_total", 1)
	}
	return len(added) + len(changed) + len(removed), nil
}

func compareBlocks(oldMain, newMain []byte) (added, changed, removed []string) {
	o, _ := capture.SplitBlocks(oldMain, "resource")
	n, _ := capture.SplitBlocks(newMain, "resource")
	for a, b := range n {
		if ob, ok := o[a]; !ok {
			added = append(added, a)
		} else if string(ob) != string(b) {
			changed = append(changed, a)
		}
	}
	for a := range o {
		if _, ok := n[a]; !ok {
			removed = append(removed, a)
		}
	}
	return
}

func (w *Watcher) sweepDaily(ctx context.Context, at string) {
	hm, _ := time.Parse("15:04", at)
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		w.logf("next sweep at %s", next.Format("2006-01-02 15:04"))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		for _, t := range w.cfg.Scope.Targets {
			if !capture.Supported(t.Platform) {
				continue
			}
			if err := w.Sweep(ctx, t); err != nil {
				w.logf("%s: sweep failed: %v", t.Key(), err)
			}
		}
	}
}
