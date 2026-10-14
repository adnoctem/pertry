// Package module reads a Terraform module's native-syntax configuration into declarations,
// module calls and provider bindings, and assembles the tree of local child modules.
//
// It works on hclsyntax bodies directly rather than decoding against a fixed schema, because the
// analysis needs every expression with its structure and range intact, and an unknown block is
// something to report rather than something a decoder should reject or drop.
package module

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/adnoctem/pertry/internal/formr/addr"
	"github.com/adnoctem/pertry/internal/formr/diag"
)

// Variable is a `variable` block.
type Variable struct {
	Name      string
	Sensitive bool
	Range     hcl.Range
}

// Output is an `output` block.
type Output struct {
	Name      string
	Value     hcl.Expression
	Sensitive bool
	Range     hcl.Range
}

// ProviderRef names a provider configuration as written in a module: a local name and an
// optional alias.
type ProviderRef struct {
	LocalName string
	Alias     string
}

func (r ProviderRef) String() string {
	if r.Alias == "" {
		return r.LocalName
	}

	return r.LocalName + "." + r.Alias
}

// Requirement is one `required_providers` entry.
type Requirement struct {
	Source addr.Provider
	// ConfigurationAliases are the aliases the module expects its caller to pass.
	ConfigurationAliases []string
}

// Declaration is a resource, data source or provider block.
type Declaration struct {
	Kind addr.Kind
	Type string
	// Name is the block label; for a provider block it is the alias, or empty.
	Name    string
	Body    *hclsyntax.Body
	Count   hcl.Expression
	ForEach hcl.Expression
	// Provider is the `provider` meta-argument, or the implied local name.
	Provider ProviderRef
	Range    hcl.Range
}

// Conditional reports whether the declaration has instances only under some condition.
func (d *Declaration) Conditional() bool { return d.Count != nil || d.ForEach != nil }

// Call is a `module` block.
type Call struct {
	Name      string
	Source    string
	Count     hcl.Expression
	ForEach   hcl.Expression
	Arguments map[string]hcl.Expression
	// Providers is the `providers` meta-argument: child reference to parent reference.
	Providers map[ProviderRef]ProviderRef
	Range     hcl.Range
}

// Module is one directory of configuration.
type Module struct {
	// Dir is the directory relative to the analysed root, slash-separated; "." for the root.
	Dir               string
	Variables         map[string]*Variable
	Locals            map[string]hcl.Expression
	Outputs           map[string]*Output
	Declarations      []*Declaration
	Calls             []*Call
	RequiredProviders map[string]Requirement
	// Conditions are lifecycle and check conditions, which read declarations without
	// configuring anything: they count as internal use.
	Conditions []hcl.Expression
}

// Provider resolves a local name to its source address.
func (m *Module) Provider(localName string) addr.Provider {
	if req, ok := m.RequiredProviders[localName]; ok {
		return req.Source
	}

	return addr.DefaultProvider(localName)
}

// Declares reports whether the module names localName in required_providers. When it does not,
// Terraform assumes hashicorp/<name>, which for any third-party provider is the wrong one.
func (m *Module) Declares(localName string) bool {
	_, ok := m.RequiredProviders[localName]

	return ok
}

// Load reads every native-syntax file in dir. root is the analysed root, used to make paths in
// diagnostics relative; label is the module path the diagnostics are attributed to.
func Load(root, dir, label string) (*Module, diag.Diagnostics) {
	var diags diag.Diagnostics

	rel, err := filepath.Rel(root, dir)
	if err != nil {
		rel = dir
	}
	m := &Module{
		Dir:               filepath.ToSlash(rel),
		Variables:         map[string]*Variable{},
		Locals:            map[string]hcl.Expression{},
		Outputs:           map[string]*Output{},
		RequiredProviders: map[string]Requirement{},
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		diags.Errorf(label, nil, "cannot read module directory %s: %v", m.Dir, err)

		return m, diags
	}

	// Sorted, so that declaration order and therefore every diagnostic is independent of the
	// order the filesystem happens to return.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		switch {
		case strings.HasSuffix(name, ".tf.json"):
			diags.Errorf(label, nil, "unsupported file %s: JSON configuration syntax is not analysed", path(m.Dir, name))
			continue
		case !strings.HasSuffix(name, ".tf"):
			continue
		case name == "override.tf" || strings.HasSuffix(name, "_override.tf"):
			diags.Errorf(label, nil, "unsupported file %s: override files are not analysed", path(m.Dir, name))
			continue
		}

		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			diags.Errorf(label, nil, "cannot read %s: %v", path(m.Dir, name), err)
			continue
		}

		file, hdiags := hclsyntax.ParseConfig(src, path(m.Dir, name), hcl.Pos{Line: 1, Column: 1})
		diags = append(diags, diag.FromHCL(label, hdiags)...)
		if hdiags.HasErrors() {
			continue
		}

		diags = append(diags, m.addFile(label, file.Body.(*hclsyntax.Body))...)
	}

	return m, diags
}

