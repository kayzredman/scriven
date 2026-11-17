package wizard

import (
	"testing"

	"scriven/internal/config"
)

func TestSeparateAutoMerge(t *testing.T) {
	quiet := func(string, ...any) {}
	p := map[string]config.MergeRule{"default": {Approvals: 1}, "dev": {AutoMerge: true}, "prod": {Approvals: 2}}
	separateAutoMerge(p, nil, quiet)
	if p["dev"].Branch != "dev-library" || p["prod"].Branch != "" {
		t.Errorf("auto-merged dev beside reviewed envs: %+v", p)
	}
	// Everything auto-merged: main is fine, no extra branch.
	p = map[string]config.MergeRule{"default": {AutoMerge: true}, "dev": {AutoMerge: true}}
	separateAutoMerge(p, nil, quiet)
	if p["dev"].Branch != "" {
		t.Errorf("no reviewed env, yet dev got branch %q", p["dev"].Branch)
	}
	// A branch chosen before is kept.
	p = map[string]config.MergeRule{"default": {Approvals: 1}, "dev": {AutoMerge: true}}
	separateAutoMerge(p, map[string]string{"dev": "sandbox"}, quiet)
	if p["dev"].Branch != "sandbox" {
		t.Errorf("earlier branch not kept: %q", p["dev"].Branch)
	}
}
