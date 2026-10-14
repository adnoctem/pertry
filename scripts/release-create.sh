#!/usr/bin/env bash
#
# Create pertry's release artifacts: every application built for each release platform, a tarball
# per application and platform, and a SHA-256 checksum file covering every tarball.
#
# semantic-release runs it before tagging; see .releaserc. Requires libsh (LIBSH_DIR) and Bazel.

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

# Every application is released for the two platforms the cgo ban exists to keep buildable:
# linux/amd64 covers WSL2 and native Linux, and windows/amd64 is a hard requirement rather than a
# nicety (docs/CONTRIBUTING.md). Keep this in step with the --config entries in
# .bazelrc and the cross-compile matrix in .github/workflows/testing.yaml.
readonly DEFAULT_TARGETS="linux/amd64,windows/amd64"
readonly BINARIES_QUERY='kind("go_binary", //cmd/...)'
readonly CHECKSUMS_FILE="CHECKSUMS_SHA256.txt"

# -------------------------
#   Flag spec
# -------------------------

# shellcheck disable=SC2034 # read by the dynamically loaded option parser
OPTS=(
	"-v,--version:version:1:optional"
	"-o,--output-dir:output_dir:1:optional"
	"-t,--targets:targets:1:optional"
	"--check-prerequisites:check_prerequisites:0:optional"
	"--dry-run:dry_run:0:optional"
	"-h,--help:help:0:optional"
)

# shellcheck disable=SC2034 # read by the dynamically loaded option parser
declare -A OPTS_HELP=(
	[version]="The release version stamped into the binaries (default: \$PERTRY_VERSION, else git describe)"
	[output_dir]="Directory to create the artifacts in; replaced if it exists (default: <repository>/dist)"
	[targets]="Comma-separated <os>/<arch> pairs to build for (default: ${DEFAULT_TARGETS})"
	[check_prerequisites]="Check that the required commands are installed, then exit"
	[dry_run]="Build every target and verify the version, but write no artifacts"
	[help]="Show this help message and exit"
)

declare -A OPTS_VALUES=()

#######################################
# Check that the commands the release needs are installed.
# Arguments:
#   None
# Outputs:
#   One line per prerequisite.
# Returns:
#   0 if all were found, 1 otherwise.
#######################################
function release_create::prerequisites() {
	local prerequisites=('bazel' 'git' 'tar' 'sha256sum')

	for prerequisite in "${prerequisites[@]}"; do
		if ! lib::os::is_executable "${prerequisite}"; then
			lib::log::print_error "Could not find '${prerequisite}' in PATH. Please install it to proceed!"
			return 1
		fi

		lib::log::print_info "Found '${prerequisite}' in PATH."
	done

	lib::log::print_success "Found all prerequisites: '${prerequisites[*]}'."
}

#######################################
# Check the version against pertry's release policy.
#
# pertry is in 0.x: a breaking change takes the minor, so there is no 1.0.0 until both command
# surfaces and the stored formats have been reviewed as stable. The guard is here as well as in
# .releaserc because this script is also what a human runs by hand, and the hand-run path is the
# one without a commit analyser to say no.
# Arguments:
#   1 - the version to check
# Outputs:
#   An explanation on rejection.
# Returns:
#   0 if the version may be released, 1 otherwise.
#######################################
function release_create::check_version() {
	local version=$1

	if [[ ! ${version} =~ ^0\.[0-9]+\.[0-9]+$ ]]; then
		lib::log::print_error "Refusing to release '${version}': pertry is in 0.x and this script accepts only a plain 0.x.y."
		lib::log::print_info "A breaking change takes the minor version. Review the policy in docs/CONTRIBUTING.md before graduating to 1.0.0."
		return 1
	fi

	if [[ ${version} == 0.0.* ]]; then
		lib::log::print_error "Refusing to release '${version}': the first release is 0.1.0."
		return 1
	fi

	lib::log::print_info "Version '${version}' satisfies the 0.x policy."
}

