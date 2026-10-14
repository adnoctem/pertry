# Agent guide — pertry

Agent-facing development guide for this repository: three Go applications, `bookscape`, `sicht` and `formr`, in one
Bazel monorepo, MIT licensed, with no cgo, built for `linux/amd64` and `windows/amd64`. [`ARCHITECTURE.md`][architecture]
describes how they work and [`CONTRIBUTING.md`][contributing] is the canonical workflow; read both before non-trivial
work. This file records what an agent otherwise gets wrong.

## Repository tasks

| Task                            | Command                                                 |
| ------------------------------- | ------------------------------------------------------- |
| Build everything                | `bazel build //...`                                     |
| Test everything                 | `bazel test //...`                                      |
| Test under the race detector    | `bazel test //... --config=race`                        |
| Regenerate the BUILD files      | `bazel run //:gazelle`                                  |
| Tidy the module graph           | `bazel mod tidy`                                        |
| Cross-compile both platforms    | `scripts/bazel-build-platforms.sh`                      |
| Vet for the Windows build       | `GOOS=windows CGO_ENABLED=0 go vet ./...`               |
| Run the hooks                   | `pre-commit run --all-files`                            |
| Build the release artifacts     | `scripts/release-create.sh --version <version>`         |
| Verify a release builds         | `scripts/release-create.sh -v <version> --dry-run`      |
| End-to-end tests (needs Docker) | `go test -tags e2e -timeout 20m ./tests/e2e/...`        |
| Integration tests (needs tools) | `go test -tags integration ./internal/...`              |
| Scan for known vulnerabilities  | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` |

The scripts need [libsh][libsh] with `LIBSH_DIR` exported. Before committing or handing changed files back, run
`pre-commit run --all-files`, resolve the findings, and rerun after a hook modifies files. For anything touching the
build or the packaging path, finish with a release dry run. **Always run both cross-compiles**: a change can pass
on Linux and fail on `windows/amd64`. `bazel build --config=windows-amd64 //...` fails at analysis because test
targets cannot resolve a toolchain for that platform; use the script.

## Things that catch people out

- **Never issue a query while iterating rows.** Both stores use one SQLite connection, so a query inside an open
  result set waits forever for the connection the open rows hold. Collect first, or page by key and close each page
  before acting on it, as `store.EachFileForRules` does.
- **Run Gazelle after adding or moving a Go file, before `bazel test`.** A test file Gazelle has not seen is in no
  target, and `bazel test` passes without running it.
- **A file's extension is not its format.** Takeout exports contain JPEGs named `.HEIC` and animated WebP named
  `.gif`. Rules and code key on `format`, from the file's content, never `ext`.
- **EXIF group prefixes matter.** The iOS screenshot marker appears under `ExifIFD:UserComment` for some files and
  `XMP-exif:UserComment` for others. Use `tags(exif, "UserComment")` and `has_tag()`, which search every group.
- **The race detector needs cgo.** `--config=race` enables it for tests only; passing the race flag on its own fails
  at analysis.
- **A fake executable written by a parallel test must be written under `syscall.ForkLock`**, or another test's fork
  can inherit the open descriptor and running the file fails with `text file busy`.
- **markdownlint wants aligned table pipes.** Editing a table usually means realigning every row. `\|` inside a cell
  is an escaped pipe, not a column separator.
- **Ollama's plain `qwen3-vl` tags are thinking variants**, and Ollama 0.40.3 does not turn thinking off for them.
  A thinking model can exhaust its context before answering. Use `-instruct` tags; compare config digests against
  the explicitly named tags before assuming what a tag is.
- **Model agreement is not accuracy.** Several models can agree on the same wrong answer at full confidence. Only
  labels made by a person settle which is right.

## Architecture and public APIs

- `cmd/<app>` is a thin entrypoint. Application code lives in `internal/<app>`; anything more than one application
  shares lives in `pkg/`. There is no global state.
- `pkg/infer` splits inference into a `Completer` (prompt plus schema to typed JSON, the only path to free text) and
  a `Decider` (bounded questions to calibrated probabilities). Backends implement what they can and return
  `ErrUnsupported` for the rest; `ModelManager` is asserted for, not assumed.