func path(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}

	return dir + "/" + name
}

func (m *Module) addFile(label string, body *hclsyntax.Body) diag.Diagnostics {
	var diags diag.Diagnostics

	for _, attr := range body.Attributes {
		rng := attr.SrcRange
		diags.Errorf(label, &rng, "unexpected top-level argument %q", attr.Name)
	}

	for _, block := range body.Blocks {
		rng := block.DefRange()
		switch block.Type {
		case "terraform":
			diags = append(diags, m.addTerraform(label, block)...)
		case "variable":
			m.Variables[block.Labels[0]] = &Variable{
				Name:      block.Labels[0],
				Sensitive: literalBool(block.Body, "sensitive"),
				Range:     rng,
			}
			m.addConditions(block.Body)
		case "locals":
			for name, attr := range block.Body.Attributes {
				m.Locals[name] = attr.Expr
			}
		case "output":
			out := &Output{Name: block.Labels[0], Sensitive: literalBool(block.Body, "sensitive"), Range: rng}
			if attr, ok := block.Body.Attributes["value"]; ok {
				out.Value = attr.Expr
			}
			m.Outputs[out.Name] = out
			m.addConditions(block.Body)
		case "resource", "data":
			m.Declarations = append(m.Declarations, m.declaration(block))
		case "provider":
			decl := &Declaration{
				Kind:     addr.KindProvider,
				Type:     block.Labels[0],
				Name:     literalString(block.Body, "alias"),
				Body:     block.Body,
				Provider: ProviderRef{LocalName: block.Labels[0], Alias: literalString(block.Body, "alias")},
				Range:    rng,
			}
			m.Declarations = append(m.Declarations, decl)
		case "module":
			call, cdiags := callFrom(label, block)
			diags = append(diags, cdiags...)
			m.Calls = append(m.Calls, call)
		case "check":
			m.addConditions(block.Body)
		case "moved", "import", "removed":
			// Refactoring and import directives configure no provider fields.
		default:
			diags.Errorf(label, &rng, "unsupported top-level block %q: the analysis does not understand it", block.Type)
		}
	}

	return diags
}

func (m *Module) addTerraform(label string, block *hclsyntax.Block) diag.Diagnostics {
	var diags diag.Diagnostics

	for _, inner := range block.Body.Blocks {
		if inner.Type != "required_providers" {
			continue
		}
		for name, attr := range inner.Body.Attributes {
			rng := attr.SrcRange
			obj, ok := attr.Expr.(*hclsyntax.ObjectConsExpr)
			if !ok {
				// The legacy `name = "version"` form names no source.
				m.RequiredProviders[name] = Requirement{Source: addr.DefaultProvider(name)}
				continue
			}
			req := Requirement{Source: addr.DefaultProvider(name)}
			for _, item := range obj.Items {
				key := objectKey(item.KeyExpr)
				switch key {
				case "source":
					v, d := item.ValueExpr.Value(nil)
					if d.HasErrors() || v.Type() != cty.String || v.IsNull() {
						diags.Errorf(label, &rng, "provider %q has a source that is not a literal string", name)
						continue
					}
					src, err := addr.ParseProvider(v.AsString())
					if err != nil {
						diags.Errorf(label, &rng, "provider %q: %v", name, err)
						continue
					}
					req.Source = src
				case "configuration_aliases":
					tuple, ok := item.ValueExpr.(*hclsyntax.TupleConsExpr)
					if !ok {
						continue
					}
					for _, e := range tuple.Exprs {
						if t, ok := e.(*hclsyntax.ScopeTraversalExpr); ok && len(t.Traversal) == 2 {
							if a, ok := t.Traversal[1].(hcl.TraverseAttr); ok {
								req.ConfigurationAliases = append(req.ConfigurationAliases, a.Name)
							}
						}
					}
				}
			}
			m.RequiredProviders[name] = req
		}
	}

	return diags
}

