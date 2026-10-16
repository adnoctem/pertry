package report

import (
	"strconv"
	"strings"
)

// Diff returns the lines removed from want and added in got, in order, each prefixed with `-` or
// `+`. It is empty when the two are identical. Because the report is deterministic and one row
// is one line, a changed line is a changed row.
func Diff(want, got string) []string {
	a := strings.Split(want, "\n")
	b := strings.Split(got, "\n")

	// Longest common subsequence over lines. Reports are hundreds of lines, so the quadratic
	// table is a few hundred kilobytes at most.
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

	var out []string
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "-"+a[i])
			i++
		default:
			out = append(out, "+"+b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, "-"+a[i])
	}
	for ; j < len(b); j++ {
		out = append(out, "+"+b[j])
	}

	return out
}

// ChangeKind classifies one difference between two reports.
type ChangeKind string

// Change kinds.
const (
	Added   ChangeKind = "added"
	Removed ChangeKind = "removed"
	Changed ChangeKind = "changed"
)

// Change is one row, or one line of prose, that differs between two reports.
type Change struct {
	Kind ChangeKind
	// Section is the heading the row sits under: a declaration, a provider, or a report section.
	Section string
	// Row is the row's first cell (a field path, a measurement) or, outside a table, the line.
	Row string
	// Columns lists, for a changed table row, each column that changed, as
	// `Column: before → after`.
	Columns []string
}

// Changes compares two reports row by row. A table row is identified by its section and its
// first cell, so that a field whose mapping changed reads as one change to that field rather
// than as a pair of 900-character lines; anything outside a table is compared as a line.
func Changes(want, got string) []Change {
	before, order := rows(want, nil)
	after, order := rows(got, order)

	var out []Change
	for _, id := range order {
		b, inBefore := before[id]
		a, inAfter := after[id]
		switch {
		case !inBefore:
			out = append(out, Change{Kind: Added, Section: id.section, Row: id.row})
		case !inAfter:
			out = append(out, Change{Kind: Removed, Section: id.section, Row: id.row})
		case !sameRow(a, b):
			out = append(out, Change{Kind: Changed, Section: id.section, Row: id.row, Columns: columns(b, a)})
		}
	}

	return out
}

type rowID struct{ section, row string }

type row struct {
	line   string
	header []string
	cells  []string
}

// maxCell bounds the excerpt quoted from a changed cell; a mapping cell can run to hundreds of
// characters.
const maxCell = 80

// sameRow compares table rows by their cells rather than their bytes: one wider cell re-pads
// every row of its table, and that is not a change to those rows.
func sameRow(a, b row) bool {
	if a.cells == nil || b.cells == nil {
		return a.line == b.line
	}
	if len(a.cells) != len(b.cells) {
		return false
	}
	for i := range a.cells {
		if a.cells[i] != b.cells[i] {
			return false
		}
	}

	return true
}

func columns(before, after row) []string {
	if before.cells == nil || after.cells == nil || len(before.cells) != len(after.cells) {
		return nil
	}
	var out []string
	for i := range after.cells {
		if before.cells[i] == after.cells[i] {
			continue
		}
		name := ""
		if i < len(after.header) {
			name = after.header[i] + ": "
		}
		old, cur := excerpt(before.cells[i], after.cells[i])
		out = append(out, name+old+" → "+cur)
	}

	return out
}

// excerpt quotes two versions of a cell around where they differ. Long mapping cells usually
// share most of their text, so quoting their beginnings would show two identical strings.
func excerpt(before, after string) (string, string) {
	a, b := []rune(before), []rune(after)
	if len(a) <= maxCell && len(b) <= maxCell {
		return before, after
	}

	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	// Keep a little context before the difference, then clip what remains.
	const context = 32
	start := max(0, prefix-context)

	return window(a, start, len(a)-suffix), window(b, start, len(b)-suffix)
}

func window(r []rune, start, end int) string {
	lead := ""
	if start > 0 {
		lead = "…"
	}
	if end-start > maxCell {
		end = start + maxCell
	}
	trail := ""
	if end < len(r) {
		trail = "…"
	}

	return lead + string(r[start:end]) + trail
}

// splitRow splits a table row into trimmed cells, honouring escaped pipes.
func splitRow(line string) []string {
	body := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|")
	var cells []string
	var cur strings.Builder
	escaped := false
	for _, r := range body {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			cur.WriteRune(r)
			escaped = true
		case r == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}

	return append(cells, strings.TrimSpace(cur.String()))
}

// rows indexes a report. order accumulates identities in first-seen order across both reports,
// so the output follows the report's own layout.
func rows(text string, order []rowID) (map[rowID]row, []rowID) {
	seen := make(map[rowID]bool, len(order))
	for _, id := range order {
		seen[id] = true
	}
	out := map[rowID]row{}
	add := func(id rowID, r row) {
		// A repeated identity (two identical prose lines in one section) keeps both apart.
		for n := 2; ; n++ {
			if _, dup := out[id]; !dup {
				break
			}
			id.row = strings.TrimSuffix(id.row, " ("+strconv.Itoa(n-1)+")") + " (" + strconv.Itoa(n) + ")"
		}
		out[id] = r
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
	}

	section := ""
	inTable := false
	var header []string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			section = strings.TrimSpace(strings.TrimLeft(line, "#"))
			inTable = false
		case strings.HasPrefix(line, "|"):
			cells := splitRow(line)
			if !inTable {
				inTable = true
				header = cells
				continue
			}
			if strings.Trim(cells[0], "- ") == "" {
				// The separator row.
				continue
			}
			add(rowID{section, cells[0]}, row{line: line, header: header, cells: cells})
		case strings.TrimSpace(line) == "":
			inTable = false
		default:
			inTable = false
			add(rowID{section, strings.TrimSpace(line)}, row{line: line})
		}
	}

	return out, order
}
