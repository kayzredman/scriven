package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"scriven/internal/capture"
	"scriven/internal/checks"
	"scriven/internal/config"
	"scriven/internal/k8s"
	"scriven/internal/library"
	"scriven/internal/notify"
)

// Watcher follows every Rancher target's watch API and records changes in
// the library's observed branch.
type Watcher struct {
	cfg     *config.Config
	log     io.Writer
	token   string
	resolve func(string) (string, error)
	notify  *notify.Notifier
	audit   *AuditStore
	forge   library.Forge

	libMu sync.Mutex // one library update at a time, across all targets
}

func New(c *config.Config, log io.Writer) (*Watcher, error) {
	res := checks.Resolver(c)
	token, err := res.Resolve(c.Library.Credentials)
	if err != nil {
		return nil, err
	}
	f, err := library.NewForge(c.Library, token)
	if err != nil {
		return nil, err
	}
	return &Watcher{cfg: c, log: log, token: token, resolve: res.Resolve, notify: notify.New(c.Notify, res), forge: f}, nil
}

func (w *Watcher) logf(format string, a ...any) {
	fmt.Fprintf(w.log, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// Run watches until ctx ends. sweepOnStart runs a full sweep of each target
// first, which catches anything that changed while scriven was not running.
func (w *Watcher) Run(ctx context.Context, sweepOnStart bool) error {
	if addr := w.cfg.Watch.AuditListen; addr != "" {
		w.audit = NewAuditStore()
		srv := &http.Server{Addr: addr, Handler: w.mux(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			w.logf("audit webhook listening on %s (POST /audit/<target-id>)", addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				w.logf("audit webhook stopped: %v", err)
			}
		}()
		go func() { <-ctx.Done(); _ = srv.Close() }()
	}

	var wg sync.WaitGroup
	started, sweepOnly := 0, 0
	for _, t := range w.cfg.Scope.Targets {
		if t.Platform != "rancher" {
			if !capture.Supported(t.Platform) {
				w.logf("%s: %s targets are not supported yet, skipped", t.Key(), t.Platform)
				continue
			}
			// No change feed for this platform yet: drift is found by sweeps.
			w.logf("%s: not watched live; covered by sweeps only", t.Key())
			sweepOnly++
			if sweepOnStart {
				if err := w.Sweep(ctx, t); err != nil {
					w.logf("%s: start-up sweep failed: %v", t.Key(), err)
				}
			}
			continue
		}
		if sweepOnStart {
			if err := w.Sweep(ctx, t); err != nil {
				w.logf("%s: start-up sweep failed: %v", t.Key(), err)
			}
		}
		tw, err := w.newTargetWatch(ctx, t)
		if err != nil {
			w.logf("%s: not watched: %v", t.Key(), err)
			continue
		}
		started++
		wg.Add(1)
		go func() { defer wg.Done(); tw.run(ctx) }()
	}
	at := w.cfg.Watch.SweepAt
	if started == 0 && (sweepOnly == 0 || at == "") {
		return fmt.Errorf("no target could be watched (sweep-only targets need watch.sweep_at, or run `scriven sweep` from a scheduler)")
	}
	if at != "" {
		go w.sweepDaily(ctx, at)
	}
	if started == 0 {
		<-ctx.Done()
	}
	wg.Wait()
	return ctx.Err()
}

// ---- one target ----

type event struct {
	op  string // added | changed | removed
	res k8s.Resource
	obj unstructured.Unstructured
	at  time.Time
}

type targetWatch struct {
	w        *Watcher
	s        *capture.Session
	debounce time.Duration

	mu      sync.Mutex
	synced  bool
	hashes  map[string]string // object key -> hash of what matters
	pending map[string]event
	timers  map[string]*time.Timer
	ready   chan string
	hpa     cache.Store
}

func (w *Watcher) newTargetWatch(ctx context.Context, t config.Target) (*targetWatch, error) {
	s, err := capture.Open(ctx, t, capture.FilterFor(w.cfg), nil)
	if err != nil {
		return nil, err
	}
	s.Quiet()
	return &targetWatch{w: w, s: s, debounce: w.cfg.Watch.DebounceDuration(),
		hashes: map[string]string{}, pending: map[string]event{}, timers: map[string]*time.Timer{},
		ready: make(chan string, 1024)}, nil
}

func objKey(r k8s.Resource, o *unstructured.Unstructured) string {
	return r.GVR.String() + "|" + o.GetNamespace() + "|" + o.GetName()
}

func (tw *targetWatch) run(ctx context.Context) {
	t := tw.s.Target
	c := tw.s.Client
	resources, err := c.ListableResources()
	if err != nil {
		tw.w.logf("%s: discovery failed: %v", t.Key(), err)
		return
	}
	factory := dynamicinformer.NewDynamicSharedInformerFactory(c.Dynamic, 0)
	var syncs []cache.InformerSynced
	kinds := 0
	for _, r := range resources {
		if !tw.s.Filter.WatchableKind(r) || !capture.Typed(r) || !tw.readable(ctx, r) {
			continue
		}
		inf := factory.ForResource(r.GVR).Informer()
		if r.Kind == "HorizontalPodAutoscaler" {
			tw.hpa = inf.GetStore()
		}
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(o any) { tw.observe("added", r, o) },
			UpdateFunc: func(_, o any) { tw.observe("changed", r, o) },
			DeleteFunc: func(o any) { tw.observe("removed", r, o) },
		})
		syncs = append(syncs, inf.HasSynced)
		kinds++
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), syncs...) {
		return
	}
	tw.mu.Lock()
	tw.synced = true
	n := len(tw.hashes)
	tw.mu.Unlock()
	tw.w.logf("%s: watching %d kinds, %d objects in scope (debounce %s)", t.Key(), kinds, n, tw.debounce)

	for {
		select {
		case <-ctx.Done():
			return
		case key := <-tw.ready:
			batch := []string{key}
			// Gather everything else that is due, so one library update covers a burst.
			drain := time.After(500 * time.Millisecond)
		collect:
			for {
				select {
				case k := <-tw.ready:
					batch = append(batch, k)
				case <-drain:
					break collect
				}
			}
			tw.process(ctx, batch)
		}
	}
}

