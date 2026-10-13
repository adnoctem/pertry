// Package harness holds the repository's build-harness invariants.
//
// It contains no application logic. Its only job is to give `bazel build //...` and
// `bazel test //...` a real Go target that exercises the two constraints the whole repo
// depends on, so that a regression fails the build instead of surfacing later in an
// application:
//
//   - SQLite is reachable through database/sql with the pure-Go driver registered under the
//     name "sqlite" (not "sqlite3"), per docs/ARCHITECTURE.md#storage.
//   - That driver compiles for every release platform, including windows/amd64, without a C
//     toolchain. Building this package under the windows_amd64 platform is what proves cgo has
//     not crept back in.
//
// The cgo ban itself is enforced by the build configuration, not by a test: .bazelrc sets
// rules_go's `pure` flag for every build, and CI additionally cross-compiles with
// CGO_ENABLED=0. A test cannot assert it, because Go's `cgo` build tag reports whether a C
// toolchain is available on the builder, not whether this module uses one.
//
// Both applications open their own databases through their own store packages; nothing here
// is meant to be imported by them.
package harness

import (
	"context"
	"database/sql"
	"fmt"

	// Pure-Go SQLite. Registers the "sqlite" driver. No cgo.
	_ "modernc.org/sqlite"
)

// DriverName is the driver registered by modernc.org/sqlite. It is deliberately spelled out
// here because the cgo binding most SQLite documentation assumes registers "sqlite3", and
// passing that name to sql.Open fails only at runtime.
const DriverName = "sqlite"

// SQLiteVersion opens an in-memory database with the pure-Go driver and returns the SQLite
// library version it reports. A non-empty result means the driver is both linked and able to
// execute a statement on this platform.
func SQLiteVersion(ctx context.Context) (string, error) {
	db, err := sql.Open(DriverName, ":memory:")
	if err != nil {
		return "", fmt.Errorf("open in-memory %s database: %w", DriverName, err)
	}
	// Discarding the close error is deliberate: this database is in-memory and read-only, so
	// there is nothing to flush and nothing a caller could do about a failure here.
	defer func() { _ = db.Close() }()

	var version string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return "", fmt.Errorf("query sqlite_version(): %w", err)
	}

	return version, nil
}
