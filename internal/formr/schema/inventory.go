package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/adnoctem/pertry/internal/formr/addr"
)

// Flags are a node's schema flags.
type Flags struct {
	Required   bool
	Optional   bool
	Computed   bool
	Sensitive  bool
	Deprecated bool
	WriteOnly  bool
}

// Configurable reports whether a module can set the field at all.
func (f Flags) Configurable() bool { return f.Required || f.Optional }

// ComputedOnly reports a field only the provider sets. It is never a missing input.
func (f Flags) ComputedOnly() bool { return f.Computed && !f.Configurable() }

// Readable reports whether the field's value can be read back. A write-only field cannot.
func (f Flags) Readable() bool { return !f.WriteOnly }

// String renders the flags in a fixed order for the report.
func (f Flags) String() string {
	var parts []string
	switch {
	case f.Required:
		parts = append(parts, "required")
	case f.Optional && f.Computed:
		parts = append(parts, "optional+computed")
	case f.Optional:
		parts = append(parts, "optional")
	case f.Computed:
		parts = append(parts, "computed")
	}
	if f.Sensitive {
		parts = append(parts, "sensitive")
	}
	if f.WriteOnly {
		parts = append(parts, "write-only")
	}
	if f.Deprecated {
		parts = append(parts, "deprecated")
	}

	return strings.Join(parts, ", ")
}

// Node is one field or container in a schema.
type Node struct {
	Path addr.Path
	// Container nodes (blocks, nested-type attributes, object-typed attributes) are listed but
	// never counted: percentages are over leaves, so that a block is not counted twice.
	Container bool
	Type      string
	Flags     Flags
	// Block is true for a nested block, as opposed to an attribute.
	Block bool
	// Nesting is the block or nested-type nesting mode, empty for plain attributes.
	Nesting  string
	MinItems uint64
	MaxItems uint64
	// Fingerprint is a short structural hash. It changes when anything about the node except its
	// prose changes, including keys this decoder does not model.
	Fingerprint string
}

// Inventory is every node of one schema, in path order.
type Inventory struct {
	Nodes []Node
}

// Leaves returns the non-container nodes.
func (inv *Inventory) Leaves() []Node {
	var out []Node
	for _, n := range inv.Nodes {
		if !n.Container {
			out = append(out, n)
		}
	}

	return out
}

// Lookup returns the node at path.
func (inv *Inventory) Lookup(p addr.Path) (Node, bool) {
	for _, n := range inv.Nodes {
		if n.Path.Compare(p) == 0 {
			return n, true
		}
	}

	return Node{}, false
}

// Set is a decoded `providers schema -json` document: the typed form for walking, the raw form
// for fingerprints, so that metadata the typed decoder does not know still invalidates a
// baseline when it changes.
type Set struct {
	FormatVersion string
	typed         *tfjson.ProviderSchemas
	raw           map[string]any
}

// SupportedFormatMajor is the only `format_version` major this build understands.
const SupportedFormatMajor = "1"

