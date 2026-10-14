// Package analysis traces where the values a module assigns to provider fields come from, and
// where the values its declarations produce go, then classifies every field of every schema in
// scope.
//
// The tracer answers one question - where can the value at this path within this expression
// come from - and never evaluates anything to answer it. It reads no files, runs no functions
// and prints no values; what it cannot follow it says it cannot follow.
package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/adnoctem/pertry/internal/formr/addr"
	"github.com/adnoctem/pertry/internal/formr/module"
)

// OriginKind distinguishes where a value can come from.
type OriginKind int

// Origin kinds.
const (
	// Public is an input variable of the analysed root.
	Public OriginKind = iota
	// Produced is an attribute of a declaration somewhere in the tree.
	Produced
)

// Origin is one source a value can come from.
type Origin struct {
	Kind OriginKind
	// Var is the variable path for a Public origin: the variable name, then any steps into it.
	Var  addr.Path
	Decl addr.Declaration
	// Path is the attribute path within Decl for a Produced origin.
	Path addr.Path
	// Exact is true when the value passes through unchanged.
	Exact bool
}

// String renders the origin symbolically. It never contains a value.
func (o Origin) String() string {
	if o.Kind == Public {
		return "var." + o.Var.String()
	}
	if len(o.Path) == 0 {
		return o.Decl.String()
	}

	return o.Decl.String() + "." + o.Path.String()
}

// Flow is what a trace found.
type Flow struct {
	Origins []Origin
	// Conditional is true when a condition selects between alternatives on the way.
	Conditional bool
	// Unresolved lists what could not be followed. A flow with any entry is never coverage.
	Unresolved []string
	// ForcedKeys are object keys a merge sets regardless of the inputs.
	ForcedKeys []string
}

func (f Flow) union(g Flow) Flow {
	f.Origins = append(append([]Origin(nil), f.Origins...), g.Origins...)
	f.Conditional = f.Conditional || g.Conditional
	f.Unresolved = append(append([]string(nil), f.Unresolved...), g.Unresolved...)
	f.ForcedKeys = append(append([]string(nil), f.ForcedKeys...), g.ForcedKeys...)

	return f
}

func (f Flow) inexact() Flow {
	out := f
	out.Origins = make([]Origin, len(f.Origins))
	for i, o := range f.Origins {
		o.Exact = false
		out.Origins[i] = o
	}

	return out
}

func unresolved(format string, args ...any) Flow {
	return Flow{Unresolved: []string{fmt.Sprintf(format, args...)}}
}

// symbolKind is what a name bound by an iteration refers to.
type symbolKind int

const (
	// iterator is `each`, or a dynamic block's iterator: it has `.value` and `.key`.
	iterator symbolKind = iota
	// elemValue is a `for` expression's value symbol.
	elemValue
	// elemKey is a `for` expression's key symbol.
	elemKey
)

type symbol struct {
	kind symbolKind
	// collection is the iterated expression, traced in its own scope.
	collection hcl.Expression
	scope      scope
}

// env is a chain of names bound by iterations, innermost first.
type env struct {
	parent *env
	name   string
	sym    symbol
}

func (e *env) bind(name string, sym symbol) *env { return &env{parent: e, name: name, sym: sym} }

func (e *env) lookup(name string) (symbol, bool) {
	for cur := e; cur != nil; cur = cur.parent {
		if cur.name == name {
			return cur.sym, true
		}
	}

	return symbol{}, false
}

type scope struct {
	inst *module.Instance
	env  *env
}

// maxDepth bounds a trace that the cycle guard has not already stopped.
const maxDepth = 256

// tracer holds the cycle guard for one top-level trace.
type tracer struct {
	visiting map[string]bool
	depth    int
	// control also follows what selects a value - conditions, index keys, filters - and not only
	// where the value comes from. It answers "what does this depend on", which is the question
	// for count and for_each, and the wrong one for a field's mapping: a variable that only gates
	// creation does not configure the fields of what it creates.
	control bool
}

func newTracer() *tracer { return &tracer{visiting: map[string]bool{}} }

// Trace is the entry point: where can the value at path within e, in inst, come from?
func Trace(inst *module.Instance, e hcl.Expression, path addr.Path) Flow {
	return newTracer().trace(scope{inst: inst}, e, path)
}

// Dependencies returns everything e depends on, including what only selects or gates its value.
func Dependencies(inst *module.Instance, e hcl.Expression) Flow {
	t := newTracer()
	t.control = true

	return t.trace(scope{inst: inst}, e, nil).inexact()
}

