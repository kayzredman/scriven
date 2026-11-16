package watch

import "strings"

// blockDiff is a small line diff of two HCL blocks with two lines of context,
// enough to show what changed in a notification.
func blockDiff(before, after string) string {
	a := splitLines(before)
	b := splitLines(after)
	switch {
	case len(a) == 0 && len(b) == 0:
		return ""
	case len(a) == 0:
		return prefix(b, "+ ")
	case len(b) == 0:
		return prefix(a, "- ")
	}
	// Longest common subsequence table.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type line struct {
		op   byte
		text string
	}
	var lines []line
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			lines = append(lines, line{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			lines = append(lines, line{'-', a[i]})
			i++
		default:
			lines = append(lines, line{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		lines = append(lines, line{'-', a[i]})
	}
	for ; j < len(b); j++ {
		lines = append(lines, line{'+', b[j]})
	}

	const ctx = 2
	keep := make([]bool, len(lines))
	for k, l := range lines {
		if l.op != ' ' {
			for m := max(0, k-ctx); m <= min(len(lines)-1, k+ctx); m++ {
				keep[m] = true
			}
		}
	}
	var sb strings.Builder
	gap := false
	for k, l := range lines {
		if !keep[k] {
			gap = true
			continue
		}
		if gap && sb.Len() > 0 {
			sb.WriteString("  ...\n")
		}
		gap = false
		sb.WriteByte(l.op)
		sb.WriteString(" " + l.text + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func prefix(lines []string, p string) string {
	for i := range lines {
		lines[i] = p + lines[i]
	}
	return strings.Join(lines, "\n")
}