func (m *Module) declaration(block *hclsyntax.Block) *Declaration {
	kind := addr.KindResource
	if block.Type == "data" {
		kind = addr.KindData
	}

	d := &Declaration{
		Kind:  kind,
		Type:  block.Labels[0],
		Name:  block.Labels[1],
		Body:  block.Body,
		Range: block.DefRange(),
	}
	if attr, ok := block.Body.Attributes["count"]; ok {
		d.Count = attr.Expr
	}
	if attr, ok := block.Body.Attributes["for_each"]; ok {
		d.ForEach = attr.Expr
	}

	d.Provider = ProviderRef{LocalName: impliedLocalName(d.Type)}
	if attr, ok := block.Body.Attributes["provider"]; ok {
		if ref, ok := providerRef(attr.Expr); ok {
			d.Provider = ref
		}
	}

	for _, inner := range block.Body.Blocks {
		if inner.Type == "lifecycle" {
			m.addConditions(inner.Body)
		}
	}

	return d
}

// addConditions collects the expressions of precondition, postcondition and validation blocks.
func (m *Module) addConditions(body *hclsyntax.Body) {
	for _, inner := range body.Blocks {
		switch inner.Type {
		case "precondition", "postcondition", "validation", "assert":
			for _, name := range []string{"condition", "error_message"} {
				if attr, ok := inner.Body.Attributes[name]; ok {
					m.Conditions = append(m.Conditions, attr.Expr)
				}
			}
		}
	}
}

func callFrom(label string, block *hclsyntax.Block) (*Call, diag.Diagnostics) {
	var diags diag.Diagnostics

	c := &Call{
		Name:      block.Labels[0],
		Arguments: map[string]hcl.Expression{},
		Providers: map[ProviderRef]ProviderRef{},
		Range:     block.DefRange(),
	}

	for name, attr := range block.Body.Attributes {
		switch name {
		case "source":
			c.Source = literalString(block.Body, "source")
		case "version", "depends_on":
		case "count":
			c.Count = attr.Expr
		case "for_each":
			c.ForEach = attr.Expr
		case "providers":
			obj, ok := attr.Expr.(*hclsyntax.ObjectConsExpr)
			if !ok {
				rng := attr.SrcRange
				diags.Errorf(label, &rng, "module %q: providers is not an object", c.Name)
				continue
			}
			for _, item := range obj.Items {
				child, ok1 := providerRef(unwrapKey(item.KeyExpr))
				parent, ok2 := providerRef(item.ValueExpr)
				if !ok1 || !ok2 {
					rng := item.KeyExpr.Range()
					diags.Errorf(label, &rng, "module %q: unreadable providers entry", c.Name)
					continue
				}
				c.Providers[child] = parent
			}
		default:
			c.Arguments[name] = attr.Expr
		}
	}

	return c, diags
}

// impliedLocalName is Terraform's rule: the resource type's prefix up to the first underscore.
func impliedLocalName(typ string) string {
	if i := strings.IndexByte(typ, '_'); i > 0 {
		return typ[:i]
	}

	return typ
}

func providerRef(expr hcl.Expression) (ProviderRef, bool) {
	trav, diags := hcl.AbsTraversalForExpr(expr)
	if diags.HasErrors() || len(trav) == 0 || len(trav) > 2 {
		return ProviderRef{}, false
	}

	ref := ProviderRef{LocalName: trav.RootName()}
	if len(trav) == 2 {
		a, ok := trav[1].(hcl.TraverseAttr)
		if !ok {
			return ProviderRef{}, false
		}
		ref.Alias = a.Name
	}

	return ref, true
}

func unwrapKey(expr hclsyntax.Expression) hcl.Expression {
	if k, ok := expr.(*hclsyntax.ObjectConsKeyExpr); ok {
		return k.Wrapped
	}

	return expr
}

func objectKey(expr hclsyntax.Expression) string {
	if k, ok := expr.(*hclsyntax.ObjectConsKeyExpr); ok {
		if name := hcl.ExprAsKeyword(k.Wrapped); name != "" {
			return name
		}
		expr = k.Wrapped
	}

	v, d := expr.Value(nil)
	if d.HasErrors() || v.Type() != cty.String || v.IsNull() {
		return ""
	}

	return v.AsString()
}

func literalString(body *hclsyntax.Body, name string) string {
	attr, ok := body.Attributes[name]
	if !ok {
		return ""
	}
	v, d := attr.Expr.Value(nil)
	if d.HasErrors() || v.Type() != cty.String || v.IsNull() {
		return ""
	}

	return v.AsString()
}

func literalBool(body *hclsyntax.Body, name string) bool {
	attr, ok := body.Attributes[name]
	if !ok {
		return false
	}
	v, d := attr.Expr.Value(nil)
	if d.HasErrors() || v.Type() != cty.Bool || v.IsNull() {
		return false
	}

	return v.True()
}

// isLocalSource reports whether a module source is a local path, the only kind M0 follows.
func isLocalSource(src string) bool {
	return strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../")
}
