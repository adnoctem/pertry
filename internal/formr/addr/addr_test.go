package addr_test

import (
	"testing"

	"github.com/adnoctem/pertry/internal/formr/addr"
)

func TestParseProvider(t *testing.T) {
	for in, want := range map[string]string{
		"hcloud":                        "registry.terraform.io/hashicorp/hcloud",
		"hetznercloud/hcloud":           "registry.terraform.io/hetznercloud/hcloud",
		"Example.com/Acme/Thing":        "example.com/acme/thing",
		"registry.terraform.io/acme/x/": "",
	} {
		got, err := addr.ParseProvider(in)
		if want == "" {
			if err == nil {
				t.Errorf("ParseProvider(%q) = %s, want an error", in, got)
			}
			continue
		}
		if err != nil || got.String() != want {
			t.Errorf("ParseProvider(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
}

func TestCoversAndWithin(t *testing.T) {
	network := addr.Path{addr.A("network"), addr.E(), addr.A("ip")}

	cases := []struct {
		ref            addr.Path
		covers, within bool
	}{
		{addr.Path{addr.A("network")}, true, false},
		{addr.Path{addr.A("network"), addr.E(), addr.A("ip")}, true, false},
		// An index into the set selects an element, like a splat.
		{addr.Path{addr.A("network"), addr.A("0"), addr.A("ip")}, true, false},
		{addr.Path{addr.A("network"), addr.E(), addr.A("mac")}, false, false},
		{addr.Path{addr.A("network"), addr.E(), addr.A("ip"), addr.E()}, false, true},
		{nil, true, false},
	}
	for _, c := range cases {
		if got := c.ref.Covers(network); got != c.covers {
			t.Errorf("%s covers %s = %v, want %v", c.ref, network, got, c.covers)
		}
		if got := c.ref.Within(network); got != c.within {
			t.Errorf("%s within %s = %v, want %v", c.ref, network, got, c.within)
		}
	}
}

// A nested field and a literal map key render alike but are different identities.
func TestPathsAreTyped(t *testing.T) {
	field := addr.Path{addr.A("timeouts"), addr.A("create")}
	mapElem := addr.Path{addr.A("timeouts"), addr.M()}
	if field.Compare(mapElem) == 0 {
		t.Error("a field and a map element compare equal")
	}
	if got := (addr.Path{addr.A("labels"), addr.M()}).String(); got != "labels{}" {
		t.Errorf("map element renders %q", got)
	}
}
