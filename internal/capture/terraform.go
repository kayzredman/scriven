package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// tf runs the Terraform CLI in a working directory. Captures are always
// verified in a scratch directory, never inside the library checkout.
type tf struct {
	dir string
	env []string
}

func (t *tf) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "terraform", args...)
	cmd.Dir = t.dir
	cmd.Env = append(os.Environ(), t.env...)
	cmd.Env = append(cmd.Env, "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("terraform %s: %w\n%s", args[0], err, tail(out.String(), 40))
	}
	return out.String(), nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"..."}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

func TerraformVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "terraform", "version", "-json").Output()
	if err != nil {
		return "", fmt.Errorf("terraform not found on PATH: %w", err)
	}
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

var lockVersion = regexp.MustCompile(`(?s)provider "registry.terraform.io/([^"]+)" \{\s*version\s*=\s*"([^"]+)"`)

// lockedProviders reads the exact provider versions `terraform init` selected.
func lockedProviders(dir string) map[string]string {
	b, err := os.ReadFile(filepath.Join(dir, ".terraform.lock.hcl"))
	out := map[string]string{}
	if err != nil {
		return out
	}
	for _, m := range lockVersion.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = m[2]
	}
	return out
}

// PlanResult is the outcome of the zero-diff check.
type PlanResult struct {
	Imports  int
	NoOps    int
	Changes  []string // "<address>: <actions>" for anything that is not a no-op
	Rendered string   // human-readable plan, kept when there are changes
	// Updates that only touch Terraform-side settings (e.g. wait_for_rollout),
	// which have no counterpart in the cluster. Recorded, not counted as drift.
	ProviderOnly []string
}

// Attributes of hashicorp/kubernetes resources that control how Terraform
// waits or applies, not what is deployed. Import leaves them unset in state,
// so the first plan always "sets" them; that changes nothing in the cluster.
var providerOnlyAttrs = map[string]bool{
	"wait_for_rollout": true, "wait_for_load_balancer": true, "wait_for_default_service_account": true,
	"wait_for_completion": true, "wait_for": true, "timeouts": true, "field_manager": true,
}

func (p PlanResult) ZeroDiff() bool { return len(p.Changes) == 0 }

// kubernetesNote explains a planned difference that does not change the
// cluster, or returns "" when the difference is real.
func kubernetesNote(byAddr map[string]Discovered) func(string, leafDiff) string {
	return func(addr string, df leafDiff) string {
		if providerOnlyAttrs[df.path[0]] {
			return df.path[0] + " (Terraform-only setting)"
		}
		if d, ok := byAddr[addr]; ok && quirkDiff(d, df.path, df.before, df.after) {
			return df.path[len(df.path)-1] + " (unset in cluster = true; provider reads false)"
		}
		return ""
	}
}

// planZeroDiff plans the generated code against the live environment and
// reports every resource Terraform would create, change or destroy.
// Nothing is applied: planning only reads.
//
// An update is still a no-op for the cluster when every differing field is
// either a Terraform-only setting or the known provider read quirk on a field
// the live object leaves unset (see quirks.go). Both are recorded as notes.
func (t *tf) planZeroDiff(ctx context.Context, note func(addr string, df leafDiff) string) (*PlanResult, error) {
	if _, err := t.run(ctx, "plan", "-input=false", "-lock=false", "-no-color", "-out=capture.tfplan"); err != nil {
		return nil, err
	}
	js, err := t.run(ctx, "show", "-json", "capture.tfplan")
	if err != nil {
		return nil, err
	}
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions      []string        `json:"actions"`
				Importing    json.RawMessage `json:"importing"`
				Before       any             `json:"before"`
				After        any             `json:"after"`
				AfterUnknown any             `json:"after_unknown"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal([]byte(js), &plan); err != nil {
		return nil, fmt.Errorf("reading plan: %w", err)
	}
	r := &PlanResult{}
	for _, rc := range plan.ResourceChanges {
		ch := rc.Change
		if len(ch.Importing) > 0 && string(ch.Importing) != "null" {
			r.Imports++
		}
		if len(ch.Actions) == 1 && ch.Actions[0] == "no-op" {
			r.NoOps++
			continue
		}
		if len(ch.Actions) == 1 && ch.Actions[0] == "update" {
			var real, notes []string
			for _, df := range leafDiffs(nil, ch.Before, ch.After, ch.AfterUnknown) {
				if n := note(rc.Address, df); n != "" {
					notes = append(notes, n)
				} else {
					real = append(real, strings.Join(df.path, "."))
				}
			}
			if len(real) == 0 && len(notes) > 0 {
				r.NoOps++
				sort.Strings(notes)
				r.ProviderOnly = append(r.ProviderOnly, rc.Address+": "+strings.Join(notes, "; "))
				continue
			}
			r.Changes = append(r.Changes, fmt.Sprintf("%s: update %s", rc.Address, strings.Join(real, ", ")))
			continue
		}
		r.Changes = append(r.Changes, fmt.Sprintf("%s: %s", rc.Address, strings.Join(ch.Actions, ",")))
	}
	if len(r.Changes) > 0 {
		r.Rendered, _ = t.run(ctx, "show", "-no-color", "capture.tfplan")
	}
	return r, nil
}

type leafDiff struct {
	path          []string
	before, after any
}

// leafDiffs walks before/after plan values and returns every leaf that differs,
// plus every leaf Terraform marks as unknown after apply.
func leafDiffs(path []string, before, after, unknown any) []leafDiff {
	if u, ok := unknown.(bool); ok && u {
		return []leafDiff{{append([]string{}, path...), before, "(known after apply)"}}
	}
	bm, bIsMap := before.(map[string]any)
	am, aIsMap := after.(map[string]any)
	um, _ := unknown.(map[string]any)
	if bIsMap || aIsMap {
		var out []leafDiff
		keys := map[string]bool{}
		for k := range bm {
			keys[k] = true
		}
		for k := range am {
			keys[k] = true
		}
		for k := range um {
			keys[k] = true
		}
		for k := range keys {
			out = append(out, leafDiffs(append(path, k), bm[k], am[k], um[k])...)
		}
		return out
	}
	bl, bIsList := before.([]any)
	al, aIsList := after.([]any)
	ul, _ := unknown.([]any)
	if bIsList || aIsList {
		var out []leafDiff
		n := max(len(bl), len(al), len(ul))
		for i := 0; i < n; i++ {
			out = append(out, leafDiffs(append(path, strconv.Itoa(i)), at(bl, i), at(al, i), at(ul, i))...)
		}
		return out
	}
	if !reflect.DeepEqual(before, after) {
		return []leafDiff{{append([]string{}, path...), before, after}}
	}
	return nil
}

func at(l []any, i int) any {
	if i < len(l) {
		return l[i]
	}
	return nil
}
