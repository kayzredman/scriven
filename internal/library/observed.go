package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"scriven/internal/config"
)

// ObservedBranch holds what the watcher saw for environments that merge into
// main. It is merged into main by PR after review, and restarted from main
// once that happens. Environments with their own base branch get their own
// observed branch (see ObservedBranchFor).
const ObservedBranch = "observed"

// ObservedBranchFor names the observed branch for a base branch: "observed"
// for main, "observed-<base>" for any other.
func ObservedBranchFor(cfg config.Library, base string) string {
	if base == "" || base == cfg.MainBranch() {
		return ObservedBranch
	}
	return ObservedBranch + "-" + base
}

// OpenObserved clones the library with history and checks out the observed
// branch for base: it continues the branch while it has unmerged commits, and
// starts it again from base once those have been merged.
func OpenObserved(ctx context.Context, cfg config.Library, token, base string) (*Repo, error) {
	if base == "" {
		base = cfg.MainBranch()
	}
	dir, err := os.MkdirTemp("", "scriven-library-")
	if err != nil {
		return nil, err
	}
	obs := ObservedBranchFor(cfg, base)
	r := &Repo{cfg: cfg, token: token, observed: obs}
	if _, err := r.git(ctx, "clone", "--branch", base, cfg.URL, dir); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	r.dir = dir
	from := "origin/" + base
	if _, err := r.git(ctx, "fetch", "origin", obs+":refs/remotes/origin/"+obs); err != nil {
		// No observed branch yet: start it from base.
		_, err = r.git(ctx, "checkout", "-q", "-b", obs, from)
		return r, err
	}
	if _, err := r.git(ctx, "merge-base", "--is-ancestor", "origin/"+obs, from); err == nil {
		// Everything observed so far is in base: start again from it (a fast-forward).
		_, err = r.git(ctx, "checkout", "-q", "-B", obs, from)
		return r, err
	}
	if _, err := r.git(ctx, "checkout", "-q", "-B", obs, "origin/"+obs); err != nil {
		return nil, err
	}
	// Keep observed current with base (e.g. a capture PR merged meanwhile).
	if _, err := r.git(ctx, "merge", "-q", "--no-edit", from); err != nil {
		// Conflicting edits to the same files: base wins, and the next sweep
		// re-detects anything that still differs from the live environment.
		_, _ = r.git(ctx, "merge", "--abort")
		if _, err := r.git(ctx, "checkout", "-q", "-B", obs, from); err != nil {
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
		if !bookkeeping(f) {
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

// PushObserved pushes the observed branch (forced only after a reset to its base).
func (r *Repo) PushObserved(ctx context.Context) error {
	obs := r.observed
	if obs == "" {
		obs = ObservedBranch
	}
	args := []string{"push", "-q", "origin", "HEAD:refs/heads/" + obs}
	if r.force {
		args = append(args, "--force")
	}
	_, err := r.git(ctx, args...)
	return err
}
