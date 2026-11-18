package capture

import "testing"

func TestCount(t *testing.T) {
	for n, want := range map[int]string{0: "0 resources", 1: "1 resource", 30: "30 resources"} {
		if got := Count(n, "resource"); got != want {
			t.Errorf("Count(%d) = %q, want %q", n, got, want)
		}
	}
}