func (t *tracer) trace(sc scope, e hcl.Expression, path addr.Path) Flow {
	if e == nil {
		return Flow{}
	}
	t.depth++
	defer func() { t.depth-- }()
	if t.depth > maxDepth {
		return unresolved("expression nesting exceeds %d levels", maxDepth)
	}

	switch x := e.(type) {
	case *hclsyntax.LiteralValueExpr:
		return Flow{}
	case *hclsyntax.TemplateExpr:
		if x.IsStringLiteral() {
			return Flow{}
		}
		var f Flow
		for _, part := range x.Parts {
			f = f.union(t.trace(sc, part, nil))
		}

		return f.inexact()
	case *hclsyntax.TemplateWrapExpr:
		return t.trace(sc, x.Wrapped, path)
	case *hclsyntax.TemplateJoinExpr:
		return t.trace(sc, x.Tuple, nil).inexact()
	case *hclsyntax.ParenthesesExpr:
		return t.trace(sc, x.Expression, path)
	case *hclsyntax.ScopeTraversalExpr:
		return t.traversal(sc, x.Traversal, path)
	case *hclsyntax.RelativeTraversalExpr:
		return t.trace(sc, x.Source, steps(x.Traversal).Append(path...))
	case *hclsyntax.IndexExpr:
		f := t.trace(sc, x.Collection, addr.Path{keySegment(x.Key)}.Append(path...))
		if t.control {
			f = f.union(t.trace(sc, x.Key, nil).inexact())
		}

		return f
	case *hclsyntax.SplatExpr:
		return t.splat(sc, x, path)
	case *hclsyntax.ConditionalExpr:
		f := t.trace(sc, x.TrueResult, path).union(t.trace(sc, x.FalseResult, path))
		if t.control {
			f = f.union(t.trace(sc, x.Condition, nil).inexact())
		}
		f.Conditional = true

		return f
	case *hclsyntax.TupleConsExpr:
		var f Flow
		if len(path) > 0 && path[0].Kind != addr.Attr {
			for _, el := range x.Exprs {
				f = f.union(t.trace(sc, el, path[1:]))
			}

			return f
		}
		for _, el := range x.Exprs {
			f = f.union(t.trace(sc, el, nil))
		}

		return f.inexact()
	case *hclsyntax.ObjectConsExpr:
		return t.object(sc, x, path)
	case *hclsyntax.ObjectConsKeyExpr:
		return t.trace(sc, x.Wrapped, path)
	case *hclsyntax.FunctionCallExpr:
		return t.call(sc, x, path)
	case *hclsyntax.BinaryOpExpr:
		return t.trace(sc, x.LHS, nil).union(t.trace(sc, x.RHS, nil)).inexact()
	case *hclsyntax.UnaryOpExpr:
		return t.trace(sc, x.Val, nil).inexact()
	case *hclsyntax.ForExpr:
		return t.forExpr(sc, x, path)
	default:
		return unresolved("expression of type %T is not modelled", e)
	}
}

// steps converts traversal steps into a path. A string index selects by key, which in an
// object is an attribute; any other index selects an element.
func steps(trav hcl.Traversal) addr.Path {
	var out addr.Path
	for _, step := range trav {
		switch s := step.(type) {
		case hcl.TraverseAttr:
			out = append(out, addr.A(s.Name))
		case hcl.TraverseIndex:
			if s.Key.Type() == cty.String && s.Key.IsKnown() && !s.Key.IsNull() {
				out = append(out, addr.A(s.Key.AsString()))
			} else {
				out = append(out, addr.E())
			}
		default:
			out = append(out, addr.E())
		}
	}

	return out
}

func keySegment(key hclsyntax.Expression) addr.Segment {
	if lit, ok := key.(*hclsyntax.LiteralValueExpr); ok && lit.Val.Type() == cty.String && !lit.Val.IsNull() {
		return addr.A(lit.Val.AsString())
	}
	if tpl, ok := key.(*hclsyntax.TemplateExpr); ok && tpl.IsStringLiteral() {
		if v, d := tpl.Value(nil); !d.HasErrors() && v.Type() == cty.String {
			return addr.A(v.AsString())
		}
	}

	return addr.E()
}

