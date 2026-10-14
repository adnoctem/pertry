package schema_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/adnoctem/pertry/internal/formr/addr"
	"github.com/adnoctem/pertry/internal/formr/schema"
)

type fixedRunner string

func (r fixedRunner) Run(context.Context, string, ...string) ([]byte, error) { return []byte(r), nil }

func TestReadLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, schema.LockFile)
	lock := `
provider "registry.terraform.io/acme/acme" {
  version     = "1.2.3"
  constraints = "~> 1.2"
  hashes      = ["h1:SYNTHETIC"]
}
`
	if err := os.WriteFile(path, []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := schema.ReadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := addr.ParseProvider("acme/acme")
	if got[p] != "1.2.3" {
		t.Errorf("lock = %v", got)
	}

	missing, err := schema.ReadLock(filepath.Join(dir, "absent"))
	if err != nil || len(missing) != 0 {
		t.Errorf("missing lock file: %v, %v; want empty and no error", missing, err)
	}
}
