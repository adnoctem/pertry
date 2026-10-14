// Package diag carries formr's diagnostics: what it could not read, could not analyse or does
// not support.
//
// A diagnostic is never a footnote. Anything that leaves the inventory or the analysis short of
// complete is a diagnostic, and the report and the exit code both say so - the alternative is a
// coverage report that is green because it did not look.
package diag

import (
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2"
)

// Severity orders diagnostics by consequence.
type Severity int

// Severities. An Error makes the inventory incomplete; a Warning is reported and does not.
const (
	Warning Severity = iota
	Error
)

func (s Severity) String() string {
	if s == Error {
		return "error"
	}

	return "warning"
}

// Diagnostic is one finding.
type Diagnostic struct {
	Severity Severity
	// Module is the module path the finding belongs to, rendered (`.` or `module.x`).
	Module  string
	Summary string
	Detail  string
	// Subject is the source range, relative to the analysed root. It is printed on the console and
	// deliberately left out of the committed report, where a line number would turn every
	// unrelated edit above it into report drift.
	Subject *hcl.Range
}

// Diagnostics is an ordered collection.
type Diagnostics []Diagnostic

// Errorf appends an error.
func (d *Diagnostics) Errorf(module string, subject *hcl.Range, format string, args ...any) {
	*d = append(*d, Diagnostic{Severity: Error, Module: module, Summary: fmt.Sprintf(format, args...), Subject: subject})
}

// Warnf appends a warning.
func (d *Diagnostics) Warnf(module string, subject *hcl.Range, format string, args ...any) {
	*d = append(*d, Diagnostic{Severity: Warning, Module: module, Summary: fmt.Sprintf(format, args...), Subject: subject})
}

// HasErrors reports whether any diagnostic is an error.
func (d Diagnostics) HasErrors() bool {
	for _, x := range d {
		if x.Severity == Error {
			return true
		}
	}

	return false
}

// FromHCL converts HCL's own diagnostics.
func FromHCL(module string, in hcl.Diagnostics) Diagnostics {
	out := make(Diagnostics, 0, len(in))
	for _, x := range in {
		sev := Warning
		if x.Severity == hcl.DiagError {
			sev = Error
		}
		out = append(out, Diagnostic{Severity: sev, Module: module, Summary: x.Summary, Detail: x.Detail, Subject: x.Subject})
	}

	return out
}

// Sorted returns the diagnostics in a stable order independent of discovery order, so that the
// report does not change when files are read in a different sequence.
func (d Diagnostics) Sorted() Diagnostics {
	out := append(Diagnostics(nil), d...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.Module != b.Module {
			return a.Module < b.Module
		}
		if a.Summary != b.Summary {
			return a.Summary < b.Summary
		}

		return a.Detail < b.Detail
	})

	return dedupe(out)
}

func dedupe(in Diagnostics) Diagnostics {
	out := make(Diagnostics, 0, len(in))
	for _, x := range in {
		if n := len(out); n > 0 {
			p := out[n-1]
			if p.Severity == x.Severity && p.Module == x.Module && p.Summary == x.Summary && p.Detail == x.Detail {
				continue
			}
		}
		out = append(out, x)
	}

	return out
}
