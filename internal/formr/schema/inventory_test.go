package schema_test

import (
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/formr/addr"
	"github.com/adnoctem/pertry/internal/formr/formrtest"
	"github.com/adnoctem/pertry/internal/formr/schema"
)

func inventory(t *testing.T, doc string, kind addr.Kind, typ string) *schema.Inventory {
	t.Helper()
	set, err := schema.Decode([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := addr.ParseProvider(formrtest.Provider)
	inv, ok := set.Inventory(p, kind, typ)
	if !ok {
		t.Fatalf("no schema for %s %s", kind, typ)
	}

	return inv
}

func TestInventoryPaths(t *testing.T) {
	inv := inventory(t, formrtest.Schema, addr.KindResource, "acme_server")

	var got []string
	for _, n := range inv.Nodes {
		got = append(got, n.Path.String())
	}
	want := []string{
		"id", "ip", "labels", "name", "network", "network[].ip", "network[].mac", "network[].network_id",
		"secret", "settings", "settings.level", "settings.mode", "size", "timeouts", "timeouts.create",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("paths:\n got %v\nwant %v", got, want)
	}

	// Containers are listed but not counted.
	leaves := len(inv.Leaves())
	if leaves != 12 {
		t.Errorf("leaves = %d, want 12", leaves)
	}
}

func fingerprintOf(t *testing.T, doc, path string) string {
	t.Helper()
	inv := inventory(t, doc, addr.KindResource, "acme_server")
	for _, n := range inv.Nodes {
		if n.Path.String() == path {
			return n.Fingerprint
		}
	}
	t.Fatalf("no node %s", path)

	return ""
}

func TestFingerprints(t *testing.T) {
	base := fingerprintOf(t, formrtest.Schema, "name")

	reworded := strings.Replace(formrtest.Schema, `"description": "Server name."`, `"description": "The server's name."`, 1)
	if got := fingerprintOf(t, reworded, "name"); got != base {
		t.Errorf("a reworded description changed the fingerprint")
	}

	optional := strings.Replace(formrtest.Schema, `"name": {"type": "string", "required": true, "description": "Server name."}`,
		`"name": {"type": "string", "optional": true, "description": "Server name."}`, 1)
	if got := fingerprintOf(t, optional, "name"); got == base {
		t.Errorf("required becoming optional did not change the fingerprint")
	}

	// A key this decoder does not model still changes the fingerprint.
	unknown := strings.Replace(formrtest.Schema, `"name": {"type": "string", "required": true,`,
		`"name": {"type": "string", "required": true, "future_flag": true,`, 1)
	if got := fingerprintOf(t, unknown, "name"); got == base {
		t.Errorf("an unknown key did not change the fingerprint")
	}

	// A change to a nested field changes its container too.
	nested := strings.Replace(formrtest.Schema, `"mac": {"type": "string", "computed": true}`, `"mac": {"type": "number", "computed": true}`, 1)
	if fingerprintOf(t, nested, "network") == fingerprintOf(t, formrtest.Schema, "network") {
		t.Errorf("a nested change did not change the block's fingerprint")
	}
}

func TestUnsupportedFormatIsRejected(t *testing.T) {
	doc := strings.Replace(formrtest.Schema, `"format_version": "1.0"`, `"format_version": "2.0"`, 1)
	if _, err := schema.Decode([]byte(doc)); err == nil {
		t.Error("format 2.0 was accepted")
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	p, _ := addr.ParseProvider(formrtest.Provider)
	prov := schema.Provenance{Engine: schema.EngineInfo{Name: "terraform", Version: "1.16.5"}, Providers: map[addr.Provider]string{p: "1.2.3"}}

	data, err := schema.WriteSnapshot([]byte(formrtest.Schema), prov)
	if err != nil {
		t.Fatal(err)
	}
	raw, got, warnings, err := schema.ReadSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || got.Engine != prov.Engine || got.Providers[p] != "1.2.3" {
		t.Errorf("provenance = %+v, warnings %v", got, warnings)
	}
	if _, err := schema.Decode(raw); err != nil {
		t.Errorf("snapshot schemas do not decode: %v", err)
	}

	// A bare schema document is accepted, and the missing provenance is said out loud.
	_, _, warnings, err = schema.ReadSnapshot([]byte(formrtest.Schema))
	if err != nil || len(warnings) == 0 {
		t.Errorf("bare document: err %v, warnings %v; want a provenance warning", err, warnings)
	}
}

func TestEngineInfo(t *testing.T) {
	for out, want := range map[string]schema.EngineInfo{
		"Terraform v1.16.5\non linux_amd64\n": {Name: "terraform", Version: "1.16.5"},
		"OpenTofu v1.10.0\non linux_amd64\n":  {Name: "opentofu", Version: "1.10.0"},
	} {
		eng := schema.Engine{Runner: fixedRunner(out)}
		got, err := eng.Info(t.Context(), ".")
		if err != nil || got != want {
			t.Errorf("Info(%q) = %+v, %v; want %+v", out, got, err, want)
		}
	}
}
