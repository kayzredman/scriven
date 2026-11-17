package library

import (
	"testing"

	"scriven/internal/config"
)

func TestObservedBranchFor(t *testing.T) {
	cfg := config.Library{}
	for base, want := range map[string]string{"": "observed", "main": "observed", "dev-library": "observed-dev-library"} {
		if got := ObservedBranchFor(cfg, base); got != want {
			t.Errorf("ObservedBranchFor(%q) = %q, want %q", base, got, want)
		}
	}
}