#######################################
# Print the Bazel label of every releasable binary, one per line.
# Arguments:
#   None
# Outputs:
#   One label per line.
# Returns:
#   0 if at least one was found, 1 otherwise.
#######################################
function release_create::binaries() {
	local labels
	if ! labels=$(bazel query "${BINARIES_QUERY}" 2>/dev/null) || [[ -z ${labels} ]]; then
		lib::log::print_error "No go_binary targets under //cmd - nothing to release."
		return 1
	fi

	printf '%s\n' "${labels}"
}

#######################################
# Build every application for one target and copy the binaries into the staging directory.
#
# The binaries are built with --stamp so that the version reaches their own version output through
# the workspace status script; without it the stamp is Bazel's "redacted" placeholder.
# Arguments:
#   1 - the target as <os>/<arch>
#   2 - the staging directory
#   3 - the version to stamp
#   @ - the labels to build
# Outputs:
#   One line per binary copied.
# Returns:
#   0 on success, non-zero if Bazel failed.
#######################################
function release_create::build_target() {
	local target=$1 staging=$2 version=$3
	shift 3
	local labels=("$@")

	local goos="${target%%/*}"
	local goarch="${target##*/}"
	local platform="@io_bazel_rules_go//go/toolchain:${goos}_${goarch}"

	lib::log::print_info "Building ${#labels[@]} binary/binaries for ${goos}/${goarch}."

	PERTRY_VERSION="${version}" bazel build --stamp --platforms="${platform}" "${labels[@]}"

	# Staged under the bare command name, not a platform-suffixed one. The name in the archive
	# is the name the command is installed as, so `tar -xzf - sicht` yields something runnable
	# rather than something to rename first. The platform lives in the tarball's name, where
	# somebody downloading it can see it, and each target gets its own staging directory so the
	# bare names cannot collide between them.
	mkdir -p "${staging}"

	local label name src dest
	for label in "${labels[@]}"; do
		name="${label##*:}"
		src=$(PERTRY_VERSION="${version}" bazel cquery --output=files --platforms="${platform}" "${label}" 2>/dev/null)

		dest="${staging}/${name}"
		[[ ${goos} == "windows" ]] && dest="${dest}.exe"

		install -m 0755 "${src}" "${dest}"
		lib::log::print_info "  ${goos}/${goarch}: ${dest##*/}"
	done
}

