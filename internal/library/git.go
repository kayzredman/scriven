// Package library keeps captures in the Git-backed IaC library. It is the only
// state scriven has. It uses the git CLI; the token is passed per command as an
// HTTP header and is never written to disk.
package library

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"scriven/internal/config"
)

type Repo struct {
	cfg   config.Library
	token string
	dir   string
}

func defaultUser(provider string) string {
	switch provider {
	case "github":
		return "x-access-token"
	case "gitlab":
		return "oauth2"
	}
	return "scriven"
}

func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	full := []string{"-c", "credential.helper=", "-c", "core.autocrlf=false"}
	if r.token != "" {
		user := r.cfg.Username
		if user == "" {
			user = defaultUser(r.cfg.Provider)
		}
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + r.token))
		full = append(full, "-c", "http.extraHeader=Authorization: Basic "+auth)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=scriven", "GIT_AUTHOR_EMAIL=scriven@localhost",
		"GIT_COMMITTER_NAME=scriven", "GIT_COMMITTER_EMAIL=scriven@localhost")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		msg := strings.ReplaceAll(out.String(), r.token, "***")
		return msg, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(msg))
	}
	return out.String(), nil
}

// Clone makes a shallow working copy in a temp directory. Call Close when done.
func Clone(ctx context.Context, cfg config.Library, token string) (*Repo, error) {
	dir, err := os.MkdirTemp("", "scriven-library-")
	if err != nil {
		return nil, err
	}
	r := &Repo{cfg: cfg, token: token}
	if _, err := r.git(ctx, "clone", "--depth=1", "--branch", cfg.MainBranch(), cfg.URL, dir); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	r.dir = dir
	return r, nil
}

func (r *Repo) Close() { os.RemoveAll(r.dir) }

// TestAccess proves scriven can clone and push, using a scratch branch it deletes again.
func TestAccess(ctx context.Context, cfg config.Library, token string) error {
	r, err := Clone(ctx, cfg, token)
	if err != nil {
		return err
	}
	defer r.Close()
	branch := fmt.Sprintf("scriven-access-test-%d", time.Now().Unix())
	if _, err := r.git(ctx, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push to a scratch branch failed: %w", err)
	}
	if _, err := r.git(ctx, "push", "origin", "--delete", branch); err != nil {
		return fmt.Errorf("pushed, but could not delete scratch branch %s: %w", branch, err)
	}
	return nil
}

// CommitFiles writes files under dir on a new branch, commits and pushes it.
// It returns false when the files are identical to what main already holds.
func (r *Repo) CommitFiles(ctx context.Context, branch, dir string, files map[string][]byte, message string) (bool, error) {
	if _, err := r.git(ctx, "checkout", "-q", "-b", branch); err != nil {
		return false, err
	}
	target := filepath.Join(r.dir, filepath.FromSlash(dir))
	// Replace the directory wholesale so resources removed from the cluster disappear too.
	if err := os.RemoveAll(target); err != nil {
		return false, err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return false, err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(target, name), b, 0o644); err != nil {
			return false, err
		}
	}
	if _, err := r.git(ctx, "add", "-A", "--", dir); err != nil {
		return false, err
	}
	// Ignore catalog.yaml's timestamp when deciding whether anything changed.
	out, err := r.git(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		return false, err
	}
	changed := false
	for _, f := range strings.Fields(out) {
		if !strings.HasSuffix(f, "/catalog.yaml") {
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if _, err := r.git(ctx, "commit", "-q", "-m", message); err != nil {
		return false, err
	}
	if _, err := r.git(ctx, "push", "-q", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return false, err
	}
	return true, nil
}

// DiffStat summarises what the last commit changed, for notifications.
func (r *Repo) DiffStat(ctx context.Context) string {
	out, err := r.git(ctx, "show", "--stat", "--format=", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