func (t *tracer) splat(sc scope, x *hclsyntax.SplatExpr, path addr.Path) Flow {
	var each addr.Path
	switch e := x.Each.(type) {
	case *hclsyntax.AnonSymbolExpr:
	case *hclsyntax.RelativeTraversalExpr:
		if _, ok := e.Source.(*hclsyntax.AnonSymbolExpr); !ok {
			return unresolved("splat with a computed projection is not modelled")
		}
		each = steps(e.Traversal)
	default:
		return unresolved("splat with a computed projection is not modelled")
	}

	// The splat is a list of projections; an element step on it selects one source element.
	rest := path
	if len(rest) > 0 && rest[0].Kind != addr.Attr {
		rest = rest[1:]
	}

	return t.trace(sc, x.Source, addr.Path{addr.E()}.Append(each...).Append(rest...))
}

func (t *tracer) object(sc scope, x *hclsyntax.ObjectConsExpr, path addr.Path) Flow {
	if len(path) == 0 {
		var f Flow
		for _, item := range x.Items {
			f = f.union(t.trace(sc, item.ValueExpr, nil))
		}

		return f.inexact()
	}

	var f Flow
	for _, item := range x.Items {
		key, literal := literalKey(item.KeyExpr)
		switch {
		case !literal:
			// A computed key could be any key; its value may be the one asked for.
			f = f.union(t.trace(sc, item.ValueExpr, path[1:]))
			f.Conditional = true
		case path[0].Kind != addr.Attr || key == path[0].Name:
			f = f.union(t.trace(sc, item.ValueExpr, path[1:]))
		}
	}

	return f
}

func literalKey(e hclsyntax.Expression) (string, bool) {
	if k, ok := e.(*hclsyntax.ObjectConsKeyExpr); ok {
		if name := hcl.ExprAsKeyword(k.Wrapped); name != "" && !k.ForceNonLiteral {
			return name, true
		}
		e = k.Wrapped
	}
	v, d := e.Value(nil)
	if d.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return "", false
	}

	return v.AsString(), true
}

func (t *tracer) forExpr(sc scope, x *hclsyntax.ForExpr, path addr.Path) Flow {
	inner := sc
	if x.KeyVar != "" {
		inner.env = inner.env.bind(x.KeyVar, symbol{kind: elemKey, collection: x.CollExpr, scope: sc})
	}
	inner.env = inner.env.bind(x.ValVar, symbol{kind: elemValue, collection: x.CollExpr, scope: sc})

	var f Flow
	if len(path) > 0 {
		f = t.trace(inner, x.ValExpr, path[1:])
	} else {
		f = t.trace(inner, x.ValExpr, nil).inexact()
	}
	if x.KeyExpr != nil {
		f = f.union(t.trace(inner, x.KeyExpr, nil).inexact())
	}
	if x.CondExpr != nil {
		f.Conditional = true
		if t.control {
			f = f.union(t.trace(inner, x.CondExpr, nil).inexact())
		}
	}
	if t.control {
		f = f.union(t.trace(sc, x.CollExpr, nil).inexact())
	}

	return f
}

func (t *tracer) traversal(sc scope, trav hcl.Traversal, path addr.Path) Flow {
	root := trav.RootName()
	rest := steps(trav[1:])

	if sym, ok := sc.env.lookup(root); ok {
		return t.symbol(sym, root, rest, path)
	}

	switch root {
	case "var":
		return t.variable(sc, rest, path)
	case "local":
		return t.local(sc, rest, path)
	case "module":
		return t.moduleOutput(sc, rest, path)
	case "data":
		if len(rest) < 2 {
			return unresolved("incomplete data source reference")
		}

		return t.declaration(sc, addr.KindData, rest[0].Name, rest[1].Name, rest[2:], path)
	case "count", "path", "terraform":
		// count.index, path.module and terraform.workspace are fixed by the run, not inputs.
		return Flow{}
	case "each":
		return unresolved("each used outside a for_each")
	case "self":
		return unresolved("self is not modelled")
	default:
		if len(rest) < 1 {
			return unresolved("incomplete reference %s", root)
		}

		return t.declaration(sc, addr.KindResource, root, rest[0].Name, rest[1:], path)
	}
}

func (t *tracer) symbol(sym symbol, root string, rest, path addr.Path) Flow {
	switch sym.kind {
	case elemValue:
		return t.trace(sym.scope, sym.collection, addr.Path{addr.E()}.Append(rest...).Append(path...))
	case elemKey:
		return t.trace(sym.scope, sym.collection, nil).inexact()
	}

	if len(rest) == 0 {
		return unresolved("%s used without .value or .key", root)
	}
	switch rest[0].Name {
	case "value":
		return t.trace(sym.scope, sym.collection, addr.Path{addr.E()}.Append(rest[1:]...).Append(path...))
	case "key":
		return t.trace(sym.scope, sym.collection, nil).inexact()
	default:
		return unresolved("%s.%s is not an iterator attribute", root, rest[0].Name)
	}
}

