package analysis

import (
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/adnoctem/pertry/internal/formr/addr"
	"github.com/adnoctem/pertry/internal/formr/diag"
	"github.com/adnoctem/pertry/internal/formr/module"
	"github.com/adnoctem/pertry/internal/formr/schema"
)

// Mapping classifies how a configurable field is set.
type Mapping string

// Input mappings.
const (
	Direct      Mapping = "direct"
	Transformed Mapping = "transformed"
	Derived     Mapping = "derived"
	Fixed       Mapping = "fixed"
	Omitted     Mapping = "omitted"
	Unresolved  Mapping = "unresolved"
	// CallerOwned is a provider configuration field supplied by whoever calls the module.
	CallerOwned Mapping = "caller-owned"
	// Present marks a static nested block: the block exists, and its fields say what is in it.
	Present Mapping = "present"
	// NotConfigurable marks a computed-only field, which is never a missing input.
	NotConfigurable Mapping = ""
)

// Availability says whether a set field is always set.
type Availability string

// Availabilities.
const (
	Unconditional Availability = "unconditional"
	Conditional   Availability = "conditional"
	Coupled       Availability = "coupled"
	NotSet        Availability = ""
)

// Usage classifies what happens to a readable field's value.
type Usage string

// Result usages.
const (
	Exported          Usage = "exported"
	PartiallyExported Usage = "partially-exported"
	Internal          Usage = "internal"
	Unused            Usage = "unused"
	NotReadable       Usage = ""
)

// Review is a row's review status.
type Review string

// Review statuses. Exceptions arrive in M2.
const (
	Automatic   Review = "automatic"
	NeedsReview Review = "needs-review"
)

// Field is one classified schema node.
type Field struct {
	Node         schema.Node
	Input        Mapping
	Origins      []string
	ForcedKeys   []string
	Unresolved   []string
	Availability Availability
	Usage        Usage
	Review       Review
}

// Declaration is one classified declaration.
type Declaration struct {
	Addr     addr.Declaration
	Provider addr.Provider
	// Instances lists what count or for_each depends on, when the declaration is conditional.
	Instances []string
	// InstancesConditional is true when count or for_each is present.
	InstancesConditional bool
	// SchemaMissing is true when no schema for the type was available.
	SchemaMissing bool
	Fields        []Field
}

// ProviderConfig is one provider configuration the module tree uses.
type ProviderConfig struct {
	Provider addr.Provider
	Alias    string
	Owner    module.Owner
	// Location is where the provider block lives, for a module-owned configuration.
	Location      string
	SchemaMissing bool
	Fields        []Field
}

// Result is the analysis of one module tree.
type Result struct {
	Providers    []ProviderConfig
	Declarations []Declaration
	Diagnostics  diag.Diagnostics
}

// fieldFlow is what was found for one node in one declaration body.
type fieldFlow struct {
	flow Flow
	set  bool
}

type declWork struct {
	inst  *module.Instance
	decl  *module.Declaration
	inv   *schema.Inventory
	flows map[string]*fieldFlow
	out   *Declaration
}

