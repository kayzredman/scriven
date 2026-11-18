package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"scriven/internal/awscloud"
	"scriven/internal/capture"
	"scriven/internal/config"
	"scriven/internal/library"
	"scriven/internal/notify"
)

// CloudTrail delivers events to its history within minutes, not instantly,
// and not always in order. Each poll looks this far back and skips events it
// has already seen.
const trailLookback = 20 * time.Minute

// awsWatch follows one AWS target through CloudTrail's event history. A
// burst of write calls to the services scriven captures triggers one full
// capture of the target; every resource whose code changed is committed on
// its own to the observed branch, naming the CloudTrail caller that touched it.
type awsWatch struct {
	w        *Watcher
	t        config.Target
	poll     time.Duration
	debounce time.Duration

	seen    map[string]time.Time // CloudTrail event ID -> event time
	pending []awscloud.WriteEvent
	quiet   time.Time // when the pending burst may be recorded
}

func (w *Watcher) newAWSWatch(t config.Target) *awsWatch {
	return &awsWatch{w: w, t: t, poll: w.cfg.Watch.AWSPollDuration(), debounce: w.cfg.Watch.DebounceDuration(),
		seen: map[string]time.Time{}}
}

// check reports whether the identity may read CloudTrail's event history.
func (aw *awsWatch) check(ctx context.Context) error {
	c, err := capture.ConnectAWS(ctx, aw.t, aw.w.resolve)
	if err != nil {
		return err
	}
	_, err = c.WriteEvents(ctx, time.Now().Add(-time.Minute), false)
	return err
}

