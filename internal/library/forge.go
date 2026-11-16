package library

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"scriven/internal/config"
)

// Forge is the Git host's API: pull requests and branch protection.
type Forge interface {
	OpenPR(ctx context.Context, head, base, title, body string) (PR, error)
	Protection(ctx context.Context, branch string) (Protection, error)
	Merge(ctx context.Context, pr PR) error
	// Supersede closes open PRs whose branch starts with prefix, except keep, and
	// returns their numbers. Used so only the newest capture of a target is open.
	Supersede(ctx context.Context, prefix string, keep PR) ([]int, error)
	// FindOpenPR returns the open PR from branch head, if there is one.
	FindOpenPR(ctx context.Context, head string) (PR, bool, error)
}

type PR struct {
	Number int
	URL    string
}

type Protection struct {
	Protected         bool
	RequiredApprovals int
	DirectPushAllowed bool
}

// ErrNoAPI means the provider is plain Git: branches are pushed, but PRs and
// branch protection have to be handled by people.
var ErrNoAPI = fmt.Errorf("provider \"git\" has no API: open the PR by hand")

func NewForge(cfg config.Library, token string) (Forge, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("library.url: %w", err)
	}
	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("library.url: expected https://host/<owner>/<repo>, got %s", cfg.URL)
	}
	h := &httpAPI{token: token, client: &http.Client{Timeout: 15 * time.Second}}
	base := cfg.APIURL
	switch cfg.Provider {
	case "gitea":
		if base == "" {
			base = fmt.Sprintf("%s://%s/api/v1", u.Scheme, u.Host)
		}
		h.base, h.auth = base, "token "+token
		return &gitea{h, parts[0], strings.Join(parts[1:], "/")}, nil
	case "github":
		if base == "" {
			base = "https://api.github.com"
			if u.Host != "github.com" {
				base = fmt.Sprintf("%s://%s/api/v3", u.Scheme, u.Host)
			}
		}
		h.base, h.auth = base, "Bearer "+token
		return &github{h, parts[0], parts[1]}, nil
	case "gitlab":
		if base == "" {
			base = fmt.Sprintf("%s://%s/api/v4", u.Scheme, u.Host)
		}
		h.base, h.header = base, "PRIVATE-TOKEN"
		return &gitlab{h, url.PathEscape(path)}, nil
	case "git":
		return noAPI{}, nil
	}
	return nil, fmt.Errorf("library.provider: unknown %q", cfg.Provider)
}

type httpAPI struct {
	base, auth, header, token string
	client                    *http.Client
}

func (h *httpAPI) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		return 0, err
	}
	if h.header != "" {
		req.Header.Set(h.header, h.token)
	} else {
		req.Header.Set("Authorization", h.auth)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// --- Gitea (tested against the lab) ---

type gitea struct {
	*httpAPI
	owner, repo string
}

func (g *gitea) OpenPR(ctx context.Context, head, base, title, body string) (PR, error) {
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	_, err := g.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/pulls", g.owner, g.repo),
		map[string]any{"head": head, "base": base, "title": title, "body": body}, &out)
	return PR{out.Number, out.HTMLURL}, err
}

func (g *gitea) Protection(ctx context.Context, branch string) (Protection, error) {
	var out struct {
		EnablePush        bool `json:"enable_push"`
		RequiredApprovals int  `json:"required_approvals"`
	}
	code, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/branch_protections/%s", g.owner, g.repo, branch), nil, &out)
	if code == 404 {
		return Protection{DirectPushAllowed: true}, nil
	}
	if err != nil {
		return Protection{}, err
	}
	return Protection{Protected: true, RequiredApprovals: out.RequiredApprovals, DirectPushAllowed: out.EnablePush}, nil
}

func (g *gitea) Merge(ctx context.Context, pr PR) error {
	_, err := g.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", g.owner, g.repo, pr.Number),
		map[string]any{"Do": "merge", "delete_branch_after_merge": true}, nil)
	return err
}