// readable probes list permission so forbidden kinds don't spam watch errors.
func (tw *targetWatch) readable(ctx context.Context, r k8s.Resource) bool {
	_, err := tw.s.Client.Dynamic.Resource(r.GVR).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

func (tw *targetWatch) observe(op string, r k8s.Resource, o any) {
	if d, ok := o.(cache.DeletedFinalStateUnknown); ok {
		o = d.Obj
	}
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return
	}
	if tw.s.Filter.Decide(r, *u).Verdict != capture.Capture {
		return
	}
	key := objKey(r, u)
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if op == "removed" {
		delete(tw.hashes, key)
	} else {
		h := tw.hash(r, u)
		prev, seen := tw.hashes[key]
		tw.hashes[key] = h
		if !tw.synced || (seen && prev == h) {
			return // initial listing, or only status/metadata noise changed
		}
		if !seen {
			op = "added"
		}
	}
	if p, ok := tw.pending[key]; ok && p.op == "added" && op == "changed" {
		op = "added" // still new as far as the library is concerned
	}
	tw.pending[key] = event{op: op, res: r, obj: *u.DeepCopy(), at: time.Now()}
	if t, ok := tw.timers[key]; ok {
		t.Reset(tw.debounce)
		return
	}
	tw.timers[key] = time.AfterFunc(tw.debounce, func() { tw.ready <- key })
}

// hash covers what a capture would write, so status updates, resourceVersion
// bumps and autoscaler replica changes don't count as changes.
func (tw *targetWatch) hash(r k8s.Resource, u *unstructured.Unstructured) string {
	o := u.DeepCopy().Object
	delete(o, "status")
	if md, ok := o["metadata"].(map[string]any); ok {
		for _, k := range []string{"resourceVersion", "managedFields", "generation", "uid", "creationTimestamp", "selfLink"} {
			delete(md, k)
		}
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, "deployment.kubernetes.io/revision")
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	if tw.autoscaled(r, u) {
		unstructured.RemoveNestedField(o, "spec", "replicas")
	}
	b, _ := json.Marshal(o)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (tw *targetWatch) autoscaled(r k8s.Resource, u *unstructured.Unstructured) bool {
	if tw.hpa == nil || (r.Kind != "Deployment" && r.Kind != "StatefulSet") {
		return false
	}
	for _, x := range tw.hpa.List() {
		h, ok := x.(*unstructured.Unstructured)
		if !ok || h.GetNamespace() != u.GetNamespace() {
			continue
		}
		kind, _, _ := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "kind")
		name, _, _ := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "name")
		if kind == r.Kind && name == u.GetName() {
			return true
		}
	}
	return false
}