func (aw *awsWatch) run(ctx context.Context) {
	since := time.Now().Add(-trailLookback)
	tick := time.NewTicker(aw.poll)
	defer tick.Stop()
	for {
		aw.fetch(ctx, since)
		since = time.Now().Add(-trailLookback)
		if len(aw.pending) > 0 && !time.Now().Before(aw.quiet) {
			evs := aw.pending
			aw.pending = nil
			if err := aw.record(ctx, evs); err != nil {
				aw.w.logf("%s: could not record changes: %v", aw.t.Key(), err)
				aw.w.metrics.Add("scriven_record_errors_total", 1, "target", aw.t.Key())
				_ = aw.w.notify.Send(ctx, notify.Message{
					Title: "Scriven could not record a change on " + aw.t.Key(),
					Text:  "CloudTrail showed a change but it was not written to the library. The next sweep will retry.",
					Facts: map[string]string{"error": err.Error()},
				})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (aw *awsWatch) fetch(ctx context.Context, since time.Time) {
	c, err := capture.ConnectAWS(ctx, aw.t, aw.w.resolve) // fresh credentials: assumed roles expire
	if err != nil {
		aw.w.logf("%s: %v", aw.t.Key(), err)
		return
	}
	evs, err := c.WriteEvents(ctx, since, aw.t.CapturesGlobal())
	if err != nil {
		aw.w.logf("%s: reading CloudTrail: %v", aw.t.Key(), err)
		return
	}
	for _, e := range evs {
		if _, dup := aw.seen[e.ID]; dup {
			continue
		}
		aw.seen[e.ID] = e.At
		if e.Agent == "CloudFormation" {
			// A stack doing its own work: its resources are left out of the
			// capture, so re-capturing for this could never record anything.
			aw.w.logf("%s: %s %s by %s (skipped: stack resources are not captured)", aw.t.Key(), e.Source, e.Name, who(e))
			continue
		}
		aw.pending = append(aw.pending, e)
		aw.quiet = time.Now().Add(aw.debounce)
		aw.w.logf("%s: %s %s by %s", aw.t.Key(), e.Source, e.Name, who(e))
	}
	for id, at := range aw.seen {
		if time.Since(at) > 2*trailLookback {
			delete(aw.seen, id)
		}
	}
}

func who(e awscloud.WriteEvent) string {
	if e.Agent == "" {
		return e.Who
	}
	return e.Who + " via " + e.Agent
}

type awsOutcome struct {
	addr, op, diff string
	ev             *awscloud.WriteEvent // the latest event that touched it; nil if none matched
}

func (aw *awsWatch) record(ctx context.Context, evs []awscloud.WriteEvent) error {
	w, t := aw.w, aw.t
	w.logf("%s: %d write call(s) seen in CloudTrail, re-capturing", t.Key(), len(evs))
	res, err := capture.Run(ctx, t, capture.Options{Filter: capture.FilterFor(w.cfg), Secrets: w.resolve})
	if err != nil {
		return err
	}
	if !res.Plan.ZeroDiff() {
		return fmt.Errorf("re-capture failed verification: %s", strings.Join(res.Plan.Changes, "; "))
	}

	w.libMu.Lock()
	defer w.libMu.Unlock()
	base := w.cfg.BaseBranch(t.Env)
	repo, err := library.OpenObserved(ctx, w.cfg.Library, w.token, base)
	if err != nil {
		return err
	}
	defer repo.Close()
	dir := res.Dir
	if !repo.Exists(dir + "/main.tf") {
		return fmt.Errorf("no baseline in the library at %s on %s: run `scriven capture %s` and merge it first", dir, base, t.ID)
	}
	oldMain, _ := os.ReadFile(repo.Path(dir + "/main.tf"))
	oldImp, _ := os.ReadFile(repo.Path(dir + "/imports.tf"))
	added, changed, removed := compareBlocks(oldMain, res.Files["main.tf"])
	if len(added)+len(changed)+len(removed) == 0 {
		w.logf("%s: the library already matches the account", t.Key())
		return nil
	}
	ids := importIDs(oldImp)
	for a, id := range importIDs(res.Files["imports.tf"]) {
		ids[a] = id
	}
	newRes, _ := capture.SplitBlocks(res.Files["main.tf"], "resource")
	newImp, _ := capture.SplitBlocks(res.Files["imports.tf"], "import")

	// Everything but main.tf and imports.tf goes in with the first commit:
	// providers.tf may gain the account data source a new resource uses.
	for name, b := range res.Files {
		if name == "main.tf" || name == "imports.tf" {
			continue
		}
		if err := os.WriteFile(filepath.Join(repo.Path(dir), name), b, 0o644); err != nil {
			return err
		}
	}
	var ops []awsOutcome
	for _, a := range added {
		ops = append(ops, awsOutcome{addr: a, op: "added"})
	}
	for _, a := range changed {
		ops = append(ops, awsOutcome{addr: a, op: "changed"})
	}
	for _, a := range removed {
		ops = append(ops, awsOutcome{addr: a, op: "removed"})
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].addr < ops[j].addr })

	var done []awsOutcome
	for _, o := range ops {
		ch := capture.Change{Address: o.addr}
		if o.op != "removed" {
			ch.Resource, ch.Import = newRes[o.addr], newImp[o.addr]
		}
		before, err := capture.ApplyChanges(repo.Path(dir), []capture.Change{ch})
		if err != nil {
			return err
		}
		o.diff = blockDiff(before[o.addr], string(ch.Resource))
		o.ev = latestTouching(evs, ids[o.addr])
		verb := map[string]string{"added": "add", "changed": "update", "removed": "remove"}[o.op]
		msg := fmt.Sprintf("observe %s: %s %s\n\n%s on %s, seen in CloudTrail by scriven's watcher.\nChanged by: %s",
			t.Key(), verb, o.addr, label(o.expected()), t.Key(), o.changedBy())
		if o.ev != nil {
			msg += fmt.Sprintf("\nCall: %s %s at %s", o.ev.Source, o.ev.Name, o.ev.At.UTC().Format(time.RFC3339))
		}
		committed, err := repo.Commit(ctx, dir, msg)
		if err != nil {
			return err
		}
		if committed {
			done = append(done, o)
		}
	}
	if len(done) == 0 {
		return nil
	}
	if err := repo.PushObserved(ctx); err != nil {
		return err
	}
	pr := w.ensurePR(ctx, base)
	for _, o := range done {
		w.logf("%s: recorded %s %s (%s) by %s", t.Key(), o.op, o.addr, label(o.expected()), o.changedBy())
		w.metrics.Add("scriven_changes_recorded_total", 1, "target", t.Key(), "label",
			map[bool]string{true: "expected", false: "drift"}[o.expected()])
		aw.notify(ctx, o, pr)
	}
	return nil
}

func (o awsOutcome) expected() bool { return o.ev != nil && o.ev.Agent == "Terraform" }

func (o awsOutcome) changedBy() string {
	if o.ev == nil {
		return "unknown (no CloudTrail call named this resource)"
	}
	return who(*o.ev)
}

func latestTouching(evs []awscloud.WriteEvent, importID string) *awscloud.WriteEvent {
	if importID == "" {
		return nil
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Touches(importID) {
			return &evs[i]
		}
	}
	return nil
}

var importIDRe = regexp.MustCompile(`(?m)^\s*id\s*=\s*"((?:[^"\\]|\\.)*)"`)

// importIDs maps each address in imports.tf to the ID it imports.
func importIDs(src []byte) map[string]string {
	out := map[string]string{}
	blocks, err := capture.SplitBlocks(src, "import")
	if err != nil {
		return out
	}
	for addr, b := range blocks {
		if m := importIDRe.FindSubmatch(b); m != nil {
			out[addr] = string(m[1])
		}
	}
	return out
}

func (aw *awsWatch) notify(ctx context.Context, o awsOutcome, pr library.PR) {
	what := map[string]string{"added": "new", "changed": "changed", "removed": "removed"}[o.op]
	title := fmt.Sprintf("Drift on %s: %s %s", aw.t.Key(), o.addr, what)
	if o.expected() {
		title = fmt.Sprintf("Terraform change on %s: %s %s", aw.t.Key(), o.addr, what)
	}
	facts := map[string]string{"environment": aw.t.Env, "resource": o.addr, "changed by": o.changedBy()}
	if o.ev != nil {
		facts["call"] = o.ev.Name
		facts["at"] = o.ev.At.Format(time.RFC1123)
	}
	if err := aw.w.notify.Send(ctx, notify.Message{
		Title:  title,
		Text:   label(o.expected()) + ". Recorded in the library's observed branch for review.",
		Facts:  facts,
		Link:   pr.URL,
		Detail: o.diff,
	}); err != nil {
		aw.w.logf("notification failed: %v", err)
		aw.w.metrics.Add("scriven_notifications_failed_total", 1)
	}
}
