// Package watch is `scriven watch` and `scriven sweep`: follow each cluster's
// change feed, re-capture only what changed, commit it to the library's
// observed branch, and tell people what changed and who changed it.
package watch

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AuditStore keeps recent write events that Kubernetes API servers push to
// scriven's audit webhook (POST /audit/<target-id>). It answers "who made
// this change?" without scriven needing access to nodes or log files.
type AuditStore struct {
	mu     sync.Mutex
	events []AuditEvent
	max    int
}

type AuditEvent struct {
	Target    string
	Time      time.Time
	User      string
	UserAgent string
	Verb      string
	Resource  string // plural, e.g. "deployments"
	Sub       string // subresource, e.g. "scale"
	Namespace string
	Name      string
}

func NewAuditStore() *AuditStore { return &AuditStore{max: 5000} }

var writeVerbs = map[string]bool{"create": true, "update": true, "patch": true, "delete": true}

// ServeHTTP accepts an audit.k8s.io/v1 EventList.
func (s *AuditStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/audit/") {
		http.NotFound(w, r)
		return
	}
	target := strings.TrimPrefix(r.URL.Path, "/audit/")
	var list struct {
		Items []struct {
			Stage                    string    `json:"stage"`
			Verb                     string    `json:"verb"`
			UserAgent                string    `json:"userAgent"`
			StageTimestamp           time.Time `json:"stageTimestamp"`
			RequestReceivedTimestamp time.Time `json:"requestReceivedTimestamp"`
			User                     struct {
				Username string `json:"username"`
			} `json:"user"`
			ImpersonatedUser *struct {
				Username string `json:"username"`
			} `json:"impersonatedUser"`
			ObjectRef *struct {
				Resource    string `json:"resource"`
				Namespace   string `json:"namespace"`
				Name        string `json:"name"`
				Subresource string `json:"subresource"`
			} `json:"objectRef"`
			ResponseStatus *struct {
				Code int `json:"code"`
			} `json:"responseStatus"`
		} `json:"items"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil || json.Unmarshal(body, &list) != nil {
		http.Error(w, "bad audit payload", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range list.Items {
		ref := e.ObjectRef
		if e.Stage != "ResponseComplete" || ref == nil || !writeVerbs[e.Verb] || ref.Name == "" {
			continue
		}
		if ref.Subresource != "" && ref.Subresource != "scale" {
			continue // status updates come from controllers, not people
		}
		if e.ResponseStatus != nil && e.ResponseStatus.Code >= 300 {
			continue
		}
		user := e.User.Username
		if e.ImpersonatedUser != nil && e.ImpersonatedUser.Username != "" {
			user = e.ImpersonatedUser.Username + " (impersonated by " + user + ")"
		}
		ts := e.StageTimestamp
		if ts.IsZero() {
			ts = e.RequestReceivedTimestamp
		}
		s.events = append(s.events, AuditEvent{Target: target, Time: ts, User: user, UserAgent: e.UserAgent,
			Verb: e.Verb, Resource: ref.Resource, Sub: ref.Subresource, Namespace: ref.Namespace, Name: ref.Name})
	}
	if n := len(s.events); n > s.max {
		s.events = append([]AuditEvent(nil), s.events[n-s.max:]...)
	}
	w.WriteHeader(http.StatusOK)
}

// Who returns the most recent write to an object since a given time.
func (s *AuditStore) Who(target, resource, namespace, name string, since time.Time) (AuditEvent, bool) {
	if s == nil {
		return AuditEvent{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.events) - 1; i >= 0; i-- {
		e := s.events[i]
		if e.Time.Before(since) {
			continue // batches can arrive out of order
		}
		if e.Target == target && e.Resource == resource && e.Namespace == namespace && e.Name == name {
			return e, true
		}
	}
	return AuditEvent{}, false
}