// ---- turning changes into library commits ----

type outcome struct {
	ev       event
	addr     string
	diff     string
	who      string
	via      string
	expected bool
}

func (tw *targetWatch) process(ctx context.Context, keys []string) {
	tw.mu.Lock()
	var evs []event
	for _, k := range keys {
		if ev, ok := tw.pending[k]; ok {
			evs = append(evs, ev)
			delete(tw.pending, k)
			delete(tw.timers, k)
		}
	}
	tw.mu.Unlock()
	if len(evs) == 0 {
		return
	}
	t := tw.s.Target
	for _, ev := range evs {
		tw.w.logf("%s: %s %s %s", t.Key(), ev.op, ev.res.Kind, objID(ev.obj))
	}
	if err := tw.record(ctx, evs); err != nil {
		tw.w.logf("%s: could not record changes: %v", t.Key(), err)
		_ = tw.w.notify.Send(ctx, notify.Message{
			Title: "Scriven could not record a change on " + t.Key(),
			Text:  "A change was seen but not written to the library. The next sweep will retry.",
			Facts: map[string]string{"error": err.Error()},
		})
	}
}

func (tw *targetWatch) record(ctx context.Context, evs []event) error {
	w, t := tw.w, tw.s.Target
	w.libMu.Lock()
	defer w.libMu.Unlock()

	repo, err := library.OpenObserved(ctx, w.cfg.Library, w.token)
	if err != nil {
		return err
	}
	defer repo.Close()
	dir := capture.LibraryDir(t)
	if !repo.Exists(dir + "/main.tf") {
		return fmt.Errorf("no baseline in the library at %s: run `scriven capture %s` and merge it first", dir, t.ID)
	}

	// Re-capture only the objects that were added or changed.
	var objs []capture.Discovered
	for _, ev := range evs {
		if ev.op != "removed" {
			objs = append(objs, capture.Discovered{Res: ev.res, Obj: ev.obj})
		}
	}
	var cp *capture.Captured
	if len(objs) > 0 {
		if cp, err = tw.s.CaptureObjects(ctx, objs, false); err != nil {
			return err
		}
		if !cp.Plan.ZeroDiff() {
			return fmt.Errorf("re-capture failed verification: %s", strings.Join(cp.Plan.Changes, "; "))
		}
	}

	var done []outcome
	for _, ev := range evs {
		d := capture.Discovered{Res: ev.res, Obj: ev.obj}
		addr := capture.AddressOf(d)
		ch := capture.Change{Address: addr}
		if ev.op != "removed" {
			ch.Resource, ch.Import = cp.Resources[addr], cp.Imports[addr]
			if ch.Resource == nil {
				return fmt.Errorf("%s: re-capture produced no resource", addr)
			}
		}
		before, err := capture.ApplyChanges(repo.Path(dir), []capture.Change{ch})
		if err != nil {
			return err
		}
		o := outcome{ev: ev, addr: addr, diff: blockDiff(before[addr], string(ch.Resource))}
		o.who, o.via, o.expected = tw.attribute(ev)
		verb := map[string]string{"added": "add", "changed": "update", "removed": "remove"}[ev.op]
		msg := fmt.Sprintf("observe %s: %s %s %s\n\n%s on %s, seen by scriven's watcher.\nChanged by: %s",
			t.Key(), verb, ev.res.Kind, objID(ev.obj), label(o.expected), t.Key(), o.who)
		committed, err := repo.Commit(ctx, dir, msg)
		if err != nil {
			return err
		}
		if !committed {
			w.logf("%s: %s %s already matches the library", t.Key(), ev.res.Kind, objID(ev.obj))
			continue
		}
		done = append(done, o)
	}
	if len(done) == 0 {
		return nil
	}
	if err := repo.PushObserved(ctx); err != nil {
		return err
	}
	pr := w.ensurePR(ctx)
	for _, o := range done {
		w.logf("%s: recorded %s (%s) by %s", t.Key(), o.addr, label(o.expected), o.who)
		w.sendChange(ctx, t, o, pr)
	}
	return nil
}

