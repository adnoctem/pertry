# Ad Noctem Collective - `pertry` Repository Contributing Guidelines

Contributions are welcome via GitHub's Pull Requests. This document covers the development workflow, the code
conventions and the release procedure.

Start with the [Architecture][architecture] for how the applications work and what they share. Each
application's README beside its entrypoint is its user documentation. Participation follows the
[Code of Conduct][conduct]; report suspected vulnerabilities privately using the [Security Policy][security]. Open
work is tracked in the [TODO][todo].

## 🧰 Prerequisites

Develop on Linux or WSL2. You need:

- Git and [bazelisk][bazelisk], which reads `.bazelversion` and fetches the matching Bazel;
- [`pre-commit`][precommit], and [Bun][bun] for Prettier;
- [libsh][libsh] 0.7.0 or newer for the release and cross-compilation scripts;
- a Docker-compatible engine and a Go toolchain for the end-to-end tests, which run outside Bazel.

Working on `sicht` also needs [exiftool][exiftool] 12.0 or newer on `PATH` and a running [Ollama][ollama] 0.12.11 or
newer, installed from the official release rather than a CPU-only distribution package. `sicht doctor` checks both
and says how to fix what is missing.

The Go SDK used by the build, every Go module and every Bazel rule set are pinned in `MODULE.bazel` and `go.mod`
and fetched by Bazel.

```sh
pre-commit install
bazel build //...
bazel test //...
```

The scripts take their option parsing, logging and prerequisite checks from libsh and find it through `LIBSH_DIR`.
They report a missing or incompatible installation themselves.

```sh
curl -fsSL https://raw.githubusercontent.com/adnoctem/libsh/v0.7.0/bin/install |
  bash -s -- --version v0.7.0
export PATH="${HOME}/.local/bin:${PATH}"
export LIBSH_DIR="$(libman path)"
```

Install a pinned release rather than `main`: libsh is pre-1.0 and its minor releases may break compatibility. CI
installs the same pinned version.

## 🗂️ Repository Layout

The [Architecture][architecture_layout] shows the full tree. In short: `cmd/<app>/` holds a thin entrypoint and the
application's README, `internal/<app>/` its code, `pkg/` what more than one application shares, `tests/e2e/` the
container-backed tests, and `scripts/` the release, stamp and cross-compilation scripts. `dist/` (release output)
and `secrets/` (local real data) are gitignored.

## ⚒️ Building and Testing

| Task                         | Command                                                 |
| ---------------------------- | ------------------------------------------------------- |
| Build everything             | `bazel build //...`                                     |
| Test everything              | `bazel test //...`                                      |
| Test under the race detector | `bazel test //... --config=race`                        |
| Test one package             | `bazel test //internal/sicht/store:store_test`          |
| Regenerate the BUILD files   | `bazel run //:gazelle`                                  |
| Tidy the module graph        | `bazel mod tidy`                                        |
| Cross-compile both platforms | `scripts/bazel-build-platforms.sh`                      |
| Cross-compile one platform   | `scripts/bazel-build-platforms.sh -c windows-amd64`     |
| Vet for the Windows build    | `GOOS=windows CGO_ENABLED=0 go vet ./...`               |
| Known vulnerabilities        | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` |
| Run the hooks                | `pre-commit run --all-files`                            |

Run Gazelle after adding, moving or renaming a Go file. A test file Gazelle has not seen belongs to no target, and
`bazel test` then passes without running it.

**Always run both cross-compiles.** `windows/amd64` is a supported platform that nobody develops on, so a change can
pass on Linux and fail there. `internal/fsops` and `internal/sicht/doctor` carry per-platform files, and
`GOOS=windows go vet` checks the ones a Linux build never compiles. `bazel build --config=windows-amd64 //...` does
not work for this: test targets cannot resolve a test toolchain for a platform the host cannot execute, so the script
builds every non-test target instead.

