#!/usr/bin/env bash
#
# Cross-compile pertry for every release platform.
#
# The windows/amd64 build is a hard requirement rather than a nicety (docs/design/sicht.md
# section 16) and is the reason cgo is banned repo-wide, so it is checked on every pull request.
#
# Deliberately not `bazel build --config=<platform> //...`. Under Bazel 9 a test target resolves
# @bazel_tools//tools/test:default_test_toolchain_type against the *target* platform, and
# windows/amd64 is not a registered execution platform here - so expanding //... fails at analysis
# with "No matching toolchains found". Every non-test target is queried instead, which cross-
# compiles the whole shipping surface and skips test binaries that were never going to run on
# Windows anyway.
#
# Requires libsh (LIBSH_DIR) and Bazel.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
ROOT_DIR=$(cd -- "${SCRIPT_DIR}/.." && pwd)

# libsh is installed on the machine, not vendored: see docs/CONTRIBUTING.md
if [[ -z ${LIBSH_DIR:-} || ! -r ${LIBSH_DIR}/lib.sh ]]; then
	printf 'Cannot read libsh at %s/lib.sh; install libsh and export LIBSH_DIR (see docs/CONTRIBUTING.md).\n' "${LIBSH_DIR:-<unset>}" >&2
	exit 1
fi
LIBSH_DIR=$(cd -P -- "${LIBSH_DIR}" && pwd)
# shellcheck source=/dev/null
. "${LIBSH_DIR}/lib.sh"
if ! declare -F lib::opt::parse >/dev/null || ! declare -F lib::log::print_success >/dev/null; then
	printf 'Incompatible libsh at %s: this script needs the option parser and intent logging of libsh 0.7.0 or newer.\n' "${LIBSH_DIR}" >&2
	exit 1
fi

# The .bazelrc config names, not <os>/<arch> pairs: this script drives Bazel configurations while
# scripts/release-create.sh drives --platforms directly. Keep both in step with .bazelrc.
readonly DEFAULT_CONFIGS="linux-amd64,windows-amd64"

# Everything that is not a test target. go_test is excluded by naming what is included rather than
# by subtracting, so a new rule kind is left out until somebody decides it belongs.
readonly TARGETS_QUERY='kind("go_binary|go_library", //...)'

# -------------------------
#   Flag spec
# -------------------------

# shellcheck disable=SC2034 # read by the dynamically loaded option parser
OPTS=(
	"-c,--configs:configs:1:optional"
	"-h,--help:help:0:optional"
)

# shellcheck disable=SC2034 # read by the dynamically loaded option parser
declare -A OPTS_HELP=(
	[configs]="Comma-separated .bazelrc config names to build (default: ${DEFAULT_CONFIGS})"
	[help]="Show this help message and exit"
)

declare -A OPTS_VALUES=()

#######################################
# Print the Bazel label of every non-test target, one per line.
# Arguments:
#   None
# Outputs:
#   One label per line.
# Returns:
#   0 if at least one was found, 1 otherwise.
#######################################
function bazel_build_platforms::targets() {
	local labels
	if ! labels=$(bazel query "${TARGETS_QUERY}" --output=label 2>/dev/null) || [[ -z ${labels} ]]; then
		lib::log::print_error "Bazel found no non-test targets - nothing to cross-compile."
		return 1
	fi

	printf '%s\n' "${labels}"
}

#######################################
# Main
#######################################
function bazel_build_platforms::main() {
	lib::opt::parse "$@"

	if [[ -n ${OPTS_VALUES[help]:-} ]]; then
		lib::opt::usage
		return 0
	fi

	if ! lib::os::is_executable bazel; then
		lib::log::print_error "Could not find 'bazel' in PATH. Please install bazelisk to proceed!"
		return 1
	fi

	cd -- "${ROOT_DIR}"

	local configs_csv="${OPTS_VALUES[configs]:-${DEFAULT_CONFIGS}}"
	local configs=()
	IFS=',' read -r -a configs <<<"${configs_csv}"

	local labels=()
	while IFS= read -r line; do
		[[ -n ${line} ]] && labels+=("${line}")
	done < <(bazel_build_platforms::targets)

	local config
	for config in "${configs[@]}"; do
		lib::log::print_info "Cross-compiling ${#labels[@]} target(s) for '${config}'."
		bazel build --config="${config}" "${labels[@]}"
	done

	lib::log::print_success "Cross-compiled ${#labels[@]} target(s) for ${#configs[@]} platform(s)."
}

bazel_build_platforms::main "$@"