- **`sicht` uses only the Ollama backend.** Nothing about the images leaves the machine, because an image archive
  holds third parties' personal data. The hosted backends in `pkg/infer` are for `bookscape` and are not reachable
  from `sicht`. `Health` derives `Local` from the resolved host. Opt-in hosted inference for `sicht` is listed in
  [`TODO.md`][todo] and is not to be anticipated in code.
- Each application's user documentation is `cmd/<app>/README.md`. `ARCHITECTURE.md` says how the application works
  and where its limits are; the README says how to use it. Do not duplicate one into the other.
- `internal/bookscape/export` holds two projections over the same rows: `BuildBackup` for Linkwarden's JSON backup
  format (the default) and `BuildNetscape` for Netscape HTML. They apply the same filters, collection order and
  truncation. Only the JSON one carries generated descriptions into Linkwarden.
  - **Never let a slice in `export.Backup` marshal to `null`.** Linkwarden's importer then fails inside a
    transaction it does not report, and answers `200 "Success."` having imported nothing. The same happens for a
    link with neither `importDate` nor `createdAt`. A passing HTTP status proves nothing;
    `TestBackupNeverEncodesAnArrayAsNull` and the `e2e` read-back are what catch it.
- `internal/sicht/schema` is the only place JSON Schemas are built. Observational fields precede decision fields,
  and the encoded form is assembled in order rather than marshalled from a map. The `other` escape hatch with its
  `<facet>_other` companion exists only on closed taxonomy fields (`schema.HasEscapeHatch`); on an inline enum,
  `other` is an ordinary value.
- `internal/formr/analysis` never evaluates an expression. `unresolved` is never coverage: do not add a heuristic,
  default or exception that turns it into a mapping. A variable that only gates a declaration's `count` or
  `for_each` is reported on the declaration, never as a field's origin. No code path names a provider-specific type
  or field; the Hetzner pilot is an acceptance case, and the test fixtures use an invented `acme/acme` provider.
- `formr`'s report is deterministic and holds no values from the module. Never add a timestamp, absolute path,
  line number or literal value to it, and never record whether schemas came from the engine or a snapshot: the same
  schemas must render the same bytes either way.
- Name enum values after what belongs in them. A catch-all that names an absence is rarely chosen by a model.
- `internal/sicht/store` keys everything by content hash, never by path. The `decisions` view resolves human over
  rule over model; precedence is a reading of the rows, never applied by deleting any.
- `internal/fsops` treats files as irreplaceable: rename first, then copy, verify and only then delete across
  volumes, so an interruption leaves two copies rather than none.

## Conventions

- Go, formatted by `gofmt` and linted by golangci-lint with `.golangci.yml`. Standard library by default: `log/slog`,
  `net/http`, `testing`. CLIs use Cobra.
- **No cgo.** Every Bazel build disables it. SQLite is `modernc.org/sqlite`, driver name `sqlite`.
- **Ask before adding any dependency** not already in `go.mod` or `MODULE.bazel`, Go module or Bazel module alike.
- Comments explain why. A test that exists because of a specific failure says which.
- **State measurements as measurements, and estimates as estimates.** `sicht doctor`'s memory figure is an estimate
  of twice the model's weights, not a measured requirement. Benchmarks go under `docs/benchmarks/` as dated reports.
- **Measure through the real pipeline.** A model benchmark runs `sicht run` against copies of a workspace, not a
  separate harness that bypasses the stage runner.
- Documentation describes the project as it is. When behaviour changes, update `ARCHITECTURE.md` and the affected
  README in the same change; record open questions and unverified behaviour in `TODO.md`.
- Shell follows libsh's shape: `set -euo pipefail`, the libsh preamble, `<domain>-<verb>[-<object>].sh` filenames with
  a matching `<domain>_<verb>::` namespace, and a `#####` comment block over each function giving its Arguments,
  Outputs and Returns. shfmt's defaults decide formatting.
