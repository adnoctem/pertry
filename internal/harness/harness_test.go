package harness_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/adnoctem/pertry/internal/harness"
)

func TestSQLiteVersion(t *testing.T) {
	t.Parallel()

	version, err := harness.SQLiteVersion(context.Background())
	if err != nil {
		t.Fatalf("SQLiteVersion() error = %v, want nil", err)
	}

	if version == "" {
		t.Fatal("SQLiteVersion() returned an empty version string")
	}

	t.Logf("pure-Go SQLite %s on %s/%s", version, runtime.GOOS, runtime.GOARCH)
}
