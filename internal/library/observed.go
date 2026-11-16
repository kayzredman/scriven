package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"scriven/internal/config"
)

// ObservedBranch holds what the watcher saw. It is merged into main by PR
// after review, and restarted from main once that happens.
const ObservedBranch = "observed"

// OpenObserved clones the library with history and checks out the observed
// branch: it continues the branch while it has unmerged commits, and starts
// it again from main once those have been merged.
func OpenObserved(ctx context.Context, cfg config.Library, token string) (*Repo, error) {
	dir, err := os.MkdirTemp("", "scriven-library-")
	if err != nil {
		return nil, err
	}
	r := &Repo{cfg: cfg, token: token}
	if _, err := r.git(ctx, "clone", "--branch", cfg.MainBranch(), cfg.URL, dir); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	r.dir = dir
	main := "origin/" + cfg.MainBranch()
	if _, err := r.git(ctx, "fetch", "origin", ObservedBranch+":refs/remotes/origin/"+ObservedBranch); err != nil {
		// No observed branch yet: start it from main.
		_, err = r.git(ctx, "checkout", "-q", "-b", ObservedBranch, main)
		return r, err
	}
	if _, err := r.git(ctx, "merge-base", "--is-ancestor", "origin/"+ObservedBranch, main); err == nil {
		// Everything observed so far is in main: start again from main (a fast-forward).
		_, err = r.git(ctx, "checkout", "-q", "-B", ObservedBranch, main)
		return r, err
	}
	if _, err := r.git(ctx, "checkout", "-q", "-B", ObservedBranch, "origin/"+ObservedBranch); err != nil {
		return nil, err
	}
	// Keep observed current with main (e.g. a capture PR merged meanwhile).
	if _, err := r.git(ctx, "merge", "-q", "--no-edit", main); err != nil {
		// Conflicting edits to the same files: main wins, and the next sweep
		// re-detects anything that still differs from the cluster.
		_, _ = r.git(ctx, "merge", "--abort")
		if _, err := r.git(ctx, "checkout", "-q", "-B", ObservedBranch, main); err != nil {
			return nil, err
		}
		r.force = true
	}
	return r, nil
}

// Path is a path inside the working copy.
func (r *Repo) Path(rel string) string { return filepath.Join(r.dir, filepath.FromSlash(rel)) }

// Exists reports whether a path exists in the working copy.
func (r *Repo) Exists(rel string) bool {
	_, err := os.Stat(r.Path(rel))
	return err == nil
}

// Commit stages dir and commits it. It returns false when nothing changed
// (apart from catalog.yaml timestamps).
func (r *Repo) Commit(ctx context.Context, dir, message string) (bool, error) {
	if _, err := r.git(ctx, "add", "-A", "--", dir); err != nil {
		return false, err
	}
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
		_, _ = r.git(ctx, "reset", "-q", "HEAD", "--", dir)
		_, _ = r.git(ctx, "checkout", "-q", "--", dir)
		return false, nil
	}
	_, err = r.git(ctx, "commit", "-q", "-m", message)
	return err == nil, err
}

// PushObserved pushes the observed branch (forced only after a reset to main).
func (r *Repo) PushObserved(ctx context.Context) error {
	args := []string{"push", "-q", "origin", "HEAD:refs/heads/" + ObservedBranch}
	if r.force {
		args = append(args, "--force")
	}
	_, err := r.git(ctx, args...)
	return err
}
