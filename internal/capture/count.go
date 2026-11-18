package capture

import "strconv"

// Count writes a number with its noun: "1 resource", "30 resources".
func Count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