// Analyze classifies every field of every schema in scope.
func Analyze(tree *module.Tree, set *schema.Set) *Result {
	res := &Result{}
	var work []*declWork
	var internal []Origin

	providers := map[string]*ProviderConfig{}
	addProvider := func(b module.Binding) {
		key := b.Provider.String() + "|" + b.Alias
		if _, ok := providers[key]; ok {
			return
		}
		pc := &ProviderConfig{Provider: b.Provider, Alias: b.Alias, Owner: b.Owner}
		if b.Owner == module.OwnerModule {
			pc.Location = b.Instance.Path.String()
		}
		providers[key] = pc
	}

	for _, inst := range tree.Instances {
		label := inst.Path.String()

		for _, name := range sortedKeys(inst.Module.RequiredProviders) {
			addProvider(inst.Bind(module.ProviderRef{LocalName: name}))
		}

		for _, d := range inst.Module.Declarations {
			if d.Kind == addr.KindProvider {
				addProvider(inst.Bind(d.Provider))
				continue
			}

			b := inst.Bind(d.Provider)
			rng := d.Range
			if b.Owner == module.OwnerUnbound {
				res.Diagnostics.Errorf(label, &rng, "%s uses provider configuration %s, which nothing configures or passes in",
					addr.Declaration{Module: inst.Path, Kind: d.Kind, Type: d.Type, Name: d.Name}, d.Provider)
			}
			addProvider(b)

			out := &Declaration{
				Addr:     addr.Declaration{Module: inst.Path, Kind: d.Kind, Type: d.Type, Name: d.Name},
				Provider: b.Provider,
			}
			if gate, conditional := instanceGate(inst, d); conditional {
				out.InstancesConditional = true
				out.Instances = originStrings(gate)
				internal = append(internal, gate.Origins...)
			}

			inv, ok := set.Inventory(b.Provider, d.Kind, d.Type)
			if !ok {
				out.SchemaMissing = true
				if !inst.Module.Declares(d.Provider.LocalName) {
					res.Diagnostics.Errorf(label, &rng, "no schema for %s %s in provider %s: the inventory is incomplete; "+
						"%s does not declare %q in required_providers, so Terraform assumes %s",
						d.Kind, d.Type, b.Provider, inst.Path, d.Provider.LocalName, b.Provider)
				} else {
					res.Diagnostics.Errorf(label, &rng, "no schema for %s %s in provider %s: the inventory is incomplete", d.Kind, d.Type, b.Provider)
				}
				res.Declarations = append(res.Declarations, *out)
				continue
			}

			w := &declWork{inst: inst, decl: d, inv: inv, flows: map[string]*fieldFlow{}, out: out}
			sc := scope{inst: inst}
			if d.ForEach != nil {
				sc.env = sc.env.bind("each", symbol{kind: iterator, collection: d.ForEach, scope: scope{inst: inst}})
			}
			internal = append(internal, w.walkBody(sc, d.Body, nil, false, &res.Diagnostics)...)
			work = append(work, w)
		}

		for _, cond := range inst.Module.Conditions {
			internal = append(internal, Trace(inst, cond, nil).Origins...)
		}
	}

	var exported []Origin
	for _, name := range sortedKeys(tree.Root.Module.Outputs) {
		out := tree.Root.Module.Outputs[name]
		exported = append(exported, Trace(tree.Root, out.Value, nil).Origins...)
	}

	for _, w := range work {
		w.out.Fields = w.classify(exported, internal)
		res.Declarations = append(res.Declarations, *w.out)
	}

	res.Providers = providerConfigs(tree, set, providers, &res.Diagnostics)

	sort.SliceStable(res.Declarations, func(i, j int) bool {
		return res.Declarations[i].Addr.String() < res.Declarations[j].Addr.String()
	})

	return res
}

// instanceGate collects what decides whether a declaration has instances: its own count or
// for_each, and those of every module call above it. A resource inside a counted module call is
// as conditional as one with a count of its own.
func instanceGate(inst *module.Instance, d *module.Declaration) (Flow, bool) {
	var gate Flow
	conditional := false
	if d.Conditional() {
		conditional = true
		gate = gate.union(Dependencies(inst, firstNonNil(d.Count, d.ForEach)))
	}
	for cur := inst; cur.Parent != nil; cur = cur.Parent {
		if cur.Call.Count != nil || cur.Call.ForEach != nil {
			conditional = true
			gate = gate.union(Dependencies(cur.Parent, firstNonNil(cur.Call.Count, cur.Call.ForEach)))
		}
	}

	return gate, conditional
}

func firstNonNil(a, b hcl.Expression) hcl.Expression {
	if a != nil {
		return a
	}

	return b
}

// metaArguments are Terraform language constructs, not provider fields. `alias` and `version`
// only appear in provider blocks, where they are equally not the provider's.
var metaArguments = map[string]bool{
	"count": true, "for_each": true, "provider": true, "depends_on": true, "alias": true, "version": true,
}

var metaBlocks = map[string]bool{"lifecycle": true, "provisioner": true, "connection": true}

// walkBody records the flow of every argument and block in body against the schema nodes below
// prefix, and returns every declaration origin it read, which is internal use.
func (w *declWork) walkBody(sc scope, body *hclsyntax.Body, prefix addr.Path, dynamic bool, diags *diag.Diagnostics) []Origin {
	var internal []Origin
	label := w.inst.Path.String()

	for _, name := range sortedKeys(body.Attributes) {
		attr := body.Attributes[name]
		if len(prefix) == 0 && metaArguments[name] {
			continue
		}
		path := prefix.Append(addr.A(name))
		node, ok := w.inv.Lookup(path)
		if !ok || node.Block {
			rng := attr.SrcRange
			diags.Warnf(label, &rng, "%s sets %s, which the provider schema does not define", w.out.Addr, path)
			continue
		}

		for _, n := range w.inv.Nodes {
			// The node itself and, for an object or nested-type attribute, every field inside it.
			if len(n.Path) < len(path) || path.Compare(n.Path[:len(path)]) != 0 {
				continue
			}
			f := newTracer().trace(sc, attr.Expr, n.Path[len(path):])
			if dynamic {
				f.Conditional = true
			}
			w.record(n.Path, f)
			internal = append(internal, f.Origins...)
		}
	}

	for _, block := range body.Blocks {
		switch {
		case len(prefix) == 0 && metaBlocks[block.Type]:
			continue
		case block.Type == "dynamic":
			internal = append(internal, w.dynamicBlock(sc, block, prefix, diags)...)
		default:
			path := prefix.Append(addr.A(block.Type))
			node, ok := w.inv.Lookup(path)
			if !ok || !node.Block {
				rng := block.DefRange()
				diags.Warnf(label, &rng, "%s has a %s block, which the provider schema does not define", w.out.Addr, path)
				continue
			}
			w.record(path, Flow{Conditional: dynamic})
			internal = append(internal, w.walkBody(sc, block.Body, path.Append(nestingSegments(node.Nesting)...), dynamic, diags)...)
		}
	}

	return internal
}

