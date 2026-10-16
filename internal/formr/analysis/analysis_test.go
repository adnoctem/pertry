package analysis_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/formr/addr"
	"github.com/adnoctem/pertry/internal/formr/analysis"
	"github.com/adnoctem/pertry/internal/formr/formrtest"
	"github.com/adnoctem/pertry/internal/formr/module"
	"github.com/adnoctem/pertry/internal/formr/schema"
)

func analyse(t *testing.T, files map[string]string) *analysis.Result {
	t.Helper()

	tree, diags := module.LoadTree(formrtest.WriteModule(t, files))
	if tree == nil {
		t.Fatalf("load: %v", diags)
	}
	set, err := schema.Decode([]byte(formrtest.Schema))
	if err != nil {
		t.Fatal(err)
	}
	res := analysis.Analyze(tree, set)
	res.Diagnostics = append(diags, res.Diagnostics...)

	return res
}

func declaration(t *testing.T, res *analysis.Result, address string) analysis.Declaration {
	t.Helper()
	for _, d := range res.Declarations {
		if d.Addr.String() == address {
			return d
		}
	}
	t.Fatalf("no declaration %s", address)

	return analysis.Declaration{}
}

func field(t *testing.T, d analysis.Declaration, path string) analysis.Field {
	t.Helper()
	for _, f := range d.Fields {
		if f.Node.Path.String() == path {
			return f
		}
	}
	t.Fatalf("%s has no field %s", d.Addr, path)

	return analysis.Field{}
}

type want struct {
	input   analysis.Mapping
	origins []string
	avail   analysis.Availability
	usage   analysis.Usage
}

func check(t *testing.T, d analysis.Declaration, path string, w want) {
	t.Helper()
	f := field(t, d, path)
	got := want{input: f.Input, origins: f.Origins, avail: f.Availability, usage: f.Usage}
	if diff := cmp.Diff(w, got, cmp.AllowUnexported(want{})); diff != "" {
		t.Errorf("%s %s (-want +got):\n%s", d.Addr, path, diff)
	}
}

func TestServerFields(t *testing.T) {
	res := analyse(t, formrtest.Main)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	srv := declaration(t, res, "acme_server.this")

	check(t, srv, "name", want{analysis.Direct, []string{"var.name"}, analysis.Unconditional, analysis.Unused})
	check(t, srv, "size", want{analysis.Direct, []string{"var.size"}, analysis.Unconditional, analysis.Unused})
	check(t, srv, "labels", want{analysis.Transformed, []string{"var.labels"}, analysis.Unconditional, analysis.Unused})
	// An optional-and-computed field nobody sets is a missing input, and its value is exported.
	check(t, srv, "id", want{analysis.Omitted, nil, analysis.NotSet, analysis.Exported})
	// A computed-only field is never a missing input.
	check(t, srv, "ip", want{analysis.NotConfigurable, nil, analysis.NotSet, analysis.Unused})
	// A write-only field cannot be read back, so it has no result usage at all.
	check(t, srv, "secret", want{analysis.Direct, []string{"var.secret"}, analysis.Unconditional, analysis.NotReadable})

	// The custom iterator name resolves to the dynamic block's for_each.
	check(t, srv, "network[].ip", want{analysis.Direct, []string{"var.networks[].ip"}, analysis.Conditional, analysis.Exported})
	check(t, srv, "network[].network_id", want{analysis.Direct, []string{"var.networks[].network_id"}, analysis.Conditional, analysis.Unused})
	// Exporting network[*].ip does not export the MAC address.
	check(t, srv, "network[].mac", want{analysis.NotConfigurable, nil, analysis.NotSet, analysis.Unused})
	check(t, srv, "network", want{analysis.Transformed, []string{"var.networks"}, analysis.Conditional, analysis.PartiallyExported})

	check(t, srv, "settings.mode", want{analysis.Fixed, nil, analysis.Unconditional, analysis.Unused})
	check(t, srv, "settings.level", want{analysis.Transformed, []string{"var.name"}, analysis.Unconditional, analysis.Unused})
	check(t, srv, "timeouts", want{analysis.Omitted, nil, analysis.NotSet, analysis.Unused})
	check(t, srv, "timeouts.create", want{analysis.Omitted, nil, analysis.NotSet, analysis.Unused})
}

// A variable that only gates creation does not configure the fields of what it creates: it is
// reported once, on the declaration, and never as a field's origin.
func TestCreationToggleIsNotAValueInput(t *testing.T) {
	res := analyse(t, formrtest.Main)
	srv := declaration(t, res, "acme_server.this")

	if !srv.InstancesConditional || !cmp.Equal(srv.Instances, []string{"var.create"}) {
		t.Errorf("instances = %v (conditional %v), want conditional on var.create", srv.Instances, srv.InstancesConditional)
	}
	for _, f := range srv.Fields {
		for _, o := range f.Origins {
			if o == "var.create" {
				t.Errorf("%s lists the creation toggle as an origin", f.Node.Path)
			}
		}
	}
}

