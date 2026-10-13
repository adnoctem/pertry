package cmd

import "fmt"

// usageError marks a failure caused by how the command was invoked rather than by the work
// itself: a bad flag, a missing workspace, an unparsable config. Execute maps it to ExitUsage.
type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

// notReadyError marks a failure caused by the environment rather than by the invocation or the
// work: exiftool missing, Ollama unreachable. Execute maps it to ExitNotReady so that a wrapper
// can tell "fix your machine" apart from "fix your command" and from "it broke".
type notReadyError struct {
	err error
}

func (e *notReadyError) Error() string { return e.err.Error() }

func (e *notReadyError) Unwrap() error { return e.err }

func notReadyf(format string, args ...any) error {
	return &notReadyError{err: fmt.Errorf(format, args...)}
}

// partialError marks a run that did some of its work. Execute maps it to ExitPartial, so that a
// pass over ten thousand images that failed on four of them is distinguishable from one that
// failed outright - cmd/sicht/README.md's reason for having a fourth exit code at all.
type partialError struct {
	err error
}

func (e *partialError) Error() string { return e.err.Error() }

func (e *partialError) Unwrap() error { return e.err }