func (w *declWork) dynamicBlock(sc scope, block *hclsyntax.Block, prefix addr.Path, diags *diag.Diagnostics) []Origin {
	label := w.inst.Path.String()
	rng := block.DefRange()
	if len(block.Labels) != 1 {
		diags.Errorf(label, &rng, "%s has a dynamic block without a label", w.out.Addr)

		return nil
	}
	path := prefix.Append(addr.A(block.Labels[0]))
	node, ok := w.inv.Lookup(path)
	if !ok || !node.Block {
		diags.Warnf(label, &rng, "%s has a dynamic %s block, which the provider schema does not define", w.out.Addr, path)

		return nil
	}

	forEach, ok := block.Body.Attributes["for_each"]
	if !ok {
		diags.Errorf(label, &rng, "%s: dynamic %s has no for_each", w.out.Addr, path)

		return nil
	}
	iter := block.Labels[0]
	if it, ok := block.Body.Attributes["iterator"]; ok {
		if kw := hcl.ExprAsKeyword(it.Expr); kw != "" {
			iter = kw
		}
	}

	collection := newTracer().trace(sc, forEach.Expr, nil).inexact()
	collection.Conditional = true
	w.record(path, collection)
	internal := append([]Origin(nil), collection.Origins...)

	var content *hclsyntax.Block
	for _, b := range block.Body.Blocks {
		if b.Type == "content" {
			content = b
		}
	}
	if content == nil {
		diags.Errorf(label, &rng, "%s: dynamic %s has no content block", w.out.Addr, path)

		return internal
	}

	inner := scope{inst: sc.inst, env: sc.env.bind(iter, symbol{kind: iterator, collection: forEach.Expr, scope: sc})}

	return append(internal, w.walkBody(inner, content.Body, path.Append(nestingSegments(node.Nesting)...), true, diags)...)
}

func nestingSegments(mode string) []addr.Segment {
	switch mode {
	case "list", "set":
		return []addr.Segment{addr.E()}
	case "map":
		return []addr.Segment{addr.M()}
	default:
		return nil
	}
}

func (w *declWork) record(path addr.Path, f Flow) {
	key := path.String()
	if prev, ok := w.flows[key]; ok {
		prev.flow = prev.flow.union(f)

		return
	}
	w.flows[key] = &fieldFlow{flow: f, set: true}
}

func (w *declWork) classify(exported, internal []Origin) []Field {
	fields := make([]Field, 0, len(w.inv.Nodes))
	for _, n := range w.inv.Nodes {
		f := Field{Node: n}
		ff, set := w.flows[n.Path.String()]

		switch {
		case n.Flags.ComputedOnly():
			f.Input = NotConfigurable
		case !set:
			f.Input = Omitted
		default:
			f.Input, f.Origins = classify(ff.flow)
			if n.Block && f.Input == Fixed {
				f.Input = Present
			}
			f.ForcedKeys = uniqueSorted(ff.flow.ForcedKeys)
			f.Unresolved = uniqueSorted(ff.flow.Unresolved)
			f.Availability = Unconditional
			if ff.flow.Conditional {
				f.Availability = Conditional
			}
		}

		f.Usage = usage(w.out.Addr, n, w.inv, exported, internal)
		f.Review = Automatic
		if f.Input == Omitted || f.Input == Unresolved {
			f.Review = NeedsReview
		}
		fields = append(fields, f)
	}

	markCoupled(fields)

	return fields
}

// classify turns a flow into a mapping and its rendered origins.
func classify(f Flow) (Mapping, []string) {
	origins := originStrings(f)
	switch {
	case len(f.Unresolved) > 0:
		return Unresolved, origins
	case len(f.Origins) == 0:
		return Fixed, nil
	}

	vars := map[string]bool{}
	exact := true
	for _, o := range f.Origins {
		if o.Kind == Produced {
			return Derived, origins
		}
		vars[o.String()] = true
		exact = exact && o.Exact
	}
	if len(vars) == 1 && exact {
		return Direct, origins
	}

	return Transformed, origins
}

