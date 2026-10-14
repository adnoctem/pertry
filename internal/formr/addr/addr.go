// Package addr holds the typed identities every formr row is keyed by: provider source
// addresses, module paths, declaration addresses and field paths.
//
// Field paths are segment lists rather than dotted strings on purpose. `labels.owner` is a
// literal map key while `timeouts.create` is a nested field, and the two must never compare
// equal or sort together because they happened to render alike.
package addr

import (
	"fmt"
	"strings"
)

// DefaultHostname is the registry an unqualified provider source resolves against.
const DefaultHostname = "registry.terraform.io"

// Provider is a fully qualified provider source address.
type Provider struct {
	Hostname  string
	Namespace string
	Type      string
}

// String renders the address the way `terraform providers schema -json` keys it.
func (p Provider) String() string {
	return p.Hostname + "/" + p.Namespace + "/" + p.Type
}

// ParseProvider parses a `required_providers` source: `type`, `namespace/type` or
// `hostname/namespace/type`. Terraform compares these case-insensitively, so they are folded.
func ParseProvider(source string) (Provider, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(source)), "/")
	for _, part := range parts {
		if part == "" {
			return Provider{}, fmt.Errorf("invalid provider source %q", source)
		}
	}

	switch len(parts) {
	case 1:
		return Provider{Hostname: DefaultHostname, Namespace: "hashicorp", Type: parts[0]}, nil
	case 2:
		return Provider{Hostname: DefaultHostname, Namespace: parts[0], Type: parts[1]}, nil
	case 3:
		return Provider{Hostname: parts[0], Namespace: parts[1], Type: parts[2]}, nil
	default:
		return Provider{}, fmt.Errorf("invalid provider source %q", source)
	}
}

// DefaultProvider is what Terraform assumes for a local name nothing declares.
func DefaultProvider(localName string) Provider {
	return Provider{Hostname: DefaultHostname, Namespace: "hashicorp", Type: strings.ToLower(localName)}
}

// ModulePath names a module instance by its chain of call names. The analysed root is empty.
type ModulePath []string

// String renders `.` for the root and `module.a.module.b` otherwise.
func (m ModulePath) String() string {
	if len(m) == 0 {
		return "."
	}

	var b strings.Builder
	for i, name := range m {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString("module.")
		b.WriteString(name)
	}

	return b.String()
}

// Child returns the path of a call made from m.
func (m ModulePath) Child(name string) ModulePath {
	out := make(ModulePath, len(m), len(m)+1)
	copy(out, m)

	return append(out, name)
}

// Kind is the kind of declaration a row belongs to.
type Kind string

// Declaration kinds formr inventories.
const (
	KindProvider Kind = "provider"
	KindResource Kind = "resource"
	KindData     Kind = "data"
)

// Declaration identifies one provider configuration, resource or data block within one module
// instance. Two calls to the same source are two declarations, never one.
type Declaration struct {
	Module ModulePath
	Kind   Kind
	Type   string
	// Name is the block's name; for a provider configuration it is the alias, or empty.
	Name string
}

// Local renders the address as it would be written inside its own module.
func (d Declaration) Local() string {
	switch d.Kind {
	case KindProvider:
		if d.Name == "" {
			return "provider." + d.Type
		}

		return "provider." + d.Type + "." + d.Name
	case KindData:
		return "data." + d.Type + "." + d.Name
	default:
		return d.Type + "." + d.Name
	}
}

// String renders the absolute address: the module path prefix, then the local address.
func (d Declaration) String() string {
	if len(d.Module) == 0 {
		return d.Local()
	}

	return d.Module.String() + "." + d.Local()
}

// SegmentKind distinguishes the steps of a field path.
type SegmentKind int

// Segment kinds. Elem is one element of a list, set or tuple; MapElem one value of a map.
const (
	Attr SegmentKind = iota
	Elem
	MapElem
)

// Segment is one step of a field path.
type Segment struct {
	Kind SegmentKind
	Name string
}

// A is shorthand for an attribute or block segment.
func A(name string) Segment { return Segment{Kind: Attr, Name: name} }

// E is the collection-element segment.
func E() Segment { return Segment{Kind: Elem} }

// M is the map-element segment.
func M() Segment { return Segment{Kind: MapElem} }

// Path is a typed field path.
type Path []Segment

// Append returns a new path; it never aliases the receiver's backing array, because paths are
// built by extending a shared prefix in recursion and an aliased append would corrupt siblings.
func (p Path) Append(segs ...Segment) Path {
	out := make(Path, 0, len(p)+len(segs))
	out = append(out, p...)

	return append(out, segs...)
}

// String renders `network[].subnet_id`, `labels{}` or `timeouts.create`.
func (p Path) String() string {
	var b strings.Builder
	for i, s := range p {
		switch s.Kind {
		case Elem:
			b.WriteString("[]")
		case MapElem:
			b.WriteString("{}")
		default:
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s.Name)
		}
	}

	return b.String()
}

// Compare orders paths segment by segment, so that a container sorts directly before its
// children and siblings sort by name.
func (p Path) Compare(q Path) int {
	for i := 0; i < len(p) && i < len(q); i++ {
		if c := compareSegment(p[i], q[i]); c != 0 {
			return c
		}
	}

	return len(p) - len(q)
}

func compareSegment(a, b Segment) int {
	if a.Kind != b.Kind {
		return int(a.Kind) - int(b.Kind)
	}

	return strings.Compare(a.Name, b.Name)
}

// Covers reports whether a reference to p reads everything at q: p is q or one of q's
// ancestors. Element segments in p match any element of q, and a key lookup in p (an Attr
// where the schema has a collection) matches the collection element, because `x["k"]` and
// `x[0]` both select one element.
func (p Path) Covers(schema Path) bool {
	if len(p) > len(schema) {
		return false
	}

	return matches(p, schema[:len(p)])
}

// Within reports whether p reads strictly inside schema: a projection into one leaf, such as one
// key of a map attribute.
func (p Path) Within(schema Path) bool {
	if len(p) <= len(schema) {
		return false
	}

	return matches(p[:len(schema)], schema)
}

func matches(ref, schema Path) bool {
	for i := range ref {
		switch schema[i].Kind {
		case Elem, MapElem:
			// Any selection of one element matches an element step.
		default:
			if ref[i].Kind != Attr || ref[i].Name != schema[i].Name {
				return false
			}
		}
	}

	return true
}