The race detector needs cgo, which every other build disables; `--config=race` enables it for that test run only.

Two test tiers run outside the default suite:

```sh
# Container-backed end-to-end tests against a real Linkwarden. Needs a Docker socket and a Go toolchain.
go test -tags e2e -timeout 20m ./tests/e2e/...

# Tests needing a local binary - exiftool, or a running Ollama - or a real archive passed by path.
SICHT_TEST_ARCHIVE=/path/to/export go test -tags integration ./internal/sicht/...
```

Both tags are linted, both skip themselves with instructions when what they need is absent, and neither runs under
Bazel: `tests/` is excluded from the build graph. CI runs the end-to-end tests nightly, on demand, and on a pull
request labelled `e2e`. See [`tests/README.md`][tests_readme].

## 📝 Code Conventions

- Go is formatted by `gofmt` and linted by golangci-lint with `.golangci.yml`.
- Use the standard library by default: `log/slog` for logging, `net/http` for HTTP, `testing` for tests. CLIs use
  Cobra.
- Return errors wrapped with `%w`; do not panic. A message shown to a user says what failed and, where possible, a
  hint for what to do.
- Keep `cmd/<app>` a thin entrypoint. Application code belongs in `internal/<app>`; a package moves to `pkg/` only
  when more than one application needs it. Hold no global state.
- **No cgo.** Every Bazel build disables it, so a dependency that needs it fails to build. SQLite is
  `modernc.org/sqlite` under the driver name `sqlite`, not `sqlite3`. `internal/harness` fails the build if either
  the driver name or the cgo-free build regresses.
- **Never issue a query while iterating rows.** Both stores use one connection, so a query inside an open result
  set deadlocks. Collect results first, or page through a large table and close each page before acting on it, as
  `store.EachFileForRules` does. Wrap bulk inserts in a transaction.
- Write rules and code against what a file is, not what it is called. In `sicht`, compare a file's `format`, never
  its extension, and look EXIF tags up across groups with `tags()` or `has_tag()` rather than naming one group.
- JSON Schemas for `sicht` stages are built only in `internal/sicht/schema`, assembled in a fixed order rather than
  marshalled from a map, because a model generates fields in schema order.
- A path built from a facet value goes through `fsops.SanitiseRelative` and `fsops.SanitiseSegment` before it becomes
  a filename. A facet value can come from a model.
- `export.Backup` must never marshal a slice as `null`; Linkwarden's importer fails silently on it.
  `TestBackupNeverEncodesAnArrayAsNull` and the end-to-end read-back cover it.
- A test that writes a fake executable and runs it writes it while holding `syscall.ForkLock`. Otherwise a parallel
  test's fork can inherit the open descriptor, and running the file fails with `text file busy`.
- Comments explain why: the constraint, hazard or behaviour the code depends on. A test that exists because of a
  specific failure says which.
- Shell scripts follow libsh's shape: `set -euo pipefail`, the libsh preamble, `<domain>-<verb>[-<object>].sh` file
  names with a matching `<domain>_<verb>::` function namespace, and a comment block over each function giving its
  arguments, outputs and return values. shfmt's defaults decide formatting. `scripts/bazel-create-status.sh` uses
  only Bash, Git and coreutils, because Bazel runs it on every build and reads its output as the build stamp.

## 🔐 Secrets and Real Data

Neither application has a service secret. What this repository keeps out of version control is personal data: a
bookmark export lists everything someone saved, and an image archive holds other people's faces and documents.

- `secrets/` is gitignored and holds local real exports. Never commit their contents, quote them in a commit
  message or paste them into an issue.
- Fixtures under `testdata/` are synthetic. A test that needs a real archive takes its path from an environment
  variable such as `SICHT_TEST_ARCHIVE` and skips without it.
- `*.secret`, `.env` and `.env.*` are gitignored. `bookscape`'s provider credentials come from the environment or
  the configuration file, never a flag, so they do not appear in a process listing or shell history.