#######################################
# Package the staged binaries of one target: one tarball per application and one carrying all,
# each with the LICENSE and the README alongside the executable.
# Arguments:
#   1 - the target as <os>/<arch>
#   2 - the staging directory for that target
#   3 - the output directory
#   @ - the application names staged there
# Outputs:
#   One line per tarball.
# Returns:
#   0 on success, 1 if nothing was staged.
#######################################
function release_create::package() {
	local target=$1 staging=$2 output=$3
	shift 3
	local names=("$@")

	if [[ ${#names[@]} -eq 0 ]]; then
		lib::log::print_error "Nothing staged for ${target} - nothing to package."
		return 1
	fi

	local goos="${target%%/*}"
	local goarch="${target##*/}"

	# Owned by root in the archive rather than by whoever ran the build. The README installs
	# with `sudo tar -C /usr/local/bin`, and tar running as root restores the recorded owner -
	# so without this, a binary in /usr/local/bin would belong to the CI runner's uid.
	local -a ownership=(--owner=0 --group=0 --numeric-owner)

	local name member
	local members=()

	# One tarball per application, for somebody who wants only one of the two.
	for name in "${names[@]}"; do
		member="${name}"
		[[ ${goos} == "windows" ]] && member="${name}.exe"
		members+=("${member}")

		tar -czf "${output}/${name}_${goos}_${goarch}.tar.gz" "${ownership[@]}" \
			-C "${staging}" "${member}" \
			-C "${ROOT_DIR}" LICENSE README.md
		lib::log::print_info "  ${name}_${goos}_${goarch}.tar.gz"
	done

	# And one carrying all of them, since they share a monorepo and a version.
	tar -czf "${output}/pertry_${goos}_${goarch}.tar.gz" "${ownership[@]}" \
		-C "${staging}" "${members[@]}" \
		-C "${ROOT_DIR}" LICENSE README.md
	lib::log::print_info "  pertry_${goos}_${goarch}.tar.gz"

	lib::log::print_success "Packaged ${goos}/${goarch}: ${#names[@]} application(s) plus the combined tarball."
}

#######################################
# Write one checksum file covering every tarball in the output directory.
#
# Relative paths on purpose: a checksum file naming the build machine's directories is useless to
# whoever downloads it, and `sha256sum -c` expects the names to resolve beside the file.
# Arguments:
#   1 - the output directory
# Outputs:
#   The checksum file's name.
# Returns:
#   0 on success, 1 when there was nothing to checksum.
#######################################
function release_create::checksums() {
	local output=$1

	shopt -s nullglob
	local tarballs=("${output}"/*.tar.gz)
	shopt -u nullglob

	if [[ ${#tarballs[@]} -eq 0 ]]; then
		lib::log::print_error "No tarballs in ${output} - nothing to checksum."
		return 1
	fi

	(cd "${output}" && sha256sum -- *.tar.gz >"${CHECKSUMS_FILE}")
	lib::log::print_success "Wrote ${CHECKSUMS_FILE} over ${#tarballs[@]} tarball(s)."
}

#######################################
# Main
#######################################
function release_create::main() {
	lib::opt::parse "$@"

	if [[ -n ${OPTS_VALUES[help]:-} ]]; then
		lib::opt::usage
		return 0
	fi

	if [[ -n ${OPTS_VALUES[check_prerequisites]:-} ]]; then
		release_create::prerequisites
		return $?
	fi

	release_create::prerequisites

	# The 0.x policy gates a *release*, and only a named version is one. A run with no version
	# derives one from git and skips the check deliberately: that is how CI smoke-builds this
	# path on an ordinary commit, where `git describe` reads "fa27264-dirty" and no policy
	# about minor versions applies to it.
	local version="${OPTS_VALUES[version]:-${PERTRY_VERSION:-}}"
	if [[ -n ${version} ]]; then
		version=${version#v}
		release_create::check_version "${version}" || return 1
	else
		version=$(git -C "${ROOT_DIR}" describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
		version=${version#v}
		lib::log::print_warn "No version given; deriving '${version}' from git. A release must pass --version."
	fi

	local output="${OPTS_VALUES[output_dir]:-${ROOT_DIR}/dist}"
	local targets_csv="${OPTS_VALUES[targets]:-${DEFAULT_TARGETS}}"

	local targets=()
	IFS=',' read -r -a targets <<<"${targets_csv}"

	local labels=()
	while IFS= read -r line; do
		[[ -n ${line} ]] && labels+=("${line}")
	done < <(release_create::binaries)

	local dry_run=""
	[[ -n ${OPTS_VALUES[dry_run]:-} ]] && dry_run="yes"

	# A dry run still builds everything, because the point of it is to find out whether the
	# release *can* be built. Only the writing is skipped, into a staging directory that is
	# removed afterwards.
	local staging_root
	if [[ -n ${dry_run} ]]; then
		staging_root=$(mktemp -d)
		lib::log::print_info "Dry run: building every target, writing no artifacts."
	else
		rm -rf "${output}"
		mkdir -p "${output}"
		staging_root="${output}/tmp"
	fi

	local names=()
	local label
	for label in "${labels[@]}"; do
		names+=("${label##*:}")
	done

	local target staging
	for target in "${targets[@]}"; do
		# One staging directory per target, because the binaries are staged under their bare
		# command names and two targets would otherwise overwrite each other.
		staging="${staging_root}/${target//\//_}"

		release_create::build_target "${target}" "${staging}" "${version}" "${labels[@]}"

		[[ -n ${dry_run} ]] && continue

		release_create::package "${target}" "${staging}" "${output}" "${names[@]}"
	done

	if [[ -n ${dry_run} ]]; then
		rm -rf "${staging_root}"
		lib::log::print_success "Dry run complete: ${#labels[@]} binary/binaries build for ${#targets[@]} target(s) at version ${version}."
		return 0
	fi

	release_create::checksums "${output}"

	# The whole staging tree, not the last target's: it sits inside the output directory and
	# would otherwise be uploaded along with the artifacts.
	rm -rf "${staging_root}"

	lib::log::print_success "Release ${version} prepared in ${output}."
}

release_create::main "$@"
