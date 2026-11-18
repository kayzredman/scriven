package library

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"scriven/internal/config"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@localhost",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, r *Repo, rel, content string) {
	t.Helper()
	p := r.Path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two scriven instances open the observed branch at the same time. The one
// that pushes second replays its commits on top instead of failing; a commit
// with the same change as the other's drops out.
func TestPushObservedAfterAnotherWriter(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	run(t, root, "init", "-q", "--bare", "-b", "main", remote)
	run(t, root, "init", "-q", "-b", "main", seed)
	os.MkdirAll(filepath.Join(seed, "envs/dev"), 0o755)
	os.WriteFile(filepath.Join(seed, "envs/dev/a.tf"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(seed, "envs/dev/b.tf"), []byte("b\n"), 0o644)
	run(t, seed, "add", "-A")
	run(t, seed, "commit", "-q", "-m", "baseline")
	run(t, seed, "push", "-q", remote, "main")

	cfg := config.Library{Provider: "git", URL: remote}
	one, err := OpenObserved(ctx, cfg, "", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := OpenObserved(ctx, cfg, "", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()

	// Both see the same new file; only the second also sees a change to a.tf.
	write(t, one, "envs/dev/hpa.tf", "hpa\n")
	if ok, err := one.Commit(ctx, "envs/dev", "add hpa"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := one.PushObserved(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, two, "envs/dev/hpa.tf", "hpa\n")
	if _, err := two.Commit(ctx, "envs/dev", "add hpa"); err != nil {
		t.Fatal(err)
	}
	write(t, two, "envs/dev/a.tf", "a, autoscaled\n")
	if _, err := two.Commit(ctx, "envs/dev", "change a"); err != nil {
		t.Fatal(err)
	}
	if err := two.PushObserved(ctx); err != nil {
		t.Fatalf("second push: %v", err)
	}

	log := run(t, root, "--git-dir", remote, "log", "--format=%s", ObservedBranch)
	if got := strings.Fields(strings.ReplaceAll(log, " ", "_")); strings.Join(got, ",") != "change_a,add_hpa,baseline" {
		t.Fatalf("observed history = %v, want change a, add hpa (once), baseline", got)
	}
}