// --- GitHub (not yet exercised against a real repo) ---

type github struct {
	*httpAPI
	owner, repo string
}

func (g *github) OpenPR(ctx context.Context, head, base, title, body string) (PR, error) {
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	_, err := g.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/pulls", g.owner, g.repo),
		map[string]any{"head": head, "base": base, "title": title, "body": body}, &out)
	return PR{out.Number, out.HTMLURL}, err
}

func (g *github) Protection(ctx context.Context, branch string) (Protection, error) {
	var out struct {
		Reviews *struct {
			Count int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
	}
	code, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/branches/%s/protection", g.owner, g.repo, branch), nil, &out)
	if code == 404 {
		return Protection{DirectPushAllowed: true}, nil
	}
	if err != nil {
		return Protection{}, err
	}
	p := Protection{Protected: true, DirectPushAllowed: out.Reviews == nil}
	if out.Reviews != nil {
		p.RequiredApprovals = out.Reviews.Count
	}
	return p, nil
}

func (g *github) Merge(ctx context.Context, pr PR) error {
	_, err := g.do(ctx, "PUT", fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", g.owner, g.repo, pr.Number), map[string]any{}, nil)
	return err
}

// --- GitLab (not yet exercised against a real repo) ---

type gitlab struct {
	*httpAPI
	project string
}

func (g *gitlab) OpenPR(ctx context.Context, head, base, title, body string) (PR, error) {
	var out struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	_, err := g.do(ctx, "POST", "/projects/"+g.project+"/merge_requests",
		map[string]any{"source_branch": head, "target_branch": base, "title": title, "description": body,
			"remove_source_branch": true}, &out)
	return PR{out.IID, out.WebURL}, err
}

func (g *gitlab) Protection(ctx context.Context, branch string) (Protection, error) {
	var pb struct {
		PushAccess []struct {
			Level int `json:"access_level"`
		} `json:"push_access_levels"`
	}
	code, err := g.do(ctx, "GET", "/projects/"+g.project+"/protected_branches/"+url.PathEscape(branch), nil, &pb)
	if code == 404 {
		return Protection{DirectPushAllowed: true}, nil
	}
	if err != nil {
		return Protection{}, err
	}
	p := Protection{Protected: true}
	for _, a := range pb.PushAccess {
		if a.Level > 0 {
			p.DirectPushAllowed = true
		}
	}
	var ap struct {
		Approvals int `json:"approvals_before_merge"`
	}
	if _, err := g.do(ctx, "GET", "/projects/"+g.project+"/approvals", nil, &ap); err == nil {
		p.RequiredApprovals = ap.Approvals
	}
	return p, nil
}

func (g *gitlab) Merge(ctx context.Context, pr PR) error {
	_, err := g.do(ctx, "PUT", fmt.Sprintf("/projects/%s/merge_requests/%d/merge", g.project, pr.Number), map[string]any{}, nil)
	return err
}

type noAPI struct{}

func (noAPI) OpenPR(context.Context, string, string, string, string) (PR, error) {
	return PR{}, ErrNoAPI
}
func (noAPI) Protection(context.Context, string) (Protection, error) { return Protection{}, ErrNoAPI }
func (noAPI) Merge(context.Context, PR) error                        { return ErrNoAPI }

func (g *gitea) Supersede(ctx context.Context, prefix string, keep PR) ([]int, error) {
	var open []struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/pulls?state=open&limit=50", g.owner, g.repo), nil, &open); err != nil {
		return nil, err
	}
	var closed []int
	for _, p := range open {
		if p.Number == keep.Number || !strings.HasPrefix(p.Head.Ref, prefix) {
			continue
		}
		_, _ = g.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", g.owner, g.repo, p.Number),
			map[string]any{"body": fmt.Sprintf("Superseded by #%d, a newer capture of the same target.", keep.Number)}, nil)
		if _, err := g.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/%s/pulls/%d", g.owner, g.repo, p.Number),
			map[string]any{"state": "closed"}, nil); err != nil {
			return closed, err
		}
		_, _ = g.do(ctx, "DELETE", fmt.Sprintf("/repos/%s/%s/branches/%s", g.owner, g.repo, url.PathEscape(p.Head.Ref)), nil, nil)
		closed = append(closed, p.Number)
	}
	return closed, nil
}

