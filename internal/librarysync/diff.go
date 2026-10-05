package librarysync

import (
	"fmt"
	"strings"
)

const maxDiffLines = 4000

// UnifiedDiff is a unified diff of two texts with three lines of context, or
// a one-line note when either side is too long to compare line by line.
func UnifiedDiff(from, to, oldName, newName string) string {
	a, b := splitLines(from), splitLines(to)
	if len(a) > maxDiffLines || len(b) > maxDiffLines {
		return fmt.Sprintf("--- %s\n+++ %s\n（文件超过 %d 行，省略逐行差异）\n", oldName, newName, maxDiffLines)
	}
	// Longest common subsequence table, from the end.
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
		ai   int
		bi   int
	}
	var ops []line
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, line{' ', a[i], i, j})
			i, j = i+1, j+1
		// Removals before additions, as diff and git print a changed line.
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, line{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, line{'+', b[j], i, j})
			j++
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	const context = 3
	for k := 0; k < len(ops); {
		if ops[k].op == ' ' {
			k++
			continue
		}
		start := max(k-context, 0)
		end := k
		for end < len(ops) {
			if ops[end].op != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].op == ' ' {
				run++
			}
			if run-end > 2*context || run == len(ops) {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		aCount, bCount := 0, 0
		for _, op := range ops[start:end] {
			if op.op != '+' {
				aCount++
			}
			if op.op != '-' {
				bCount++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", ops[start].ai+1, aCount, ops[start].bi+1, bCount)
		for _, op := range ops[start:end] {
			out.WriteByte(op.op)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		k = end
	}
	return out.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
