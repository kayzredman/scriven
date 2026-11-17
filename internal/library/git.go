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
	cfg      config.Library
	token    string
	dir      string
	force    bool   // observed branch was reset to its base: next push must be forced
	observed string // name of the observed branch checked out, if any
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

// Clone makes a shallow working copy of the main branch in a temp directory.
// Call Close when done.
func Clone(ctx context.Context, cfg config.Library, token string) (*Repo, error) {
	return CloneBase(ctx, cfg, token, cfg.MainBranch())
}

// CloneBase makes a shallow working copy of base, the branch an environment's
// captures merge into. A base other than main that does not exist yet is
// created, empty apart from a README, so it holds only that environment's code.
func CloneBase(ctx context.Context, cfg config.Library, token, base string) (*Repo, error) {
	dir, err := os.MkdirTemp("", "scriven-library-")
	if err != nil {
		return nil, err
	}
	r := &Repo{cfg: cfg, token: token}
	_, err = r.git(ctx, "clone", "--depth=1", "--branch", base, cfg.URL, dir)
	if err == nil {
		r.dir = dir
		return r, nil
	}
	if base == cfg.MainBranch() {
		os.RemoveAll(dir)
		return nil, err
	}
	if out, lerr := r.git(ctx, "ls-remote", "--heads", cfg.URL, base); lerr != nil || strings.TrimSpace(out) != "" {
		os.RemoveAll(dir)
		return nil, err // the branch exists: the clone failed for another reason
	}
	if err := r.createBase(ctx, dir, base); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("creating library branch %s: %w", base, err)
	}
	return r, nil
}

func (r *Repo) createBase(ctx context.Context, dir, base string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { // a failed clone may have removed it
		return err
	}
	if _, err := r.git(ctx, "clone", "--depth=1", "--branch", r.cfg.MainBranch(), r.cfg.URL, dir); err != nil {
		return err
	}
	r.dir = dir
	if _, err := r.git(ctx, "checkout", "-q", "--orphan", base); err != nil {
		return err
	}
	if _, err := r.git(ctx, "rm", "-r", "-q", "-f", "--", "."); err != nil {
		return err
	}
	readme := fmt.Sprintf("# IaC library: %s\n\nThis branch holds the captures of the environments whose `merge_policy` names\n"+
		"`branch: %s`. Scriven merges them here without approval; environments that need review\nmerge into `%s`.\n",
		base, base, r.cfg.MainBranch())
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	if _, err := r.git(ctx, "add", "README.md"); err != nil {
		return err
	}
	if _, err := r.git(ctx, "commit", "-q", "-m", "start library branch "+base); err != nil {
		return err
	}
	_, err := r.git(ctx, "push", "-q", "origin", "HEAD:refs/heads/"+base)
	return err
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
		if !bookkeeping(f) {
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

// bookkeeping reports files that differ between runs without the environment
// having changed: catalog.yaml carries a timestamp, and Terraform's lock file
// lists provider hashes for the platform scriven ran on (Windows vs Linux).
// A difference in only these is not a change worth a commit or a notification.
func bookkeeping(path string) bool {
	return strings.HasSuffix(path, "/catalog.yaml") || strings.HasSuffix(path, "/.terraform.lock.hcl")
}