func (g *github) Supersede(ctx context.Context, prefix string, keep PR) ([]int, error) {
	var open []struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=100", g.owner, g.repo), nil, &open); err != nil {
		return nil, err
	}
	var closed []int
	for _, p := range open {
		if p.Number == keep.Number || !strings.HasPrefix(p.Head.Ref, prefix) {
			continue
		}
		_, _ = g.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", g.owner, g.repo, p.Number),
			map[string]any{"body": fmt.Sprintf("Superseded by #%d, a newer capture of the same target.", keep.Number)}, nil)
		if _, err := g.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/%s/pulls/%d", g.owner, g.repo, p.Number),
			map[string]any{"state": "closed"}, nil); err != nil {
			return closed, err
		}
		closed = append(closed, p.Number)
	}
	return closed, nil
}

func (g *gitlab) Supersede(ctx context.Context, prefix string, keep PR) ([]int, error) {
	var open []struct {
		IID          int    `json:"iid"`
		SourceBranch string `json:"source_branch"`
	}
	if _, err := g.do(ctx, "GET", "/projects/"+g.project+"/merge_requests?state=opened&per_page=100", nil, &open); err != nil {
		return nil, err
	}
	var closed []int
	for _, p := range open {
		if p.IID == keep.Number || !strings.HasPrefix(p.SourceBranch, prefix) {
			continue
		}
		_, _ = g.do(ctx, "POST", fmt.Sprintf("/projects/%s/merge_requests/%d/notes", g.project, p.IID),
			map[string]any{"body": fmt.Sprintf("Superseded by !%d, a newer capture of the same target.", keep.Number)}, nil)
		if _, err := g.do(ctx, "PUT", fmt.Sprintf("/projects/%s/merge_requests/%d", g.project, p.IID),
			map[string]any{"state_event": "close"}, nil); err != nil {
			return closed, err
		}
		closed = append(closed, p.IID)
	}
	return closed, nil
}

func (noAPI) Supersede(context.Context, string, PR) ([]int, error) { return nil, ErrNoAPI }

func (g *gitea) FindOpenPR(ctx context.Context, head string) (PR, bool, error) {
	var open []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/pulls?state=open&limit=50", g.owner, g.repo), nil, &open); err != nil {
		return PR{}, false, err
	}
	for _, p := range open {
		if p.Head.Ref == head {
			return PR{p.Number, p.HTMLURL}, true, nil
		}
	}
	return PR{}, false, nil
}

func (g *github) FindOpenPR(ctx context.Context, head string) (PR, bool, error) {
	var open []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if _, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/pulls?state=open&head=%s:%s", g.owner, g.repo, g.owner, url.QueryEscape(head)), nil, &open); err != nil {
		return PR{}, false, err
	}
	if len(open) == 0 {
		return PR{}, false, nil
	}
	return PR{open[0].Number, open[0].HTMLURL}, true, nil
}

func (g *gitlab) FindOpenPR(ctx context.Context, head string) (PR, bool, error) {
	var open []struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if _, err := g.do(ctx, "GET", "/projects/"+g.project+"/merge_requests?state=opened&source_branch="+url.QueryEscape(head), nil, &open); err != nil {
		return PR{}, false, err
	}
	if len(open) == 0 {
		return PR{}, false, nil
	}
	return PR{open[0].IID, open[0].WebURL}, true, nil
}

func (noAPI) FindOpenPR(context.Context, string) (PR, bool, error) { return PR{}, false, ErrNoAPI }