- Ground-truth labels for a `sicht` benchmark are made by a person looking at the images locally. Do not send an
  archive's images to a hosted model to label them, and publish benchmark results as aggregates only — no
  filenames, content hashes or per-file rows.
- gitleaks runs as a hook. It catches the shape of a credential, not personal data.

## 🧹 Formatting and Hooks

Shell is checked by shellcheck and formatted by shfmt. Prettier is authoritative for Markdown, JSON and YAML; it
renumbers ordered lists, rewrites emphasis and reformats JSON. Prettier runs through `bun x` and uses Bun's cache
rather than a repository `node_modules`. markdownlint requires aligned table pipes, so editing a table by hand
usually means realigning every row; an escaped `\|` inside a cell is content, not a column separator.

```sh
pre-commit run --all-files
```

The hooks cover formatting, spelling, Markdown, secret scanning, workflow configuration, shell and the Go module.
They do not run the test suite. CI runs the same hooks plus the Bazel build, the tests under the race detector, `go
vet`, govulncheck, both cross-compiles and a dry run of the release build; a separate Super-Linter workflow runs the
broader shared checks.

zizmor's `ref-version-mismatch` audit needs the GitHub API. Without a token zizmor runs only its offline audits, so
export one for the hook to match CI:

```sh
export GH_TOKEN="$(gh auth token)"
```

Pin every GitHub action to the exact commit of an exact release, with the full version in the comment — `# v7.0.1`,
not `# v7`.

## 📚 Documentation

| Document                          | Audience          | Contents                                                      |
| --------------------------------- | ----------------- | ------------------------------------------------------------- |
| `README.md`                       | Users             | Overview and quick installation                               |
| `cmd/<app>/README.md`             | Users             | Installing, configuring and running one application           |
| [`ARCHITECTURE.md`][architecture] | Users, developers | How the applications work, what they share, known limitations |
| `CONTRIBUTING.md`                 | Developers        | Workflow, conventions and the release procedure               |
| [`SECURITY.md`][security]         | Everyone          | Vulnerability reporting and trust boundaries                  |
| [`TODO.md`][todo]                 | Developers        | Open work and verification items                              |
| [`benchmarks/`][benchmarks]       | Developers        | Dated measurements and their method                           |
| `AGENTS.md`                       | Coding agents     | The same ground, condensed; the root `AGENTS.md` links to it  |

Documentation describes the project as it is. Update `ARCHITECTURE.md` and the affected application README in the
same change as the behaviour. Record open questions and unverified behaviour in `TODO.md`, and a limitation users
need to know about under the application's known limitations in `ARCHITECTURE.md`. Do not duplicate one document
into another; link to it.

State measurements as measurements, with what they were taken on, and an estimate as an estimate. A benchmark goes
under `benchmarks/` as a dated report that records its setup, its method and what it does not show.

## 📦 Dependencies

**Ask before adding a dependency**, Go module or Bazel module, that is not already in `go.mod` or `MODULE.bazel`. A
candidate must:

- build with cgo disabled;
- carry a licence compatible with MIT.

`sicht` is the stricter consumer: it sends nothing off the machine, so a dependency that phones home, or one that
only works against a hosted service, does not belong in it.

Renovate proposes updates. rules_go and Gazelle are grouped, and the `modernc.org` SQLite modules are grouped,
because each set is verified together. The Go SDK is pinned in both `MODULE.bazel` and `go.mod`, and a custom
Renovate manager keeps the two in step. A Linkwarden image update is labelled `e2e`, because
`tests/e2e/linkwarden` encodes that version's import behaviour; run the end-to-end tests before merging it. Review
the release notes of every major update before merging it.

## 🚀 Releases

semantic-release generates `CHANGELOG.md` from the commits; do not edit it by hand. It decides the version and
passes it to `scripts/release-create.sh`, which checks it, builds every application for `linux/amd64` and
`windows/amd64`, packages the tarballs and writes the checksums. The [Architecture][architecture_artifacts]
describes the artifacts.

