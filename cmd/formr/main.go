// Command formr reports how completely a Terraform module exposes the providers it uses, and
// fails CI when that report drifts or falls short of a coverage policy.
//
// This is a thin entrypoint; everything lives in internal/formr.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/adnoctem/pertry/internal/formr/cmd"

	// Imported for its link-time variables, which Bazel's x_defs populate.
	_ "github.com/adnoctem/pertry/pkg/stamp"
)

func main() {
	// os.Exit is called here and nowhere else, so that run() can use defer.
	os.Exit(run())
}

func run() int {
	// An engine invocation can download providers; Ctrl-C has to cancel it rather than leave it
	// running after formr has gone.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cmd.Execute(ctx, &cmd.App{Out: os.Stdout, Err: os.Stderr}, os.Args[1:])
}
