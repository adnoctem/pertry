package schema

import (
	"errors"
	"fmt"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/adnoctem/pertry/internal/formr/addr"
)

// LockFile is the name of Terraform's dependency lock file.
const LockFile = ".terraform.lock.hcl"

// ReadLock returns the provider releases a lock file selects, keyed by full source address.
//
// Provider releases come from here and never from a schema's numeric `version`, which is the
// version of the provider's state schema and says nothing about which release produced it. A
// missing lock file is not an error: the result is empty and the caller reports the gap.
func ReadLock(path string) (map[addr.Provider]string, error) {
	src, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[addr.Provider]string{}, nil
	}
	if err != nil {
		return nil, err
	}

	file, diags := hclsyntax.ParseConfig(src, LockFile, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", LockFile, diags.Error())
	}

	out := map[addr.Provider]string{}
	for _, block := range file.Body.(*hclsyntax.Body).Blocks {
		if block.Type != "provider" || len(block.Labels) != 1 {
			continue
		}
		p, err := addr.ParseProvider(block.Labels[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", LockFile, err)
		}
		attr, ok := block.Body.Attributes["version"]
		if !ok {
			continue
		}
		v, d := attr.Expr.Value(nil)
		if d.HasErrors() || v.Type() != cty.String || v.IsNull() {
			continue
		}
		out[p] = v.AsString()
	}

	return out, nil
}