// markCoupled flags leaves that one public input drives together: changing it changes all of
// them, which a caller cannot tell from the variable's name alone.
func markCoupled(fields []Field) {
	by := map[string][]int{}
	for i, f := range fields {
		if f.Node.Container || f.Input != Direct || len(f.Origins) != 1 {
			continue
		}
		by[f.Origins[0]] = append(by[f.Origins[0]], i)
	}
	for _, idx := range by {
		if len(idx) < 2 {
			continue
		}
		for _, i := range idx {
			if fields[i].Availability == Unconditional {
				fields[i].Availability = Coupled
			}
		}
	}
}

func usage(decl addr.Declaration, n schema.Node, inv *schema.Inventory, exported, internal []Origin) Usage {
	if !n.Flags.Readable() {
		return NotReadable
	}

	if n.Container {
		var leaves, full, partial int
		for _, l := range inv.Nodes {
			if l.Container || len(l.Path) <= len(n.Path) || !n.Path.Covers(l.Path) || !l.Flags.Readable() {
				continue
			}
			leaves++
			switch leafUsage(decl, l, exported, internal) {
			case Exported:
				full++
			case PartiallyExported:
				partial++
			}
		}
		switch {
		case leaves > 0 && full == leaves:
			return Exported
		case full+partial > 0:
			return PartiallyExported
		}
		if reads(decl, n.Path, internal) {
			return Internal
		}

		return Unused
	}

	return leafUsage(decl, n, exported, internal)
}

func leafUsage(decl addr.Declaration, n schema.Node, exported, internal []Origin) Usage {
	partial := false
	for _, o := range exported {
		if o.Kind != Produced || !sameDecl(o.Decl, decl) {
			continue
		}
		if o.Path.Covers(n.Path) {
			return Exported
		}
		if o.Path.Within(n.Path) {
			partial = true
		}
	}
	if partial {
		return PartiallyExported
	}
	if reads(decl, n.Path, internal) {
		return Internal
	}

	return Unused
}

func reads(decl addr.Declaration, path addr.Path, origins []Origin) bool {
	for _, o := range origins {
		if o.Kind == Produced && sameDecl(o.Decl, decl) && (o.Path.Covers(path) || o.Path.Within(path) || path.Covers(o.Path)) {
			return true
		}
	}

	return false
}

func sameDecl(a, b addr.Declaration) bool {
	return a.String() == b.String()
}

func originStrings(f Flow) []string {
	out := make([]string, 0, len(f.Origins))
	for _, o := range f.Origins {
		out = append(out, o.String())
	}

	return uniqueSorted(out)
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	out := cp[:0]
	for i, s := range cp {
		if i == 0 || s != cp[i-1] {
			out = append(out, s)
		}
	}

	return out
}

// providerConfigs inventories every provider configuration in use. A caller-owned one is listed
// in full and classified caller-owned; a module-owned one is traced like any declaration.
func providerConfigs(tree *module.Tree, set *schema.Set, seen map[string]*ProviderConfig, diags *diag.Diagnostics) []ProviderConfig {
	keys := sortedKeys(seen)
	out := make([]ProviderConfig, 0, len(keys))

	for _, k := range keys {
		pc := seen[k]
		if pc.Owner == module.OwnerUnbound {
			continue
		}
		inv, ok := set.Inventory(pc.Provider, addr.KindProvider, "")
		if !ok {
			pc.SchemaMissing = true
			diags.Errorf(".", nil, "no schema for provider %s: the inventory is incomplete", pc.Provider)
			out = append(out, *pc)
			continue
		}

		if pc.Owner == module.OwnerCaller {
			for _, n := range inv.Nodes {
				pc.Fields = append(pc.Fields, Field{Node: n, Input: CallerOwned, Review: Automatic})
			}
			out = append(out, *pc)
			continue
		}

		inst, decl := providerBlock(tree, pc)
		w := &declWork{
			inst:  inst,
			decl:  decl,
			inv:   inv,
			flows: map[string]*fieldFlow{},
			out:   &Declaration{Addr: addr.Declaration{Module: inst.Path, Kind: addr.KindProvider, Type: decl.Type, Name: decl.Name}},
		}
		w.walkBody(scope{inst: inst}, decl.Body, nil, false, diags)
		pc.Fields = w.classify(nil, nil)
		for i := range pc.Fields {
			pc.Fields[i].Usage = NotReadable
		}
		out = append(out, *pc)
	}

	return out
}

func providerBlock(tree *module.Tree, pc *ProviderConfig) (*module.Instance, *module.Declaration) {
	for _, inst := range tree.Instances {
		if inst.Path.String() != pc.Location {
			continue
		}
		for _, d := range inst.Module.Declarations {
			if d.Kind == addr.KindProvider && inst.Module.Provider(d.Type) == pc.Provider && d.Name == pc.Alias {
				return inst, d
			}
		}
	}

	return tree.Root, &module.Declaration{Kind: addr.KindProvider, Body: &hclsyntax.Body{}}
}
