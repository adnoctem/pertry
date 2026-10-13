#!/usr/bin/env bash
#
# Emits key/value pairs consumed by Bazel's --workspace_status_command and substituted
# into the x_defs of the go_binary targets under cmd/. Keys must be stable: renaming one
# silently turns the corresponding stamp back into its default rather than failing the
# build.
#
# Every lookup degrades to "unknown" rather than failing. This script runs on every build,
# including in a fresh clone with no commits and in CI checkouts that are shallow or
# detached - a non-zero exit here aborts the whole build.
#
# It deliberately needs nothing but Bash, Git and coreutils - not libsh, unlike
# release-create.sh beside it. Two reasons: Bazel reads this script's stdout as the stamp,
# so anything else written there would be stamped into a binary; and it runs before
# anything is built, so requiring an installed library would make a plain `bazel build`
# depend on one.

set -o errexit
set -o nounset
set -o pipefail

#######################################
# Echo the output of a command, or a default when that command fails or prints nothing.
#
# `cmd || echo default` is not enough: `git rev-parse --abbrev-ref` writes "HEAD" to stdout
# *and* exits non-zero when outside a branch, which would emit two values for one key and
# corrupt the status file.
# Arguments:
#   1 - the default to use
#   @ - the command and its arguments
# Outputs:
#   One line: the command's output, or the default.
# Returns:
#   0 always.
#######################################
fallback() {
	local default="$1"
	shift

	local out
	if out=$("$@" 2>/dev/null) && [[ -n "$out" ]]; then
		printf '%s\n' "$out"
	else
		printf '%s\n' "$default"
	fi
}

ROOT=$(fallback "$PWD" git rev-parse --show-toplevel)

# STABLE_ prefixed keys are the ones Bazel treats as invalidating: a change in a STABLE_
# value invalidates stamped actions, while a change in an unprefixed one does not. The
# version is the only key here that should force a rebuild when it moves; BUILD_DATE
# deliberately must not, or every build would be a cache miss.
#
# PERTRY_VERSION is set by the release script, because at prepare time `git describe` still
# answers with the *previous* tag - a release that relied on it would stamp itself with the
# version before the one being released.
echo STABLE_VERSION "${PERTRY_VERSION:-$(fallback 0.0.0-dev git describe --tags --always --dirty)}"

echo GIT_COMMIT_SHA "$(fallback unknown git rev-parse --short=7 HEAD)"
echo GIT_BRANCH "$(fallback unknown git rev-parse --abbrev-ref HEAD)"

# The Go SDK version is declared once, in MODULE.bazel, so the stamped GO_VERSION cannot
# drift from the toolchain that actually did the compiling. Deliberately not the `go` on
# PATH, which may be a different version entirely.
echo GO_VERSION "$(fallback unknown grep -Po '^go_sdk\.download\(version = "\K[0-9]+\.[0-9]+(\.[0-9]+)?' "$ROOT/MODULE.bazel")"

echo BUILD_DATE "$(fallback unknown date --rfc-3339=seconds)"

# Human-readable build host. Linux ships /etc/os-release; `uname -sr` is the fallback for a
# host that lacks it, which includes the Windows cross-build running on a Linux machine.
echo PLATFORM "$(fallback "$(uname -sr)" grep -Po "PRETTY_NAME=[\'\"]\K[^\"\']*" /etc/os-release)"
