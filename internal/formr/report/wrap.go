package report

import "strings"

// Wrap breaks prose lines - paragraphs and list items - longer than width, greedily, the way
// Prettier's `proseWrap: always` fills a line. Table rows, headings and HTML comments are left
// alone: a Markdown table row cannot be continued on another line, and markdownlint judges
// headings against their own limit. A width of 0 or less disables wrapping.
//
// A code span is never split. A line break inside one is valid Markdown, but Prettier de-indents
// the continuation of a code span wrapped inside a list item, which silently ends the list.
func Wrap(text string, width int) string {
	if width <= 0 {
		return text
	}

	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if len([]rune(line)) <= width || !isProse(line) {
			out = append(out, line)
			continue
		}

		first, cont := "", ""
		body := line
		if strings.HasPrefix(line, "- ") {
			first, cont, body = "- ", "  ", line[2:]
		}
		out = append(out, fill(tokens(body), first, cont, width)...)
	}

	return strings.Join(out, "\n")
}

func isProse(line string) bool {
	switch {
	case strings.HasPrefix(line, "|"), strings.HasPrefix(line, "#"), strings.HasPrefix(line, "<!--"):
		return false
	default:
		return strings.TrimSpace(line) != ""
	}
}

// tokens splits on spaces outside code spans.
func tokens(s string) []string {
	var out []string
	var cur strings.Builder
	inCode := false
	for _, r := range s {
		switch {
		case r == '`':
			inCode = !inCode
			cur.WriteRune(r)
		case r == ' ' && !inCode:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}

	return out
}

// fill lays tokens out greedily. A token longer than the width gets a line of its own, which
// is the only way a wrapped line can still exceed it.
func fill(words []string, first, cont string, width int) []string {
	var out []string
	line := first
	empty := true
	for _, w := range words {
		switch {
		case empty:
			line += w
			empty = false
		case len([]rune(line))+1+len([]rune(w)) <= width:
			line += " " + w
		default:
			out = append(out, line)
			line = cont + w
		}
	}

	return append(out, line)
}