func (t *tracer) variable(sc scope, rest, path addr.Path) Flow {
	if len(rest) == 0 || rest[0].Kind != addr.Attr {
		return unresolved("incomplete variable reference")
	}
	name := rest[0].Name

	if sc.inst.Parent == nil {
		if _, ok := sc.inst.Module.Variables[name]; !ok {
			return unresolved("reference to undeclared variable %s", name)
		}

		return Flow{Origins: []Origin{{Kind: Public, Var: rest.Append(path...), Exact: true}}}
	}

	arg, ok := sc.inst.Call.Arguments[name]
	if !ok {
		// Not passed: the child's own default applies, which no public input reaches.
		return Flow{}
	}

	parent := scope{inst: sc.inst.Parent}
	if sc.inst.Call.ForEach != nil {
		parent.env = parent.env.bind("each", symbol{kind: iterator, collection: sc.inst.Call.ForEach, scope: scope{inst: sc.inst.Parent}})
	}

	return t.guarded(fmt.Sprintf("%p|arg|%s|%s", sc.inst, name, rest[1:].Append(path...)), func() Flow {
		return t.trace(parent, arg, rest[1:].Append(path...))
	})
}

func (t *tracer) local(sc scope, rest, path addr.Path) Flow {
	if len(rest) == 0 || rest[0].Kind != addr.Attr {
		return unresolved("incomplete local reference")
	}
	expr, ok := sc.inst.Module.Locals[rest[0].Name]
	if !ok {
		return unresolved("reference to undeclared local %s", rest[0].Name)
	}

	return t.guarded(fmt.Sprintf("%p|local|%s|%s", sc.inst, rest[0].Name, rest[1:].Append(path...)), func() Flow {
		return t.trace(scope{inst: sc.inst}, expr, rest[1:].Append(path...))
	})
}

func (t *tracer) moduleOutput(sc scope, rest, path addr.Path) Flow {
	if len(rest) == 0 || rest[0].Kind != addr.Attr {
		return unresolved("incomplete module reference")
	}
	child, ok := sc.inst.Children[rest[0].Name]
	if !ok {
		return unresolved("module.%s is not analysed", rest[0].Name)
	}
	rest = rest[1:]
	if child.Call.Count != nil || child.Call.ForEach != nil {
		if len(rest) == 0 {
			return unresolved("module.%s used as a whole collection is not modelled", child.Call.Name)
		}
		rest = rest[1:]
	}

	if len(rest) == 0 {
		var f Flow
		for _, name := range sortedKeys(child.Module.Outputs) {
			f = f.union(t.output(child, name, path))
		}

		return f
	}

	return t.output(child, rest[0].Name, rest[1:].Append(path...))
}

func (t *tracer) output(child *module.Instance, name string, path addr.Path) Flow {
	out, ok := child.Module.Outputs[name]
	if !ok || out.Value == nil {
		return unresolved("%s has no output %s", child.Path, name)
	}

	return t.guarded(fmt.Sprintf("%p|output|%s|%s", child, name, path), func() Flow {
		return t.trace(scope{inst: child}, out.Value, path)
	})
}

func (t *tracer) declaration(sc scope, kind addr.Kind, typ, name string, rest, path addr.Path) Flow {
	var decl *module.Declaration
	for _, d := range sc.inst.Module.Declarations {
		if d.Kind == kind && d.Type == typ && d.Name == name {
			decl = d
			break
		}
	}
	if decl == nil {
		prefix := ""
		if kind == addr.KindData {
			prefix = "data."
		}

		return unresolved("reference to undeclared %s%s.%s", prefix, typ, name)
	}
	// The first step on a counted declaration selects an instance, not a field.
	if decl.Conditional() && len(rest) > 0 {
		rest = rest[1:]
	}

	return Flow{Origins: []Origin{{
		Kind:  Produced,
		Decl:  addr.Declaration{Module: sc.inst.Path, Kind: kind, Type: typ, Name: name},
		Path:  rest.Append(path...),
		Exact: true,
	}}}
}

func (t *tracer) guarded(key string, fn func() Flow) Flow {
	if t.visiting[key] {
		return unresolved("reference cycle")
	}
	t.visiting[key] = true
	defer delete(t.visiting, key)

	return fn()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}