func TestMergeForcedKeys(t *testing.T) {
	res := analyse(t, formrtest.Main)
	f := field(t, declaration(t, res, "acme_server.this"), "labels")
	if !cmp.Equal(f.ForcedKeys, []string{"managed_by"}) {
		t.Errorf("forced keys = %v, want [managed_by]", f.ForcedKeys)
	}
}

// Two declarations of one type are reported separately, so a field set on one cannot hide that
// it is missing on the other.
func TestDeclarationsAreNotMergedByType(t *testing.T) {
	res := analyse(t, formrtest.Main)
	check(t, declaration(t, res, "acme_volume.a"), "size", want{analysis.Fixed, nil, analysis.Unconditional, analysis.Unused})
	check(t, declaration(t, res, "acme_volume.b"), "size", want{analysis.Omitted, nil, analysis.NotSet, analysis.Unused})
}

// Two calls to one source are two scopes: an argument passed to one does not reach the other.
func TestRepeatedModuleCallsKeepTheirScopes(t *testing.T) {
	res := analyse(t, formrtest.Main)
	check(t, declaration(t, res, "module.first.acme_volume.c"), "name",
		want{analysis.Direct, []string{"var.name"}, analysis.Unconditional, analysis.Unused})
	// Not passed: the child's default applies, which no public input reaches.
	check(t, declaration(t, res, "module.second.acme_volume.c"), "name",
		want{analysis.Fixed, nil, analysis.Unconditional, analysis.Unused})
}

func TestDataSourceResults(t *testing.T) {
	res := analyse(t, formrtest.Main)
	img := declaration(t, res, "data.acme_image.os")
	check(t, img, "name", want{analysis.Fixed, nil, analysis.Unconditional, analysis.Unused})
	check(t, img, "id", want{analysis.NotConfigurable, nil, analysis.NotSet, analysis.Exported})
	check(t, img, "zones", want{analysis.NotConfigurable, nil, analysis.NotSet, analysis.Unused})
	check(t, img, "zones[].name", want{analysis.NotConfigurable, nil, analysis.NotSet, analysis.Unused})
}

// Only the types the module declares are inventoried, never the provider's whole catalogue.
func TestUnusedProviderTypesAreNotInventoried(t *testing.T) {
	res := analyse(t, formrtest.Main)
	for _, d := range res.Declarations {
		if d.Addr.Type == "acme_unused" {
			t.Errorf("acme_unused was inventoried")
		}
	}
}

func TestCallerOwnedProviderConfiguration(t *testing.T) {
	res := analyse(t, formrtest.Main)
	if len(res.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(res.Providers))
	}
	p := res.Providers[0]
	if p.Owner != module.OwnerCaller {
		t.Errorf("owner = %v, want caller", p.Owner)
	}
	for _, f := range p.Fields {
		if f.Input != analysis.CallerOwned {
			t.Errorf("%s: input %q, want caller-owned", f.Node.Path, f.Input)
		}
	}
}

func TestUnboundAliasIsAnError(t *testing.T) {
	files := formrtest.With(formrtest.Main, map[string]string{
		"alias.tf": `
resource "acme_volume" "aliased" {
  provider = acme.elsewhere
  name     = "x"
}
`,
	})
	res := analyse(t, files)
	if !res.Diagnostics.HasErrors() || !strings.Contains(res.Diagnostics[0].Summary, "nothing configures") {
		t.Errorf("diagnostics = %v, want an unbound-configuration error", res.Diagnostics)
	}
}

func TestConfigurationAliasesAndProviderMappings(t *testing.T) {
	files := formrtest.With(formrtest.Main, map[string]string{
		"providers.tf": `
provider "acme" {
  alias    = "west"
  endpoint = "SYNTHETIC-ENDPOINT"
  token    = var.name
}

module "aliased" {
  source    = "./aliased"
  providers = { acme.target = acme.west }
}
`,
		"aliased/main.tf": `
terraform {
  required_providers {
    acme = {
      source                = "acme/acme"
      configuration_aliases = [acme.target]
    }
  }
}

resource "acme_volume" "d" {
  provider = acme.target
  name     = "d"
}
`,
	})
	res := analyse(t, files)
	if res.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %v", res.Diagnostics)
	}

	var west *analysis.ProviderConfig
	for i := range res.Providers {
		if res.Providers[i].Alias == "west" {
			west = &res.Providers[i]
		}
	}
	if west == nil || west.Owner != module.OwnerModule || west.Location != "." {
		t.Fatalf("providers = %+v, want acme.west configured in .", res.Providers)
	}
	for _, f := range west.Fields {
		switch f.Node.Path.String() {
		case "token":
			if f.Input != analysis.Direct {
				t.Errorf("token input = %q, want direct", f.Input)
			}
		case "endpoint":
			if f.Input != analysis.Fixed {
				t.Errorf("endpoint input = %q, want fixed", f.Input)
			}
		}
	}
}

func TestUnsupportedConstructsAreErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"json syntax":   {"extra.tf.json": `{}`},
		"override file": {"override.tf": "# empty\n"},
		"remote module": {"remote.tf": `
module "remote" {
  source = "hashicorp/consul/aws"
}
`},
		"unknown function": {"fn.tf": `
resource "acme_volume" "f" {
  name = file("SYNTHETIC-PATH")
}
`},
	}

	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			res := analyse(t, formrtest.With(formrtest.Main, extra))
			if name == "unknown function" {
				f := field(t, declaration(t, res, "acme_volume.f"), "name")
				if f.Input != analysis.Unresolved || len(f.Unresolved) == 0 {
					t.Errorf("input = %q (%v), want unresolved with a reason", f.Input, f.Unresolved)
				}

				return
			}
			if !res.Diagnostics.HasErrors() {
				t.Errorf("no error diagnostic for %s", name)
			}
		})
	}
}

// A field that is set but not in the schema is reported, not silently dropped: it is usually a
// field the provider removed.
func TestArgumentOutsideTheSchemaIsReported(t *testing.T) {
	res := analyse(t, formrtest.With(formrtest.Main, map[string]string{
		"extra.tf": `
resource "acme_volume" "e" {
  name    = "e"
  removed = true
}
`,
	}))
	found := false
	for _, d := range res.Diagnostics {
		found = found || strings.Contains(d.Summary, "removed")
	}
	if !found {
		t.Errorf("diagnostics = %v, want one naming the unknown argument", res.Diagnostics)
	}
}

// The instance selector of a counted declaration can come from around the reference rather than
// from its own steps. Found on terraform-cloudflare-dns, where `one(resource.this)` outputs left
// every DNSSEC field reported unused.
func TestInstanceSelectorFromTheSurroundingExpression(t *testing.T) {
	res := analyse(t, formrtest.With(formrtest.Main, map[string]string{
		"instances.tf": `
resource "acme_volume" "counted" {
  count = var.create ? 1 : 0
  name  = "counted"
}

resource "acme_volume" "keyed" {
  for_each = toset(["a", "b"])
  name     = each.key
}

module "counted" {
  count  = var.create ? 1 : 0
  source = "./child"
}

output "one_whole" {
  value = one(acme_volume.counted)
}

output "keyed_ids" {
  value = { for k, v in acme_volume.keyed : k => v.id }
}

output "keyed_names" {
  value = values(acme_volume.keyed)[*].name
}

output "child_id" {
  value = one(module.counted).id
}
`,
	}))

	counted := declaration(t, res, "acme_volume.counted")
	for _, path := range []string{"id", "name", "size"} {
		if f := field(t, counted, path); f.Usage != analysis.Exported {
			t.Errorf("acme_volume.counted.%s usage = %q, want exported through one()", path, f.Usage)
		}
	}
	if f := field(t, declaration(t, res, "acme_volume.keyed"), "id"); f.Usage != analysis.Exported {
		t.Errorf("acme_volume.keyed.id usage = %q, want exported through a for over instances", f.Usage)
	}
	if f := field(t, declaration(t, res, "module.counted.acme_volume.c"), "id"); f.Usage != analysis.Exported {
		t.Errorf("module.counted.acme_volume.c.id usage = %q, want exported through one(module...)", f.Usage)
	}
}

func TestOriginRendering(t *testing.T) {
	o := analysis.Origin{Kind: analysis.Produced, Decl: addr.Declaration{Kind: addr.KindResource, Type: "acme_volume", Name: "this"},
		Path: addr.Path{addr.E(), addr.A("id")}}
	if got := o.String(); got != "acme_volume.this[].id" {
		t.Errorf("origin renders %q, want acme_volume.this[].id", got)
	}
}

// OpenTofu resolves an unqualified source such as `acme/acme` against registry.opentofu.org.
// formr assumed registry.terraform.io and found no schema for anything (found running the
// Hetzner server module under OpenTofu 1.13.1).
func TestOpenTofuRegistryHost(t *testing.T) {
	tree, diags := module.LoadTree(formrtest.WriteModule(t, formrtest.Main))
	if tree == nil {
		t.Fatalf("load: %v", diags)
	}
	doc := strings.ReplaceAll(formrtest.Schema, "registry.terraform.io/acme/acme", "registry.opentofu.org/acme/acme")
	set, err := schema.Decode([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	res := analysis.Analyze(tree, set)
	if res.Diagnostics.HasErrors() {
		t.Fatalf("diagnostics: %v", res.Diagnostics)
	}
	if got := declaration(t, res, "acme_server.this").Provider.String(); got != "registry.opentofu.org/acme/acme" {
		t.Errorf("provider = %s, want the OpenTofu registry address", got)
	}
}