// attribute names who made a change and with what tool. The audit webhook
// gives the user; the object's managedFields give the tool.
func (tw *targetWatch) attribute(ev event) (who, via string, expected bool) {
	a, ok := tw.w.audit.Who(tw.s.Target.ID, ev.res.GVR.Resource, ev.obj.GetNamespace(), ev.obj.GetName(),
		ev.at.Add(-10*time.Minute))
	switch {
	case ok:
		// The audit event is the record of this exact write: user, tool and action.
		who = a.User
		via = agentName(a.UserAgent) + " " + a.Verb
		if a.Sub != "" {
			via += "/" + a.Sub
		}
		expected = strings.Contains(strings.ToLower(a.UserAgent), "terraform")
	default:
		who = "unknown user (no audit event received)"
		if tw.w.audit == nil {
			who = "unknown user (audit webhook not configured)"
		}
		// Without audit, the object's managedFields name the tool of the latest
		// timestamped write. A deleted object's fields describe an earlier write.
		if ev.op != "removed" {
			via = lastManager(ev.obj)
		}
		expected = strings.Contains(strings.ToLower(via), "terraform")
	}
	if via != "" {
		who += " via " + via
	}
	return who, via, expected
}

// lastManager is the field manager of the most recent write to the object.
func lastManager(u unstructured.Unstructured) string {
	var best string
	var bestTime time.Time
	for _, m := range u.GetManagedFields() {
		// Status writes come from controllers; "scale" is a person or an autoscaler.
		if m.Subresource == "status" || m.Time == nil {
			continue
		}
		if best == "" || m.Time.After(bestTime) {
			best, bestTime = m.Manager, m.Time.Time
		}
	}
	return best
}

func agentName(ua string) string {
	if i := strings.IndexAny(ua, "/ "); i > 0 {
		return ua[:i]
	}
	return ua
}

func label(expected bool) string {
	if expected {
		return "Expected change (applied by Terraform; library not yet updated)"
	}
	return "Drift (changed outside IaC)"
}

func objID(u unstructured.Unstructured) string {
	if ns := u.GetNamespace(); ns != "" {
		return ns + "/" + u.GetName()
	}
	return u.GetName()
}

func (w *Watcher) ensurePR(ctx context.Context) library.PR {
	pr, ok, err := w.forge.FindOpenPR(ctx, library.ObservedBranch)
	if err == nil && ok {
		return pr
	}
	if err != nil && err != library.ErrNoAPI {
		w.logf("could not look up the observed PR: %v", err)
	}
	pr, err = w.forge.OpenPR(ctx, library.ObservedBranch, w.cfg.Library.MainBranch(),
		"Observed changes (drift)",
		"Changes scriven's watcher saw in the live environments, one commit per resource.\n\n"+
			"Merge to accept them into the library, or revert them in the cluster and close this PR.\n"+
			"Each commit message says what changed and who changed it.")
	if err != nil {
		if err != library.ErrNoAPI {
			w.logf("could not open the observed PR: %v", err)
		}
		return library.PR{}
	}
	w.logf("opened PR #%d: %s", pr.Number, pr.URL)
	return pr
}

func (w *Watcher) sendChange(ctx context.Context, t config.Target, o outcome, pr library.PR) {
	what := map[string]string{"added": "new", "changed": "changed", "removed": "removed"}[o.ev.op]
	title := fmt.Sprintf("Drift on %s: %s %s %s", t.Key(), o.ev.res.Kind, objID(o.ev.obj), what)
	if o.expected {
		title = fmt.Sprintf("Terraform change on %s: %s %s %s", t.Key(), o.ev.res.Kind, objID(o.ev.obj), what)
	}
	err := w.notify.Send(ctx, notify.Message{
		Title: title,
		Text:  label(o.expected) + ". Recorded in the library's observed branch for review.",
		Facts: map[string]string{
			"environment": t.Env,
			"resource":    o.addr,
			"changed by":  o.who,
			"seen at":     o.ev.at.Format(time.RFC1123),
		},
		Link:   pr.URL,
		Detail: o.diff,
	})
	if err != nil {
		w.logf("notification failed: %v", err)
	}
}

// mux serves the audit webhook and a health check for Kubernetes probes.
func (w *Watcher) mux() http.Handler {
	m := http.NewServeMux()
	m.Handle("/audit/", w.audit)
	m.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("ok\n"))
	})
	return m
}
