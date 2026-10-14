// Package schema acquires provider schemas - from a Terraform or OpenTofu executable, or from a
// snapshot - records where they came from, and turns each into a recursive inventory of fields
// with structural fingerprints.
package schema

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner executes the engine. It is an interface so that tests can substitute a fake that records
// its arguments - which is how the guarantee that `check` never plans or applies is tested.
type Runner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// ExecRunner runs a real executable.
type ExecRunner struct {
	// Path is the engine binary: `terraform`, `tofu`, or a path to either.
	Path string
}

// Run executes the engine in dir. Stderr is folded into the error, because an engine that fails
// usually says why there and nowhere else.
func (r ExecRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Path, args...)
	cmd.Dir = dir
	// TF_IN_AUTOMATION trims advice meant for a person at a prompt; TF_INPUT=0 makes any attempt
	// to ask for input fail instead of hanging a CI job.
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("%s %s: %w", r.Path, strings.Join(args, " "), err)
		}

		return nil, fmt.Errorf("%s %s: %w\n%s", r.Path, strings.Join(args, " "), err, msg)
	}

	return stdout.Bytes(), nil
}

// EngineInfo identifies the engine that produced a schema.
type EngineInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Engine is the adapter over `terraform` or `tofu`. Both expose the same three commands formr
// needs with the same flags, which is why there is one adapter and not two; that they behave
// identically is tested per engine rather than assumed.
type Engine struct {
	Runner  Runner
	Timeout time.Duration
}

func (e Engine) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}

	return e.Runner.Run(ctx, dir, args...)
}

// Info reads the engine's name and version from the first line of `version`, which both engines
// print as `<Name> v<version>`. `version -json` would give the version but not which engine.
func (e Engine) Info(ctx context.Context, dir string) (EngineInfo, error) {
	out, err := e.run(ctx, dir, "version")
	if err != nil {
		return EngineInfo{}, err
	}

	line, _, _ := strings.Cut(string(out), "\n")
	name, version, ok := strings.Cut(strings.TrimSpace(line), " v")
	if !ok {
		return EngineInfo{}, fmt.Errorf("unrecognised engine version output %q", line)
	}

	return EngineInfo{Name: strings.ToLower(name), Version: version}, nil
}

// Init runs `init` without a backend, without input and without upgrading: it installs what the
// lock file selects and nothing newer.
func (e Engine) Init(ctx context.Context, dir string) error {
	_, err := e.run(ctx, dir, "init", "-backend=false", "-input=false")

	return err
}

// ProvidersSchema returns the raw output of `providers schema -json`. An uninitialised module
// makes the engine fail with its own explanation, which is passed through.
func (e Engine) ProvidersSchema(ctx context.Context, dir string) ([]byte, error) {
	return e.run(ctx, dir, "providers", "schema", "-json")
}
