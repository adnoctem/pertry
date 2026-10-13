package cmd

import "fmt"

// usageError marks a failure caused by how the command was invoked - a bad flag value, a
// missing argument, an unparsable configuration file - as opposed to a failure during the
// work itself. Execute maps it to ExitUsage so that a wrapper script can tell "you asked for
// something impossible" apart from "the run failed".
type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

// usagef builds a usageError.
func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}
