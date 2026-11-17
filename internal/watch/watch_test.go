package watch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBlockDiff(t *testing.T) {
	before := "resource \"x\" \"y\" {\n  a = 1\n  b = 2\n  c = 3\n  d = 4\n  e = 5\n  f = 6\n}"
	after := strings.Replace(before, "b = 2", "b = 20", 1)
	d := blockDiff(before, after)
	if !strings.Contains(d, "-   b = 2") || !strings.Contains(d, "+   b = 20") {
		t.Fatalf("diff:\n%s", d)
	}
	if strings.Contains(d, "f = 6") {
		t.Errorf("context should be limited to 2 lines:\n%s", d)
	}
	if got := blockDiff("", "new"); got != "+ new" {
		t.Errorf("added: %q", got)
	}
	if got := blockDiff("old", ""); got != "- old" {
		t.Errorf("removed: %q", got)
	}
}

func TestAuditStore(t *testing.T) {
	s := NewAuditStore()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	body := `{"kind":"EventList","items":[
	 {"stage":"ResponseComplete","verb":"patch","userAgent":"kubectl/v1.36","stageTimestamp":"` + now + `",
	  "user":{"username":"alice"},"objectRef":{"resource":"deployments","namespace":"shop","name":"web","subresource":"scale"},
	  "responseStatus":{"code":200}},
	 {"stage":"ResponseComplete","verb":"update","stageTimestamp":"` + now + `","user":{"username":"system:serviceaccount:kube-system:deployment-controller"},
	  "objectRef":{"resource":"deployments","namespace":"shop","name":"web","subresource":"status"},"responseStatus":{"code":200}},
	 {"stage":"ResponseComplete","verb":"delete","stageTimestamp":"` + now + `","user":{"username":"bob"},
	  "objectRef":{"resource":"services","namespace":"shop","name":"rogue"},"responseStatus":{"code":403}}]}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/audit/prod-sim", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	since := time.Now().Add(-time.Minute)
	if e, ok := s.Who("prod-sim", "deployments", "shop", "web", since); !ok || e.User != "alice" {
		t.Errorf("scale by alice not found (status writes must be ignored): %+v %v", e, ok)
	}
	if _, ok := s.Who("prod-sim", "services", "shop", "rogue", since); ok {
		t.Error("a refused (403) delete must not be attributed")
	}
	if _, ok := s.Who("dev-sim", "deployments", "shop", "web", since); ok {
		t.Error("events are per target")
	}
}

func TestLastManagerSkipsStatusAndUntimed(t *testing.T) {
	at := func(m int) *metav1.Time { x := metav1.NewTime(time.Unix(int64(m), 0)); return &x }
	u := unstructured.Unstructured{Object: map[string]any{}}
	u.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "kubectl-client-side-apply", Time: at(1)},
		{Manager: "kubectl-scale", Subresource: "scale", Time: at(2)},
		{Manager: "kubectl.exe", Subresource: "scale"}, // the API server leaves scale writes untimed
		{Manager: "k3s", Subresource: "status", Time: at(3)},
	})
	if got := lastManager(u); got != "kubectl-scale" {
		t.Errorf("lastManager = %q", got)
	}
}

func TestMetrics(t *testing.T) {
	m := NewMetrics()
	m.Add("scriven_changes_recorded_total", 1, "target", "rancher/prod", "label", "drift")
	m.Add("scriven_changes_recorded_total", 1, "target", "rancher/prod", "label", "drift")
	m.sweep("aws/shop", time.Now().Add(-90*time.Second), 3, nil)
	m.sweep("aws/shop", time.Now(), 0, errBoom)
	var nilM *Metrics
	nilM.Add("scriven_up", 1) // must not panic

	srv := httptest.NewServer(m)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	out := sb.String()
	for _, want := range []string{
		"# TYPE scriven_changes_recorded_total counter",
		`scriven_changes_recorded_total{target="rancher/prod",label="drift"} 2`,
		`scriven_sweeps_total{target="aws/shop",result="drift"} 1`,
		`scriven_sweeps_total{target="aws/shop",result="error"} 1`,
		`scriven_sweep_recorded_resources{target="aws/shop"} 3`, // the failed sweep keeps the last good value
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "e+") {
		t.Errorf("values should be plain numbers:\n%s", out)
	}
}

var errBoom = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }
