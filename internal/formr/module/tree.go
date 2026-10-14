package module

import (
	"path/filepath"
	"sort"

	"github.com/adnoctem/pertry/internal/formr/addr"
	"github.com/adnoctem/pertry/internal/formr/diag"
)

// Owner says where a provider configuration comes from.
type Owner int

// Owners. A reusable module declares no provider blocks: its configuration is the caller's.
const (
	OwnerCaller Owner = iota
	OwnerModule
	OwnerUnbound
)

// Binding is the provider configuration one declaration resolves to.
type Binding struct {
	Provider addr.Provider
	Alias    string
	Owner    Owner
	// Instance and Decl locate the provider block when Owner is OwnerModule.
	Instance *Instance
	Decl     *Declaration
}

// Instance is one module in the tree: the root, or one call of a child. Two calls to the same
// directory are two instances sharing a *Module, and never share a scope.
type Instance struct {
	Path     addr.ModulePath
	Module   *Module
	Call     *Call
	Parent   *Instance
	Children map[string]*Instance
}

// Tree is the analysed root and every reachable local child.
type Tree struct {
	Root *Instance
	// Instances lists every instance in path order.
	Instances []*Instance
}

// LoadTree loads the module at root and every local child module it calls. A remote source is a
// diagnostic, and so is a cycle: both make the inventory incomplete rather than smaller.
func LoadTree(root string) (*Tree, diag.Diagnostics) {
	abs, err := filepath.Abs(root)
	if err != nil {
		var diags diag.Diagnostics
		diags.Errorf(".", nil, "cannot resolve %s: %v", root, err)

		return nil, diags
	}

	l := &loader{root: abs, cache: map[string]*Module{}}
	t := &Tree{}
	t.Root = l.instance(nil, nil, addr.ModulePath{}, abs, nil)
	walk(t.Root, func(i *Instance) { t.Instances = append(t.Instances, i) })

	return t, l.diags
}

type loader struct {
	root  string
	cache map[string]*Module
	diags diag.Diagnostics
}

func (l *loader) instance(parent *Instance, call *Call, path addr.ModulePath, dir string, stack []string) *Instance {
	m, ok := l.cache[dir]
	if !ok {
		var d diag.Diagnostics
		m, d = Load(l.root, dir, path.String())
		l.diags = append(l.diags, d...)
		l.cache[dir] = m
	}

	inst := &Instance{Path: path, Module: m, Call: call, Parent: parent, Children: map[string]*Instance{}}
	stack = append(stack, dir)

	for _, c := range m.Calls {
		label := path.String()
		rng := c.Range
		if !isLocalSource(c.Source) {
			l.diags.Errorf(label, &rng, "module %q: source %q is not a local path and is not analysed", c.Name, c.Source)
			continue
		}
		child := filepath.Clean(filepath.Join(dir, filepath.FromSlash(c.Source)))
		if contains(stack, child) {
			l.diags.Errorf(label, &rng, "module %q: source %q forms a cycle", c.Name, c.Source)
			continue
		}
		inst.Children[c.Name] = l.instance(inst, c, path.Child(c.Name), child, stack)
	}

	return inst
}

func contains(stack []string, dir string) bool {
	for _, s := range stack {
		if s == dir {
			return true
		}
	}

	return false
}

func walk(i *Instance, fn func(*Instance)) {
	fn(i)
	names := make([]string, 0, len(i.Children))
	for name := range i.Children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		walk(i.Children[name], fn)
	}
}

// Bind resolves a provider reference made in this instance to its configuration, following
// `providers` mappings and Terraform's implicit inheritance of default configurations.
func (i *Instance) Bind(ref ProviderRef) Binding {
	provider := i.Module.Provider(ref.LocalName)

	for _, d := range i.Module.Declarations {
		if d.Kind == addr.KindProvider && d.Type == ref.LocalName && d.Name == ref.Alias {
			return Binding{Provider: provider, Alias: ref.Alias, Owner: OwnerModule, Instance: i, Decl: d}
		}
	}

	if i.Parent == nil {
		if ref.Alias == "" || containsString(i.Module.RequiredProviders[ref.LocalName].ConfigurationAliases, ref.Alias) {
			return Binding{Provider: provider, Alias: ref.Alias, Owner: OwnerCaller}
		}

		return Binding{Provider: provider, Alias: ref.Alias, Owner: OwnerUnbound}
	}

	if parentRef, ok := i.Call.Providers[ref]; ok {
		b := i.Parent.Bind(parentRef)
		b.Provider = provider

		return b
	}

	if ref.Alias != "" {
		return Binding{Provider: provider, Alias: ref.Alias, Owner: OwnerUnbound}
	}

	// Implicit inheritance is by provider address: the parent may know it by another local name.
	parentName := ref.LocalName
	names := make([]string, 0, len(i.Parent.Module.RequiredProviders))
	for name := range i.Parent.Module.RequiredProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if i.Parent.Module.RequiredProviders[name].Source == provider {
			parentName = name
			break
		}
	}
	b := i.Parent.Bind(ProviderRef{LocalName: parentName})
	b.Provider = provider

	return b
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}

	return false
}