// Decode parses and validates a schema document.
func Decode(data []byte) (*Set, error) {
	var typed tfjson.ProviderSchemas
	if err := json.Unmarshal(data, &typed); err != nil {
		return nil, fmt.Errorf("decode provider schemas: %w", err)
	}
	if major, _, _ := strings.Cut(typed.FormatVersion, "."); major != SupportedFormatMajor {
		return nil, fmt.Errorf("provider schema format %q is not supported (this build reads %s.x)", typed.FormatVersion, SupportedFormatMajor)
	}
	if err := typed.Validate(); err != nil {
		return nil, fmt.Errorf("provider schemas: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	// Numbers stay as written, so that re-encoding for a fingerprint cannot reformat them.
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode provider schemas: %w", err)
	}
	providers, _ := raw["provider_schemas"].(map[string]any)

	return &Set{FormatVersion: typed.FormatVersion, typed: &typed, raw: providers}, nil
}

// Has reports whether the set carries a schema for provider p.
func (s *Set) Has(p addr.Provider) bool {
	_, ok := s.typed.Schemas[p.String()]

	return ok
}

// Resolve maps a provider address to the one these schemas use. A source written without a
// hostname means "the engine's default registry", which is registry.terraform.io for Terraform
// and registry.opentofu.org for OpenTofu, so an address parsed against one default is moved to
// the other when only the other is present. An address the schemas carry is returned unchanged,
// and so is one neither registry has.
func (s *Set) Resolve(p addr.Provider) addr.Provider {
	if s.Has(p) {
		return p
	}

	alt := p
	switch p.Hostname {
	case addr.DefaultHostname:
		alt.Hostname = addr.OpenTofuHostname
	case addr.OpenTofuHostname:
		alt.Hostname = addr.DefaultHostname
	default:
		return p
	}
	if s.Has(alt) {
		return alt
	}

	return p
}

// Inventory builds the inventory of one provider configuration, resource or data-source type.
// ok is false when the provider has no schema of that kind and name.
func (s *Set) Inventory(p addr.Provider, kind addr.Kind, typ string) (*Inventory, bool) {
	ps, ok := s.typed.Schemas[p.String()]
	if !ok {
		return nil, false
	}
	rawProvider, _ := s.raw[p.String()].(map[string]any)

	var sch *tfjson.Schema
	var raw map[string]any
	switch kind {
	case addr.KindProvider:
		sch = ps.ConfigSchema
		raw, _ = rawProvider["provider"].(map[string]any)
	case addr.KindResource:
		sch = ps.ResourceSchemas[typ]
		raw = lookup(rawProvider, "resource_schemas", typ)
	case addr.KindData:
		sch = ps.DataSourceSchemas[typ]
		raw = lookup(rawProvider, "data_source_schemas", typ)
	}
	if sch == nil {
		if kind == addr.KindProvider {
			// A provider with no configuration schema has nothing to configure.
			return &Inventory{}, true
		}

		return nil, false
	}

	inv := &Inventory{}
	if sch.Block != nil {
		rawBlock, _ := raw["block"].(map[string]any)
		walkBlock(inv, nil, sch.Block, rawBlock)
	}
	sort.SliceStable(inv.Nodes, func(i, j int) bool { return inv.Nodes[i].Path.Compare(inv.Nodes[j].Path) < 0 })

	return inv, true
}

func lookup(m map[string]any, keys ...string) map[string]any {
	cur := m
	for _, k := range keys {
		next, _ := cur[k].(map[string]any)
		if next == nil {
			return nil
		}
		cur = next
	}

	return cur
}

func walkBlock(inv *Inventory, prefix addr.Path, b *tfjson.SchemaBlock, raw map[string]any) {
	rawAttrs, _ := raw["attributes"].(map[string]any)
	for name, a := range b.Attributes {
		rawAttr, _ := rawAttrs[name].(map[string]any)
		walkAttribute(inv, prefix.Append(addr.A(name)), a, rawAttr)
	}

	rawBlocks, _ := raw["block_types"].(map[string]any)
	for name, bt := range b.NestedBlocks {
		rawBT, _ := rawBlocks[name].(map[string]any)
		path := prefix.Append(addr.A(name))
		inv.Nodes = append(inv.Nodes, Node{
			Path:        path,
			Container:   true,
			Block:       true,
			Type:        "block",
			Nesting:     string(bt.NestingMode),
			MinItems:    bt.MinItems,
			MaxItems:    bt.MaxItems,
			Flags:       Flags{Required: bt.MinItems > 0, Optional: bt.MinItems == 0, Deprecated: bt.Block != nil && bt.Block.Deprecated},
			Fingerprint: fingerprint(rawBT),
		})
		if bt.Block != nil {
			rawInner, _ := rawBT["block"].(map[string]any)
			walkBlock(inv, path.Append(nestingSegments(string(bt.NestingMode))...), bt.Block, rawInner)
		}
	}
}

// nestingSegments is the path step a nesting mode puts between a container and its fields.
func nestingSegments(mode string) []addr.Segment {
	switch mode {
	case "list", "set":
		return []addr.Segment{addr.E()}
	case "map":
		return []addr.Segment{addr.M()}
	default:
		// single and group nest one object directly.
		return nil
	}
}

func walkAttribute(inv *Inventory, path addr.Path, a *tfjson.SchemaAttribute, raw map[string]any) {
	flags := Flags{
		Required:   a.Required,
		Optional:   a.Optional,
		Computed:   a.Computed,
		Sensitive:  a.Sensitive,
		Deprecated: a.Deprecated,
		WriteOnly:  a.WriteOnly,
	}

	if nt := a.AttributeNestedType; nt != nil {
		inv.Nodes = append(inv.Nodes, Node{
			Path:        path,
			Container:   true,
			Type:        "nested",
			Nesting:     string(nt.NestingMode),
			MinItems:    nt.MinItems,
			MaxItems:    nt.MaxItems,
			Flags:       flags,
			Fingerprint: fingerprint(raw),
		})
		rawNested, _ := raw["nested_type"].(map[string]any)
		rawAttrs, _ := rawNested["attributes"].(map[string]any)
		inner := path.Append(nestingSegments(string(nt.NestingMode))...)
		for name, child := range nt.Attributes {
			rawChild, _ := rawAttrs[name].(map[string]any)
			walkAttribute(inv, inner.Append(addr.A(name)), child, rawChild)
		}

		return
	}

	t := a.AttributeType
	inv.Nodes = append(inv.Nodes, Node{
		Path:        path,
		Container:   containsObject(t),
		Type:        TypeString(t),
		Flags:       flags,
		Fingerprint: fingerprint(raw),
	})
	if containsObject(t) {
		expandType(inv, path, t, flags)
	}
}

// expandType lists the attributes of an object type written in `type`. They carry no flags of
// their own in a provider schema, so they inherit the enclosing attribute's.
func expandType(inv *Inventory, path addr.Path, t cty.Type, flags Flags) {
	switch {
	case t.IsObjectType():
		for name, at := range t.AttributeTypes() {
			p := path.Append(addr.A(name))
			inv.Nodes = append(inv.Nodes, Node{
				Path:        p,
				Container:   containsObject(at),
				Type:        TypeString(at),
				Flags:       flags,
				Fingerprint: typeFingerprint(at, flags),
			})
			expandType(inv, p, at, flags)
		}
	case t.IsListType() || t.IsSetType():
		expandType(inv, path.Append(addr.E()), t.ElementType(), flags)
	case t.IsMapType():
		expandType(inv, path.Append(addr.M()), t.ElementType(), flags)
	}
}

func containsObject(t cty.Type) bool {
	switch {
	case t.IsObjectType():
		return true
	case t.IsListType() || t.IsSetType() || t.IsMapType():
		return containsObject(t.ElementType())
	default:
		return false
	}
}

// TypeString renders a type in Terraform's constraint syntax, abbreviating object and tuple
// members, which the report lists as their own rows.
func TypeString(t cty.Type) string {
	switch {
	case t == cty.NilType:
		return ""
	case t == cty.DynamicPseudoType:
		return "any"
	case t.IsPrimitiveType():
		return t.FriendlyName()
	case t.IsListType():
		return "list(" + TypeString(t.ElementType()) + ")"
	case t.IsSetType():
		return "set(" + TypeString(t.ElementType()) + ")"
	case t.IsMapType():
		return "map(" + TypeString(t.ElementType()) + ")"
	case t.IsObjectType():
		return "object"
	case t.IsTupleType():
		return "tuple"
	default:
		return t.FriendlyName()
	}
}

// proseKeys are excluded from fingerprints: a reworded description is not a schema change.
var proseKeys = map[string]bool{"description": true, "description_kind": true, "deprecation_message": true}

func fingerprint(raw map[string]any) string {
	return hash(stripProse(raw))
}

func typeFingerprint(t cty.Type, f Flags) string {
	enc, err := ctyjson.MarshalType(t)
	if err != nil {
		enc = []byte(t.GoString())
	}

	return hash(map[string]any{"type": json.RawMessage(enc), "flags": f.String()})
}

func stripProse(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if proseKeys[k] {
				continue
			}
			out[k] = stripProse(val)
		}

		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = stripProse(val)
		}

		return out
	default:
		return v
	}
}

func hash(v any) string {
	// encoding/json sorts map keys, which is what makes this canonical.
	enc, err := json.Marshal(v)
	if err != nil {
		enc = []byte(fmt.Sprint(v))
	}
	sum := sha256.Sum256(enc)

	return hex.EncodeToString(sum[:])[:12]
}
