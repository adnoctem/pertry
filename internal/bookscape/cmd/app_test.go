package cmd_test

import (
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/bookscape/cmd"
)

// TestUnknownCommandIsReported guards a mistake with no other symptom: returning nil for an
// unrecognised argument made `bookscape nonsense` print nothing and exit 0, so a typo in a script
// was indistinguishable from a clean run.
func TestUnknownCommandIsReported(t *testing.T) {
	t.Parallel()

	_, _, err := run(t, "nonsense")
	if err == nil {
		t.Fatal("error = nil for an unknown subcommand, want a usage error")
	}

	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want it to name the problem", err)
	}

	if got := cmd.ExitCodeFor(err); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want %d (usage)", got, cmd.ExitUsage)
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	t.Parallel()

	_, _, err := run(t, "--nonsense")
	if err == nil {
		t.Fatal("error = nil for an unknown flag, want a usage error")
	}

	if got := cmd.ExitCodeFor(err); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want %d (usage)", got, cmd.ExitUsage)
	}
}
