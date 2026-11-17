package watch

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics is what `scriven watch` reports at /metrics, in the Prometheus text
// format. It is written by hand: a handful of counters and gauges do not need
// the client library and its dependencies.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]float64 // series ("name{labels}") -> value
	gauges   map[string]float64
}

func NewMetrics() *Metrics {
	return &Metrics{counters: map[string]float64{}, gauges: map[string]float64{}}
}

// metricHelp lists every metric scriven exports, with its type and help text.
var metricHelp = []struct{ name, typ, help string }{
	{"scriven_up", "gauge", "1 while scriven watch is running."},
	{"scriven_targets", "gauge", "Targets by how they are covered: mode=\"live\" (watched) or mode=\"sweep\" (sweeps only)."},
	{"scriven_target_watched", "gauge", "1 when the target's change feed is running, 0 when it could not be started."},
	{"scriven_changes_recorded_total", "counter", "Changes the watcher committed to the observed branch, by label (drift or expected)."},
	{"scriven_record_errors_total", "counter", "Changes the watcher saw but could not write to the library."},
	{"scriven_sweeps_total", "counter", "Sweeps by result: up_to_date, drift or error."},
	{"scriven_sweep_duration_seconds", "gauge", "How long the target's last sweep took."},
	{"scriven_last_sweep_timestamp_seconds", "gauge", "Unix time the target's last sweep finished, whatever its result."},
	{"scriven_last_successful_sweep_timestamp_seconds", "gauge", "Unix time of the target's last sweep that did not fail. Alert when this is old."},
	{"scriven_sweep_recorded_resources", "gauge", "Resources the target's last successful sweep recorded on the observed branch as new, changed or removed (0 when the library was up to date)."},
	{"scriven_audit_events_total", "counter", "Write events received on the audit webhook."},
	{"scriven_notifications_failed_total", "counter", "Notifications that could not be delivered to at least one channel."},
}

func series(name string, labels ...string) string {
	if len(labels) == 0 {
		return name
	}
	var parts []string
	for i := 0; i+1 < len(labels); i += 2 {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(labels[i+1])
		parts = append(parts, fmt.Sprintf(`%s="%s"`, labels[i], v))
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// Add increases a counter. A nil *Metrics ignores every call, so code paths
// that run without the watcher (scriven sweep on its own) need no checks.
func (m *Metrics) Add(name string, n float64, labels ...string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.counters[series(name, labels...)] += n
	m.mu.Unlock()
}

func (m *Metrics) Set(name string, v float64, labels ...string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.gauges[series(name, labels...)] = v
	m.mu.Unlock()
}

// sweep records the outcome of one sweep. drift is the number of resources
// found different; it is ignored when err is set.
func (m *Metrics) sweep(target string, started time.Time, drift int, err error) {
	if m == nil {
		return
	}
	result := "up_to_date"
	switch {
	case err != nil:
		result = "error"
	case drift > 0:
		result = "drift"
	}
	now := time.Now()
	m.Add("scriven_sweeps_total", 1, "target", target, "result", result)
	m.Set("scriven_sweep_duration_seconds", now.Sub(started).Seconds(), "target", target)
	m.Set("scriven_last_sweep_timestamp_seconds", float64(now.Unix()), "target", target)
	if err == nil {
		m.Set("scriven_last_successful_sweep_timestamp_seconds", float64(now.Unix()), "target", target)
		m.Set("scriven_sweep_recorded_resources", float64(drift), "target", target)
	}
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(m.render()))
}

func (m *Metrics) render() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var sb strings.Builder
	for _, h := range metricHelp {
		values := m.gauges
		if h.typ == "counter" {
			values = m.counters
		}
		var keys []string
		for k := range values {
			if k == h.name || strings.HasPrefix(k, h.name+"{") {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			continue
		}
		sort.Strings(keys)
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n", h.name, h.help, h.name, h.typ)
		for _, k := range keys {
			fmt.Fprintf(&sb, "%s %s\n", k, strconv.FormatFloat(values[k], 'f', -1, 64))
		}
	}
	return sb.String()
}