The version reaches each binary's `version` output through `PERTRY_VERSION`, which the release script sets. Without
it the build stamp comes from `git describe`. Run without `--version`, the script derives a version from Git and
skips the 0.x check, which is how CI dry-runs the release path on an ordinary commit.

pertry is in 0.x. Features and breaking changes take the minor version, fixes take the patch, and documentation,
refactoring and test commits release nothing. The release script accepts only a plain `0.x.y`; `.releaserc`
applies the same rule. Review both before releasing 1.0.0.

**Releasing is manual and gated.** The Release workflow does nothing unless `PERTRY_RELEASE_ENABLED` is `true` in
the repository's Actions variables and the workflow is dispatched. Testing dispatches it after a successful run on
`main` or `next`, passing the commit it tested, and Release refuses to continue if the branch has advanced since.

Release configuration:

- `ANC_GITHUB_TOKEN`, an optional Actions secret, is used when branch rules require a dedicated release identity;
  that identity must be allowed to push the changelog commit and the release tag. Without it the run's own
  `GITHUB_TOKEN` is used.
- `PERTRY_RELEASE_ENABLED` is the gate.

The workflow's `bootstrap` input publishes exactly 0.1.0 on a repository with no version tags. It refuses to run
once a `v*` tag exists and is not a retry mechanism.

```sh
scripts/release-create.sh --check-prerequisites       # what the script needs on the machine
scripts/release-create.sh --version 0.2.0 --dry-run   # builds everything, writes nothing
scripts/release-create.sh --version 0.2.0             # the artifacts, under dist/
```

### Completing a Partial Release

Publication is not transactional: a tag and its release assets cannot land or fail together. The workflow keeps
`dist/` as an artifact for 14 days, including on failure, so a partial release is completed rather than rebuilt; a
rebuild produces different bytes under a version that may already have been downloaded.

Inspect what exists and complete only what is missing by attaching the retained assets with `gh release upload`.
Never overwrite a published version.

## ℹ️ Commit Message Format

`type(scope): summary`, imperative and lowercase, with no trailing period. Types:
`feat|fix|docs|refactor|test|chore|build|ci`. Scopes: an application (`bookscape`, `sicht`), a sub-package where it
clarifies (`sicht/doctor`, `infer`), `build`, `ci` or `docs`. The body explains the problem, the resulting behaviour
and how it was checked. Mark an intentional compatibility change with a `BREAKING CHANGE` footer; in 0.x it takes
the minor version.

semantic-release derives the version from these messages, so a wrong type produces a wrong release.

## ✅ How to Contribute

1. Fork this repository, develop, and test your changes.
2. Add your GitHub username to the [`AUTHORS`][authors] and [`CODEOWNERS`][owners] files.
3. Submit a pull request. Keep changes to different applications in separate pull requests where they are
   independent.

A pull request must pass CI: the hooks, the Bazel build and tests, `go vet`, govulncheck, both cross-compiles and
the release dry run.

<!-- File references -->

[architecture]: ARCHITECTURE.md
[architecture_layout]: ARCHITECTURE.md#repository-layout
[architecture_artifacts]: ARCHITECTURE.md#release-artifacts
[conduct]: CODE_OF_CONDUCT.md
[security]: SECURITY.md
[todo]: TODO.md
[benchmarks]: benchmarks/README.md
[tests_readme]: ../tests/README.md
[authors]: ../.github/AUTHORS
[owners]: ../.github/CODEOWNERS

<!-- General links -->

[bazelisk]: https://github.com/bazelbuild/bazelisk
[precommit]: https://pre-commit.com
[bun]: https://bun.sh
[libsh]: https://github.com/adnoctem/libsh
[exiftool]: https://exiftool.org
[ollama]: https://ollama.com