- Prettier is authoritative for Markdown, JSON and YAML.
- Conventional commits: `type(scope): summary`, imperative and lowercase, no trailing period. Types:
  `feat|fix|docs|refactor|test|chore|build|ci`. Scopes: `bookscape`, `sicht`, a sub-package where it clarifies
  (`sicht/doctor`, `infer`), `build`, `ci` or `docs`. Explain the problem, the resulting behaviour and how it was
  checked; semantic-release derives the version from these.
- pertry is in 0.x: a breaking change takes the minor version, and the release script refuses anything that is not a
  plain `0.x.y`.
- **Publishing is gated.** Do not push, tag or publish anything without the user's authorisation.

## Security review rules

- Never commit secrets, credentials or tokens. `secrets/` is gitignored and holds the owner's real exports; fixtures
  under `testdata/` are synthetic and stay that way.
- **Never look at the owner's images to label or inspect them.** Doing so sends them off the machine. Ground truth
  is labelled by a person locally; published results are aggregates only, with no filenames or hashes.
- Do not log a credential and do not accept one as a process argument. `bookscape`'s provider keys come from the
  environment or the configuration file.
- **`sicht` never sends image data off the machine.** A non-loopback `OLLAMA_HOST` is warned about twice by `doctor`:
  for exposure, and because inference would no longer be local.
- **No session token is planned for `sicht serve`.** The applications run on the machine holding the data and are not
  for public deployment. The loopback bind, Host-header validation, `http.CrossOriginProtection` and the CSP are what
  defend the planned web interface against web pages in the user's own browser, so none of them is optional.
- A path built from a facet value is sanitised before it becomes a filename: `fsops.SanitiseRelative` rejects `..`,
  absolute paths and drive letters; `SanitiseSegment` handles the characters Windows refuses, trailing dots and
  spaces, and reserved device names. A facet value can come from a model.
- Never disable certificate verification. `bookscape check` talks to arbitrary bookmarked hosts.
- Verify downloaded content against an independently trusted digest before using it. Bazel's dependencies are
  digest-pinned.

## Layout

- `cmd/<app>/`: entrypoint and that application's `README.md`.
- `internal/bookscape/`: `netscape`, `store`, `audit`, `dedupe`, `liveness`, `classify`, `review`, `export`, `config`,
  `cmd`.
- `internal/sicht/`: `core`, `config`, `store`, `meta`, `scan`, `rules`, `schema`, `stage`, `confidence`, `imageprep`,
  `apply`, `manifest`, `doctor`, `bench`, `cmd`.
- `internal/formr/`: `addr`, `diag`, `module`, `schema`, `analysis`, `report`, `cmd`, and `formrtest` for shared
  fixtures.
- `internal/fsops/`: verified move and copy with a cross-volume fallback, used by `apply` and `undo`.
- `internal/harness/`: build-time checks for the SQLite driver and the cgo-free build.
- `pkg/infer/`: `ollamabackend`, `openaibackend`, `anthropicbackend` and a `Fake` for tests.
- `pkg/stamp/`: link-time build metadata reported by `<app> version`. Adding a stamp means the variable, the `x_defs`
  entry on the library and the key in `scripts/bazel-create-status.sh`; missing one leaves the value `redacted`.
- `tests/e2e/`: container-backed tests behind the `e2e` tag, excluded from Bazel through `# gazelle:exclude tests`.
- `scripts/`: `release-create.sh` (release artifacts), `bazel-build-platforms.sh` (cross-compiles), both libsh
  consumers; `bazel-create-status.sh` (build stamps), which needs only Bash, Git and coreutils.
- `docs/`: this file, `ARCHITECTURE.md`, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `TODO.md` and
  `benchmarks/`.
- `dist/` (release output) and `secrets/` (real exports) are gitignored.
- This file lives at `docs/AGENTS.md`; the root `AGENTS.md` is a symlink. Edit this file.

<!-- File references -->

[architecture]: ARCHITECTURE.md
[contributing]: CONTRIBUTING.md
[todo]: TODO.md

<!-- General links -->

[libsh]: https://github.com/adnoctem/libsh