// Function families. The lists are deliberately explicit: a function not named here is
// unresolved, so that a new built-in cannot be silently mistaken for a pass-through.
var (
	// selecting functions return one of their arguments unchanged.
	selecting = set("try", "coalesce", "sensitive", "nonsensitive")
	// passThrough functions convert a collection's type without changing its values.
	passThrough = set("tolist", "toset", "tomap")
	// elementOf functions return one element of their first argument.
	elementOf = set("one", "element")
	// transforming functions compute a new value from their arguments.
	transforming = set(
		"abs", "alltrue", "anytrue", "base64decode", "base64encode", "base64gzip", "can", "ceil", "chomp",
		"chunklist", "cidrhost", "cidrnetmask", "cidrsubnet", "cidrsubnets", "coalescelist", "compact",
		"contains", "distinct", "endswith", "flatten", "floor", "format", "formatdate", "formatlist",
		"indent", "index", "join", "jsondecode", "jsonencode", "keys", "length", "log", "lower", "matchkeys",
		"max", "md5", "min", "parseint", "pow", "range", "regex", "regexall", "replace", "reverse",
		"setintersection", "setproduct", "setsubtract", "setunion", "sha1", "sha256", "sha512", "signum",
		"slice", "sort", "split", "startswith", "strcontains", "strrev", "substr", "sum", "textdecodebase64",
		"textencodebase64", "timeadd", "timecmp", "title", "tobool", "tonumber", "tostring", "transpose",
		"trim", "trimprefix", "trimspace", "trimsuffix", "upper", "urlencode", "values", "yamldecode",
		"yamlencode", "zipmap",
	)
	// readsFiles functions are never evaluated: formr does not read the module's files.
	readsFiles = set(
		"abspath", "file", "filebase64", "filebase64sha256", "filebase64sha512", "fileexists", "filemd5",
		"fileset", "filesha1", "filesha256", "filesha512", "templatefile",
	)
)

func set(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}

	return out
}

func (t *tracer) call(sc scope, x *hclsyntax.FunctionCallExpr, path addr.Path) Flow {
	name := x.Name
	if strings.Contains(name, "::") {
		return unresolved("provider function %s is not modelled", name)
	}

	switch {
	case selecting[name]:
		var f Flow
		for _, a := range x.Args {
			f = f.union(t.trace(sc, a, path))
		}

		return f
	case passThrough[name]:
		if len(x.Args) != 1 {
			return unresolved("%s with %d arguments", name, len(x.Args))
		}

		return t.trace(sc, x.Args[0], path)
	case elementOf[name]:
		if len(x.Args) == 0 {
			return unresolved("%s without arguments", name)
		}

		return t.trace(sc, x.Args[0], addr.Path{addr.E()}.Append(path...))
	case name == "lookup":
		if len(x.Args) < 2 {
			return unresolved("lookup with %d arguments", len(x.Args))
		}
		f := t.trace(sc, x.Args[0], addr.Path{keySegment(x.Args[1])}.Append(path...))
		if len(x.Args) > 2 {
			f = f.union(t.trace(sc, x.Args[2], path))
		}

		return f
	case name == "merge":
		return t.merge(sc, x, path)
	case name == "concat":
		var f Flow
		if len(path) > 0 && path[0].Kind != addr.Attr {
			for _, a := range x.Args {
				f = f.union(t.trace(sc, a, path))
			}

			return f
		}
		for _, a := range x.Args {
			f = f.union(t.trace(sc, a, nil))
		}

		return f.inexact()
	case transforming[name]:
		var f Flow
		for _, a := range x.Args {
			f = f.union(t.trace(sc, a, nil))
		}

		return f.inexact()
	case readsFiles[name]:
		return unresolved("function %s reads files and is not evaluated", name)
	default:
		return unresolved("function %s is not modelled", name)
	}
}

// merge unions its arguments. Literal keys in an object argument are recorded as forced: the
// module sets them whatever the caller passes, which `merge(var.labels, {owner = "x"})` makes
// easy to miss.
func (t *tracer) merge(sc scope, x *hclsyntax.FunctionCallExpr, path addr.Path) Flow {
	var f Flow
	for _, a := range x.Args {
		if len(path) > 0 {
			f = f.union(t.trace(sc, a, path))
		} else {
			f = f.union(t.trace(sc, a, nil).inexact())
		}

		obj, ok := a.(*hclsyntax.ObjectConsExpr)
		if !ok {
			continue
		}
		for _, item := range obj.Items {
			if key, literal := literalKey(item.KeyExpr); literal && (len(path) == 0 || path[0].Kind != addr.Attr || path[0].Name == key) {
				f.ForcedKeys = append(f.ForcedKeys, key)
			}
		}
	}

	return f
}
