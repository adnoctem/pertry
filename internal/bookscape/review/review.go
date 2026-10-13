// Package review renders classified bookmarks for human inspection.
//
// No batch of several thousand links reaches Linkwarden without a sampled inspection pass. The
// irreversible step is the import on the far side - Linkwarden creates collections as a side
// effect, so unwinding a bad full run by hand is genuinely tedious - and this is the gate in
// front of it (docs/ARCHITECTURE.md#process-model).
package review

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/olekukonko/tablewriter"

	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// Table renders items as a table.
//
// The columns are chosen so that a reviewer can judge a classification without opening the
// link: the confidence says how much to trust it, the original folder path is the signal most
// likely to contradict a wrong collection, and the description is what the export will actually
// carry.
func Table(w io.Writer, items []store.ReviewItem) error {
	t := tablewriter.NewWriter(w)
	t.Header("ID", "Conf", "Src", "Live", "Collection", "Tags", "Original folder", "Link")

	for _, item := range items {
		if err := t.Append(
			strconv.FormatInt(item.ID, 10),
			confidence(item),
			short(string(item.Source)),
			short(string(item.Liveness)),
			item.Collection,
			strings.Join(item.Tags, ", "),
			strings.Join(item.SourcePaths, " | "),
			urlAndDescription(item),
		); err != nil {
			return fmt.Errorf("render review row for bookmark %d: %w", item.ID, err)
		}
	}

	if err := t.Render(); err != nil {
		return fmt.Errorf("render review table: %w", err)
	}

	return nil
}

// confidence renders the score, blank for a rule or manual result where it is always 1 and
// would only be noise.
func confidence(item store.ReviewItem) string {
	if item.Source != store.ClassifySourceModel {
		return ""
	}

	return fmt.Sprintf("%.2f", item.Confidence)
}

// short abbreviates a source or liveness value to keep the table narrow enough to read in a
// terminal.
func short(s string) string {
	switch s {
	case "":
		return "-"
	case string(store.ClassifySourceModel):
		return "mdl"
	case string(store.ClassifySourceManual):
		return "man"
	case string(store.ClassifySourceRule):
		return "rul"
	case string(store.StatusAlive):
		return "ok"
	case string(store.StatusDead):
		return "dead"
	case string(store.StatusUncertain):
		return "?"
	case string(store.StatusSkipped):
		return "skip"
	default:
		return s
	}
}

// urlAndDescription stacks the URL over the generated description. They share a column because
// the description is a statement about that URL, and side by side they would push the table
// past any sensible terminal width.
func urlAndDescription(item store.ReviewItem) string {
	if item.Description == "" {
		return item.URL
	}

	return item.URL + "\n" + item.Description
}

// Legend explains the abbreviations the table uses.
const Legend = "Src: rul=rule mdl=model man=manual   Live: ok=alive ?=uncertain skip=not fetchable"
