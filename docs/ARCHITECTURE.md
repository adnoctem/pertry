# pertry Architecture

This repository contains three local-first command-line applications written in Go, built and released together:
[`bookscape`][bookscape_readme], a bookmark archival and enrichment pipeline that emits for Linkwarden;
[`sicht`][sicht_readme], an iterative image sorter that uses metadata rules first and a local vision model for the
rest; and [`formr`][formr_readme], which reports how completely a Terraform module exposes the provider schemas it
uses. Each is a single static executable for Linux and Windows.

This document describes how the applications work, what they share, and where their limits are. Each application
README owns its installation, configuration and command reference. [Contributing][contributing] owns the
development workflow and code conventions; the [Security Policy][security] owns vulnerability reporting and trust
boundaries. Measurements that informed the defaults are published under [Benchmarks][benchmarks].

- [Repository Layout](#repository-layout)
- [Shared Architecture](#shared-architecture)
- [bookscape](#bookscape)
- [sicht](#sicht)
- [formr](#formr)
- [Build and Distribution](#build-and-distribution)
- [Privacy Notes](#privacy-notes)

## Repository Layout

```text
├── cmd/<app>/                # Thin entrypoint and the application's user-facing README
├── internal/
│   ├── bookscape/            # netscape, store, audit, dedupe, liveness, classify, review, export, config, cmd
│   ├── sicht/                # core, config, store, meta, scan, rules, schema, stage, confidence, imageprep,
│   │                         # apply, manifest, doctor, bench, cmd
│   ├── formr/                # addr, diag, module, schema, analysis, report, cmd, formrtest
│   ├── fsops/                # Verified move and copy with a cross-volume fallback
│   └── harness/              # Build-time checks for the SQLite driver and the cgo-free build
├── pkg/
│   ├── infer/                # Provider-agnostic inference: Ollama, OpenAI and Anthropic backends, a Fake
│   └── stamp/                # Link-time build metadata
├── tests/e2e/                # Container-backed end-to-end tests, behind the `e2e` build tag
├── scripts/                  # Release artifacts, build stamps and cross-compilation
└── docs/
    └── benchmarks/           # Published measurements, aggregates only
```

The Go module path is `github.com/adnoctem/pertry`. Application code lives in `internal/<app>`; a package moves to
`pkg/` only when more than one application uses it. `internal/sicht/core` imports neither the CLI nor any other frontend, so
a frontend is a thin layer over it. Neither application holds global state: the CLI builds its state once per
invocation and passes it explicitly.

## Shared Architecture

### Process Model

All three applications are short-lived command-line programs; there is no daemon and no network listener.
bookscape and sicht keep their state in a local SQLite database that each subcommand reads and writes before it
exits. formr keeps no state: each run reads a module and its provider schemas and produces a report. The pipelines are split into subcommands that
can each be re-run, resumed or re-scoped without repeating the others, and every slow stage persists results as it
completes them, so an interrupted run keeps its progress.

bookscape and sicht follow the same order of work: deterministic rules first, a model only for what rules cannot decide, and a
human review step that gates the irreversible one — `bookscape review` gates `export`, `sicht review` gates
`apply`. No application deletes user data.

Exit codes distinguish a usage error from a failed run:

| Code | bookscape     | sicht                                       | formr                                              |
| ---- | ------------- | ------------------------------------------- | -------------------------------------------------- |
| `0`  | success       | success                                     | success                                            |
| `1`  | generic error | generic error                               | report drift or coverage-policy failure            |
| `2`  | usage error   | usage error                                 | usage, engine, parse or unsupported-analysis error |
| `3`  | —             | environment not ready (`doctor` has a fail) | —                                                  |
| `4`  | —             | partial failure (some items failed)         | —                                                  |

### Storage

bookscape and sicht store state in SQLite through `modernc.org/sqlite`, a pure-Go implementation registered under
the driver name `sqlite`. Databases run in WAL mode with foreign keys enforced, and schema migrations are embedded
in the binary and applied on open.

The connection pool is limited to one connection so that SQLite never sees concurrent writers. A consequence is
that a query issued while another query's rows are still open waits for that connection indefinitely. Store code
therefore collects a result set before acting on it, and iteration over large tables reads one page at a time,
paged by key, closing each page before the caller's callback runs.

### Inference Layer

`pkg/infer` separates two capabilities, because providers differ in kind rather than only in vendor:

| Interface      | Contract                                                                                   |
| -------------- | ------------------------------------------------------------------------------------------ |
| `Completer`    | Prompt, JSON Schema and optional images in; schema-shaped JSON out. The only path to text. |
| `Decider`      | Bounded questions in; answers with calibrated probabilities out. No free text.             |
| `ModelManager` | Health, model information and model download, for backends that host their own weights.    |

A backend implements what it can and returns `ErrUnsupported` for the rest; callers assert for `ModelManager`
rather than assuming it.

| Backend            | Implements                       | Used by                 |
| ------------------ | -------------------------------- | ----------------------- |
| `ollamabackend`    | `Completer`, `ModelManager`      | `bookscape` and `sicht` |
| `openaibackend`    | `Completer`, `Decider`           | `bookscape`             |
| `anthropicbackend` | `Completer`                      | `bookscape`             |
| `Fake`             | `Completer`, `Decider`, fixtures | tests                   |

Requests carry generation options verbatim, so `temperature`, `seed` and `num_ctx` reach the provider as
configured. Responses carry token log probabilities when requested and available.

`ollamabackend` talks to Ollama's REST API directly with `net/http`; it does not use Ollama's Go SDK. It reports
whether the configured server is local by resolving `OLLAMA_HOST` rather than assuming it: an unparsable host
counts as not local, and an unreachable server reports nothing.

### Build Metadata

`pkg/stamp` holds the version, commit, branch, build date, build platform and Go toolchain, injected at link time.
Every application reports it with `<app> version [--json]`, which works without a database or a valid configuration.
An unstamped development build reports `redacted` for every field and says so.

## bookscape

### bookscape Scope

bookscape reads Netscape bookmark exports — the HTML file every major browser exports — deduplicates them, checks
which links still resolve, classifies them into a small flat set of collections with tags and a one-line
description, and writes a document for import into Linkwarden. It reads one format and converts between none;
SQLite is the working store in between, and every export is a projection over it. Fetching page content and
syncing with a running Linkwarden instance are out of scope.

### bookscape Pipeline

```text
browser export (HTML)
  ↓ audit     read-only: what the file holds and what the rules would resolve
  ↓ import    parse into the database, preserving folder paths and dates
  ↓ dedupe    normalise URLs, collapse duplicates onto a survivor
  ↓ check     probe liveness: alive, dead, uncertain or skipped
  ↓ classify  rules first, then a model for the remainder
  ↓ review    inspect and correct; gates export
  ↓ export    Linkwarden JSON backup (default) or Netscape HTML
```

`audit` touches neither the database nor the network. It reports whether a file is importable, what it contains,
what deduplication would do, which domains and folders dominate it with their cumulative share, how many links each
configured rule would decide, and suggested rules for the largest uncovered domains. Coverage is measured over
deduplicated survivors with their merged folder paths, exactly as the real pipeline would see them.

### Parsing

Parsing and emitting go through `github.com/virtualtam/netscape-go/v2`. `ADD_DATE`, `LAST_MODIFIED`, `PRIVATE` and
`TAGS` are typed fields of the library's bookmark type, and the importer and exporter use those fields only. Other
anchor attributes, including base64 `ICON` payloads, are discarded at import; they are most of a typical export's
bytes and no supported importer reads them.

`import` guards against the parser's silent failure modes:

- The parser matches element and attribute names case-sensitively, so a file with lowercase tags parses to zero
  bookmarks without an error. `import` compares the parsed count against an independent count of anchors and
  refuses the file if they differ.
- Out-of-range timestamps are rescaled rather than rejected, so a WebKit-epoch value lands in 1970. `import`
  range-checks every date and refuses implausible ones.
- Titles are trimmed, since an unclosed anchor absorbs the following whitespace.
- A missing or wrong `DOCTYPE` is reported with the expected value.

Re-importing a file with the same SHA-256 is refused unless forced.

### URL Normalisation and Deduplication

Normalisation produces a comparison key, `url_norm`; the original URL is always kept and is what export emits. The
key lowercases the host, optionally strips `www.`, treats `http` as `https`, drops the fragment and a trailing
slash, removes tracking parameters (`utm_*`, `fbclid`, `gclid` and others, plus configured extras) and sorts the
remaining query parameters. Each step is configurable, because each can merge URLs that are genuinely distinct on
some site; `dedupe --dry-run` prints proposed merges and every merge is recorded in `merge_log`.

Within a group sharing a key:

| Field           | Survivor value                                                         |
| --------------- | ---------------------------------------------------------------------- |
| Survivor        | Earliest `add_date`; a dated row beats an undated one; then lowest id  |
| `add_date`      | Minimum; `NULL` when no row in the group has one                       |
| `last_modified` | Maximum                                                                |
| Title           | Longest non-empty                                                      |
| Description     | Longest non-empty imported `<DD>`                                      |
| Folder paths    | Union — a link filed in several folders carries all of them as signals |
| Tags            | Union                                                                  |
| Private         | True if any row was private                                            |

Losers are kept with `canonical_id` pointing at the survivor and are never deleted. Re-running after a further
import regroups every row sharing a key, so a newly imported earlier copy displaces the previous survivor without
leaving a chain of losers.

### Liveness

`check` probes survivors with bounded global and per-host concurrency, `HEAD` first and `GET` for results that may
be an artefact of `HEAD` or bot protection. Each result is stored as it completes, so a cancelled run records
nothing for links it did not reach and keeps everything it did.

| Response                                                        | Status      |
| --------------------------------------------------------------- | ----------- |
| 2xx, or a redirect chain ending in 2xx                          | `alive`     |
| 3xx with redirect following disabled                            | `alive`     |
| 404, 410, NXDOMAIN, connection refused                          | `dead`      |
| 401, 403, 405, 429, other 4xx, 5xx, timeout, resolver failure   | `uncertain` |
| Redirect chain longer than `max_redirects`                      | `uncertain` |
| Non-HTTP scheme (`javascript:`, `place:`, `file:`, `chrome://`) | `skipped`   |

`--only` selects `unchecked` (the default, which makes the pass resumable), `uncertain`, `dead`, `alive` or `all`.
The final URL of a redirect chain is stored; `stats --redirect-targets` lists destinations many links share, which
usually means a parked domain or a soft 404.

### Classification

Classification assigns each survivor one collection from a configured, deliberately small set, plus tags and a
one-line description. Collections are the single "where does this belong" axis and export as one flat level; tags
carry every other dimension.

**Rules** run first, configured in `bookscape.yaml`. A rule matches by domain of the normalised URL (`*.example.com`
matches subdomains only) or by original folder path (`Dev/*` matches `Dev` and everything beneath it,
case-insensitively); its entries are alternatives. The first matching rule that names a collection decides it; tags
accumulate from every matching rule. A rule result has confidence 1. Re-running the rules pass replaces earlier rule
results and never touches a model or manual result.

**The model pass** classifies what the rules left, in batches (75 by default), sending URL, title and folder paths
— never page content. The provider is configurable:

| `classify.provider`   | Notes                                                                       |
| --------------------- | --------------------------------------------------------------------------- |
| `anthropic` (default) | `claude-sonnet-5` by default                                                |
| `openai`              | Completion API; the Decisions API is not used because it emits no text      |
| `ollama`              | Local; a non-thinking model is required (see [Model Choice](#model-choice)) |

The response schema orders fields `id`, `description`, `collection`, `tags`, `confidence`, so the collection is
chosen after the model has described the page, and every result is matched back to its input by bookmark id rather
than array position. A collection outside the configured set is rejected. Rejected, unanswered and failed items are
re-queued once and then left unclassified for the next run; completed batches are persisted even on cancellation.
Every result stores its model, run id and a hash of the prompt and schema together.

**Review** lists classifications, least certain first, and records corrections as `manual`, which no pass ever
overwrites. A model result below `classify.min_confidence` (0.6 by default) is held back from export until a human
accepts or corrects it, which makes the review queue and the export gate one mechanism.

### Export and Linkwarden

`export` projects survivors into one of two documents. Both apply the same filters, the same flat collection set in
configured order with empty collections omitted, and the same description truncation, so either describes the same
archive.

| Property             | `json` (default)                                 | `html`                                     |
| -------------------- | ------------------------------------------------ | ------------------------------------------ |
| Document             | Linkwarden's own backup format                   | Netscape bookmarks                         |
| Linkwarden importer  | Linkwarden backup (`format: 0`)                  | Bookmarks HTML file (`format: 1`)          |
| Description          | Imported                                         | Written as `<DD>`; discarded by Linkwarden |
| Date                 | `importDate` and `createdAt`, RFC 3339           | `ADD_DATE`                                 |
| Undated entry        | Not expressible; export's own timestamp, counted | `importDate` arrives `null`                |
| Readable by browsers | No                                               | Yes                                        |

Held back by default, each reported with its count and remedy: collapsed duplicates, unclassified links, dead links
(`export.include_dead`), links with an inconclusive check (shipped by default; `export.include_uncertain`), and
model results below the confidence threshold that nobody has reviewed. A bookmark held back for several reasons is
counted once. Output is written through a temporary file and renamed, and an existing file is replaced only with
`--force`.

Linkwarden v2.16.3's importers behave as follows, as exercised by `tests/e2e/linkwarden`:

- Original dates land in `importDate`, exactly. `createdAt` is set to the import time by both importers.
- Titles and descriptions are truncated at 254 characters, tags at 49, URLs at 2047. Tags are reused across links.
- Collections are created unconditionally and never matched by name, so importing a document twice duplicates every
  collection. The document's `<H1>` does not become a collection.
- The upload is capped by `IMPORT_LIMIT`, 10 MB by default.
- The backup importer runs inside one transaction and reports `200 "Success."` even when that transaction fails. It
  fails when `pinnedLinks`, a collection's `links` or a link's `tags` is `null`, when a link has neither
  `importDate` nor `createdAt`, or when a collection has no string `name`. The JSON exporter always emits arrays
  and dates; a malformed URL is skipped by the importer without an error.

### bookscape Data Model

| Table                   | Contents                                                                                         |
| ----------------------- | ------------------------------------------------------------------------------------------------ |
| `import_batches`        | One row per imported file, keyed by its SHA-256                                                  |
| `bookmarks`             | Verbatim URL, `url_norm`, title, imported description, dates, private flag, `canonical_id`       |
| `source_paths`          | Original folder paths, many per bookmark                                                         |
| `liveness`              | Status, HTTP status, final URL, method, error and time of the last probe                         |
| `classifications`       | Collection, generated description, confidence, source (`rule`, `model`, `manual`) and provenance |
| `tags`, `bookmark_tags` | Tags and their source (`import`, `rule`, `model`, `manual`)                                      |
| `merge_log`             | Every deduplication merge with its reason                                                        |

Every field the parser reads is stored, whether or not a given export format can carry it. Export prefers a
generated description and falls back to the imported one.

### bookscape Known Limitations

- Linkwarden's HTML importer discards descriptions; only the JSON export carries them into Linkwarden.
- Both Linkwarden importers set `createdAt` to the import time. If Linkwarden's interface sorts or displays by
  `createdAt`, the preserved chronology is stored but not visible.
- An undated bookmark exported as JSON receives the export's timestamp, because the format has no way to state an
  unknown date.
- The model pass sees URL, title and folder paths only. Pages whose titles say nothing about them classify poorly.
- The model pass has been exercised against hosted providers only; no local model has been measured for it.
- Export emits one flat level of collections; nested collections are not supported.
- Duplicate detection is exact on the normalised key. Near-duplicates such as mirrors or localised URLs are not
  merged.

## sicht

### sicht Scope

sicht sorts a large image archive — typically a Google Takeout export — in passes. Metadata rules decide what they
can, a local vision model answers the rest, a human reviews what the model was unsure of, and only then are files
moved, with every move recorded in a manifest that can be undone.

All inference is local: sicht uses only the Ollama backend and has no code path to a hosted model, because an image
archive holds other people's personal data. It does not start, stop or install Ollama, does not read Takeout
`.zip` or `.tgz` archives (extracted directories only), and never deletes a file.

### Workspace and Identity

A workspace is a directory of images with sicht's state in `<root>/.sicht/`:

```text
<root>/
├── .sicht/
│   ├── sicht.db          # SQLite
│   ├── sicht.yaml        # Configuration, rules, stages and plans
│   ├── prompts/          # One prompt per stage
│   ├── manifests/        # One JSON manifest per apply or undo batch
│   ├── thumbs/           # Reserved for thumbnails
│   └── logs/
└── <images, any structure>
```

sicht finds the workspace by walking up from the current directory, or takes `--workspace`. `init` creates the
tree and default files and never overwrites an existing configuration or prompt.

A file is identified by the SHA-256 of its content; a location is a workspace-relative path with forward slashes,
and one hash may have several. Every result attaches to the hash, so moving a file — including by `apply` — never
invalidates anything that has been decided about it.

No folder structure is assumed. `scan` walks whatever directory it is given, and nothing in the defaults reads
meaning from a directory name, since Takeout's folder names are localised.

### sicht Pipeline

```text
scan → rules → run <stage> (open) → taxonomy → run <stage> (closed) → review → apply
                 ↑                                                              │
                 └──────────────────────── next facet ◀────────────────────────┘
```

Each step writes only to the database except `apply` and `undo`. Each result is a value for a **facet** — `kind`,
`platform` — and a facet is decided by a rule, a model or a human, in that order of increasing precedence.

### Scan

`scan` hashes every file with a configured extension (default `jpg`, `jpeg`, `png`, `heic`, `heif`, `webp`,
`gif`), extracts metadata with exiftool, pairs Takeout sidecars, reads dimensions and computes a 64-bit perceptual
hash. A file whose path, size and modification time are unchanged is not re-read. `--prune` forgets files no longer
on disk; `--full` re-reads everything.

- **exiftool** runs in batches of 200 through an argument file, with group prefixes kept (`-G1`), print conversion
  off (`-n`) and UTF-8 filenames. A batch that partly fails keeps the results for the files that succeeded.
- **Format and extension** are recorded separately. `format` comes from the file's content; Takeout exports contain
  JPEGs named `.HEIC` and animated WebP named `.gif`. `format_mismatch` exposes the difference to rules.
- **Takeout sidecars** are matched by constructing the expected sidecar name and truncating it the way Takeout
  does: the whole name, `.json` included, is capped at 51 characters (counted as characters, not bytes), the
  `.supplemental-metadata` suffix may be cut to any prefix, and a duplicate counter moves from before the media
  extension to before `.json`. The older `<name>.json` form is also accepted. A sidecar no media file claims is
  reported, never an error.
- **Perceptual hashes** need decoded pixels. PNG, JPEG, GIF and WebP decode in pure Go; HEIC and animated WebP do
  not, and get no perceptual hash.

### Rules

Rules are `expr-lang` expressions, compiled once and evaluated per file over a typed environment of the file's
metadata. The first matching rule per facet wins; a rule records its own confidence.

| Expression                         | Meaning                                                       |
| ---------------------------------- | ------------------------------------------------------------- |
| `has_tag(exif, "Make")`            | The tag exists in any EXIF group                              |
| `tags(exif, "UserComment")`        | Every value of the tag across all groups it appears in        |
| `format`, `ext`, `format_mismatch` | Container format, file extension, and whether they disagree   |
| `dims`, `screen_resolutions`       | The file's pixel size and the configured list of screen sizes |

Stage inputs and plans use the same language over the file's decided facets, with `decided("kind")` and
`undecided("kind")` testing whether a facet has a decision yet.

Rules use `format` rather than the extension and look tags up across groups, because the same marker can appear in
`ExifIFD` for some files and `XMP-exif` for others. String tests use equality, since fields such as `UserComment`
also carry unrelated editor data.

The shipped rules decide `screenshot` from the iOS `UserComment` marker or a configured screen resolution, and
`photo` from a camera `Make` with an `FNumber`. `screen_resolutions` ships empty: it is specific to an archive's
devices, and `sicht stats dims` derives it from the archive itself. For each pixel size it reports how many files
are already known screenshots, known otherwise, or undecided, with evidence measured over every file of that size;
`--yaml` emits only sizes with at least 95% evidence, no counterexample and at least five known files.

### Model Stages

A stage sends each selected image to the model with a prompt and a JSON Schema generated from the stage's field
definitions. Generation is constrained to the schema and the response is validated again afterwards; a response
that fails validation is retried once and then recorded as an error.

```yaml
stages:
  - name: kind
    mode: closed
    input: 'undecided("kind")'
    fields:
      - { name: visible_ui, type: string, max: 200 }
      - {
          name: kind,
          type: enum,
          values: [screenshot, photo, scan_document, graphic],
          decision: true,
        }
  - name: platform
    mode: open
    input: 'kind == "screenshot"'
    fields:
      - { name: visible_text_excerpt, type: string, max: 300 }
      - { name: platform, type: enum, taxonomy: platform, decision: true }
```

- **Schemas are built in one place** (`internal/sicht/schema`), with observational fields emitted before decision
  fields regardless of their order in the configuration. Generation is autoregressive, so a decision is
  conditioned on what the model has already written about the image. Ollama honours the declared property order.
- **Open and closed modes.** In `open` mode a taxonomy field is a short free-text label. `sicht taxonomy <facet>`
  aggregates the labels, groups variants that differ only in case and punctuation, and emits a pasteable
  `taxonomies` block; the facet then switches to `closed`, where the field becomes an enum of the curated values.
  Enums are capped at 50 values.
- **The escape hatch** is added only to a closed taxonomy field: an `other` value plus a `<facet>_other` free-text
  companion. An answer of `other` stores the companion's label and goes to review as uncurated. An inline enum gets
  no escape hatch; `other` in its own value list is an ordinary answer.
- **Values are named for what belongs in them.** Models rarely choose a value that names an absence; the default
  `kind` stage's catch-all is `graphic` — made rather than captured — and its prompt describes it.
- **Staleness.** Every result records the model, the exact weights, and hashes of the prompt and the schema. A file
  whose result was produced under the current four is skipped; changing any of them makes results stale, and
  `--rerun` re-processes them.
- **Payload.** Non-decision fields are stored with the result for display and search. Earlier facets can be passed
  into a later stage's prompt through `context`.

Images are scaled to `max_image_edge` (1280 px) before sending, and HEIC files are never sent. An image taller than
1:3 is handled by `model.tall_strategy`: `crop` (default) keeps the top, `tile` sends overlapping tiles, `scale`
shrinks the whole image.

### Confidence

A rule result carries the rule's configured confidence. A model result's confidence is derived from token log
probabilities:

- Only the tokens of the decision value are scored, never the surrounding JSON. Ollama reports probabilities
  before the grammar mask is applied, so structural tokens the grammar forced can carry near-zero probability.
- For an enum, the value's probability is renormalised over the values the grammar allowed, which is the honest
  measure of the model's preference; the unnormalised figure is kept as well.
- A thinking model's reasoning shares the token stream with the answer, so the answer is located as the last
  occurrence of the content in the reconstructed stream.
- The top alternatives at the value's first token are stored for review.
- A backend that reports no log probabilities produces no score, which is stored as absent rather than as zero.

A result below the stage's `review_below` threshold (0.75 by default) is queued for review.

### Review

`sicht review list <facet>` shows the queue least confident first, including results with no score, with
alternatives and the model's own observations; files a human has already decided are excluded.
`sicht review set <path-or-hash> <facet>=<value>` records a human decision, stored without a confidence
value. A human decision always wins, and nothing is deleted: the `decisions` view resolves human over rule over
model, most recent first.

### Inference Backend and Model Choice

sicht uses Ollama exclusively. It never starts Ollama; an unreachable server fails with a hint to run `sicht doctor`.
`run` ensures the configured model is present, and `sicht model pull` fetches it ahead of time. `sicht model info`
reports what the server knows about the model and whether it returns log probabilities. Requests run on
`model.workers` client-side goroutines (2 by default); server-side parallelism is governed by
`OLLAMA_NUM_PARALLEL`. Each request has its own timeout, and Ctrl-C lets in-flight requests finish before exiting.

#### Model Choice

The default model is **`qwen3-vl:8b-instruct`**, with `temperature: 0`, `seed: 42` and `num_ctx: 4096`; it needs
about 8 GiB of VRAM. `qwen3-vl:4b-instruct` is the smaller alternative. Accuracy and timing for the candidates are
in [Benchmarks][benchmark_kind].

Thinking models are unsuitable. Their reasoning shares the context window with the image and the answer, and on an
ambiguous image they can exhaust it before writing an answer. Ollama's plain `qwen3-vl` tags are the thinking
variants, and Ollama 0.40.3 does not disable thinking for them when asked. `sicht doctor` warns when the configured
model reports the `thinking` capability; `model.think` is passed to Ollama when set, for models that honour it.

### Apply, Manifests and Undo

A plan in `sicht.yaml` selects files and a target path template over their facet values:

```yaml
plans:
  default:
    where: 'decided("kind")'
    target: 'sorted/{{ .kind }}/{{ .platform | default "unknown" }}'
    mode: move # move | copy
    on_conflict: suffix # suffix | skip | fail
    include_companions: true
```

`apply` without `--execute` writes a manifest of planned operations and touches nothing. Every target is validated
before any file moves: path segments are sanitised for Windows (reserved characters, trailing dots and spaces,
device names such as `CON`), and absolute paths, `..` segments and drive letters are refused, because facet values
can come from a model.

A move tries a rename first. Across volumes it copies to a temporary file beside the target, syncs it, verifies the
SHA-256, renames it into place and only then deletes the source; an interruption between copy and delete leaves
two copies rather than none. Each operation is recorded in the manifest before the next begins, and manifests are
written atomically, so a crash leaves a valid manifest of exactly what completed.

- **Companions** move with their file when `include_companions` is set: the Takeout sidecar, and a Live Photo's
  motion file (same directory and stem, unindexed extension). When two indexed files share a stem, the companion
  goes to the first by format order (`heic`, `heif`, `jpg`, `jpeg`, `png`, `webp`, `gif`) and then by name; a
  companion assigned twice fails validation, and an unclaimed one is recorded as `companion_orphaned` and left in
  place. `include_sidecars` is accepted as a synonym.
- **Duplicates** — the same hash at several paths — move once, from the first path in sorted order; the rest are
  recorded as `duplicate_left`. The same photograph exported in several formats has different hashes and is
  treated as separate files; `stats dupes` lists such groups separately.
- **Undo** reverses a batch's completed moves in reverse order with the same verification, refuses a file whose
  content has changed unless forced, writes its own linked manifest, and removes directories the batch created
  once they are empty. Copies are not undone, because undoing a copy would delete a file. A batch id may be
  abbreviated to any unique prefix.

`sicht export` emits a batch's manifest (JSON, or YAML for reading), the list of batches, or one row per decided
file.

### Doctor

`sicht doctor [--json]` checks the environment and reports each check as `ok`, `warn`, `fail` or `skip`, with a
fix hint. A check whose precondition failed is skipped rather than failed, and only `fail` sets the exit code.

| Check             | Reports                                                                                          |
| ----------------- | ------------------------------------------------------------------------------------------------ |
| `environment`     | OS, and WSL; warns when the workspace is on a slow filesystem (`9p`, `drvfs`, network mounts)    |
| `exiftool`        | Present and at least 12.0                                                                        |
| `workspace`       | Writable, database opens, migrations current                                                     |
| `disk`            | At least 2 GiB free on the workspace volume                                                      |
| `ollama`          | Reachable and at least 0.12.11                                                                   |
| `ollama exposure` | Warns when `OLLAMA_HOST` is not loopback, since Ollama has no authentication on model management |
| `inference`       | Warns when the resolved backend is not on this machine                                           |
| `model`           | Present; fails without the `vision` capability; warns on the `thinking` capability               |
| `vram`            | Warns when a loaded model is partly on the CPU; skipped when the model is not loaded             |
| `memory`          | Warns when available host memory is below an estimate of twice the model's weights               |

The filesystem check reads `/proc/mounts` rather than inferring from the path, since WSL's mount root is
configurable. A CPU-only Ollama build shows up in the `vram` check.

### sicht Data Model

| Table / view | Contents                                                                                  |
| ------------ | ----------------------------------------------------------------------------------------- |
| `files`      | Hash, size, extension, dimensions, perceptual hash                                        |
| `locations`  | Relative path → hash, with modification time at full precision                            |
| `metadata`   | exiftool JSON and sidecar JSON per hash                                                   |
| `results`    | One row per hash, facet and source, with confidence, provenance, payload and alternatives |
| `decisions`  | View: the winning result per hash and facet                                               |
| `runs`       | One row per stage run with its statistics                                                 |
| `batches`    | One row per apply or undo batch, pointing at its manifest                                 |

### sicht Known Limitations

- **There is no web interface.** Review, taxonomy curation and batch handling are CLI-only, and thumbnails are not
  generated.
- **HEIC files cannot be shown to the model or thumbnailed**, because HEIC does not decode without cgo. They can be
  decided only by rules or by a human.
- **Video is not indexed**; Live Photo motion files move only as companions of their still.
- **The `platform` stage has not been validated end to end** on a real screenshot set, and the shipped `kind`
  prompt has been measured on one year of one archive. Screenshot and `scan_document` accuracy are effectively
  unmeasured.
- **`tile` and `scale`** for tall images are implemented but have not been compared on real data.
- **Near-duplicates** are hashed but not grouped or acted on.
- Native Windows is supported by the build and cross-compiled on every change, but sicht has been exercised on
  Linux and WSL.
- Only Ollama is supported as an inference backend.

## formr

### formr Scope

formr reads a Terraform module's native-syntax configuration and the schemas of the providers it uses, and writes
`SCHEMA.md`: every field of every provider configuration, resource and data source the module tree uses, and what the
module does with each. `formr check` regenerates the report in memory, compares it with the committed one and
evaluates a coverage policy. formr is a development tool. It never configures a provider, refreshes, plans or
applies, and it never prints a value from the module.

In scope are the analysed root and every local child module it calls (`./` or `../` sources); each call is its own
scope, so two calls to one directory are reported separately. Every resource and data-source type the tree declares
is inventoried in full, including fields the HCL never mentions; provider types the module does not declare are
not. Examples and tests are separate roots and are not part of a module's report.

### Schema Acquisition

Schemas come from `<engine> providers schema -json` run in the module directory, where the engine is `terraform`,
`tofu` or a path given with `--engine`. formr calls the executable with `os/exec` and passes `TF_IN_AUTOMATION=1`
and `TF_INPUT=0`, with a per-command timeout. `--init` first runs `init -backend=false -input=false`, which installs
what the lock file selects without upgrading. Alternatively `--schema-file` reads a snapshot written by
`formr snapshot`, and no engine runs.

Provider releases come from `.terraform.lock.hcl`, not from the schema's numeric `version`, which is the provider's
state-schema version. A snapshot carries the engine and the releases with it; a bare `providers schema -json`
document is accepted with a warning that its provenance is unknown. Only schema `format_version` 1.x is read.

### Inventory and Fingerprints

The inventory is recursive over attributes, nested blocks in every nesting mode, nested-type attributes, and object
types written in an attribute's `type`. Field paths are typed segments, rendered as `network[].ip` for a collection
element, `labels{}` for a map element and `timeouts.create` for a nested field. Containers are listed as rows, and
every count in the report is over leaves, so a block is not counted twice.

Each node carries a fingerprint: the first 12 hex digits of a SHA-256 over the node's raw schema JSON with
`description`, `description_kind` and `deprecation_message` removed and keys sorted. A reworded description does not
change it; a changed type, flag, nesting mode or nested field does, as does any schema key formr does not itself
model.

### Expression Tracing

For each field a declaration sets, formr traces the expression back to its origins: an input variable of the
analysed root, or an attribute of another declaration. The trace follows locals, child-module arguments and outputs,
`dynamic` blocks with default or custom iterators, `for_each` and `each`, conditionals, tuple and object
constructors, `for` expressions, splats and index steps, and a fixed list of built-in functions:

| Functions                                                    | Treatment                                                              |
| ------------------------------------------------------------ | ---------------------------------------------------------------------- |
| `try`, `coalesce`, `sensitive`, `nonsensitive`               | Any argument may pass through unchanged                                |
| `tolist`, `toset`, `tomap`, `one`, `element`, `lookup`       | Values pass through; the collection type changes                       |
| `merge`, `concat`                                            | Arguments are combined; literal keys in `merge` are reported as forced |
| Other pure built-ins (`tonumber`, `yamlencode`, `length`, …) | The result depends on the arguments but is not one of them             |
| `file`, `templatefile` and other file functions              | Unresolved: formr reads no files                                       |
| Provider functions and any built-in not listed               | Unresolved                                                             |

Count and `for_each` expressions are traced separately, including what only selects or gates a value, and reported
once per declaration. A variable that only decides whether a resource exists is therefore never listed as an origin
of that resource's fields. A resource inside a module called with `count` or `for_each` is reported as conditional.

### Field Classification

| Configurable field | Meaning                                                                          |
| ------------------ | -------------------------------------------------------------------------------- |
| `direct`           | One input variable reaches the field unchanged                                   |
| `transformed`      | Input variables reach it through a function or operator, or several are combined |
| `derived`          | Another declaration's attribute reaches it                                       |
| `fixed`            | Set, but no input reaches it                                                     |
| `omitted`          | Not set                                                                          |
| `unresolved`       | Something on the way could not be followed; never counted as covered             |
| `caller-owned`     | A provider configuration field, set by whoever configures the provider           |

Computed-only fields have no input classification. A set field is `conditional` when a condition or a `dynamic`
block's `for_each` decides whether it is set, and `coupled` when one variable drives several fields of the same
declaration directly. Readable fields are `exported` when a root output reaches them, `partially-exported` when a
root output reaches part of a container or one element of a leaf, `internal` when another declaration, local or
condition reads them, and `unused` otherwise. Write-only fields have no result classification.

A provider configuration that the tree does not declare a `provider` block for is caller-owned. An aliased
configuration that nothing binds — no provider block, no `configuration_aliases`, no `providers` mapping — is an
error.

### Report and Policy

`SCHEMA.md` lists provenance, a summary of the leaf counts, diagnostics, every provider configuration and every
declaration, sorted by identity. It contains no timestamps, absolute paths or line numbers, and identical input
produces identical bytes regardless of file names or order. Tables are padded as Prettier pads them. Diagnostics
are printed to standard error with their source ranges.

`formr check` fails when the committed report differs from the regenerated one, and evaluates the `default` policy
against the regenerated analysis: the inventory is complete and no field is unresolved. Rewriting the report cannot
make a policy failure pass.

### formr Known Limitations

- **Exceptions and the `complete-resource-interface` policy are not implemented.** Every omitted or unresolved
  field is marked `needs-review`, and there is no way yet to record that an omission is intended.
- **Remote child modules, `.tf.json` files and override files are not analysed.** Each is reported as an error that
  makes the inventory incomplete.
- **Ephemeral resources, actions and provider functions** referenced by a module are not inventoried; a provider
  function in an expression is unresolved.
- **A traced reference is evidence of a dependency, not proof** that a caller controls the field's whole domain.
  Validation rules, type conversions and defaults are not evaluated.
- **The report covers the provider releases it was generated with.** It says nothing about other releases the
  module's version constraints allow.
- formr has been run against Terraform. The OpenTofu path uses the same commands and has not been exercised.
- A module whose provider configurations must be passed in through `configuration_aliases` cannot always be
  initialised on its own; formr does not yet build a schema-only root for it.

## Build and Distribution

### Build System

The build uses Bazel with Bzlmod and Gazelle-generated `BUILD` files. `.bazelversion` and `MODULE.bazel` pin Bazel,
rules_go, Gazelle and the Go SDK; the Go SDK version also appears in `go.mod`. No C toolchain is registered and
`.bazelrc` builds every target with cgo disabled, so a dependency that needs cgo fails to build. Race-detector
testing needs cgo and therefore runs as a separate `--config=race` that enables it for tests only.

Both release platforms are cross-compiled from one host by `scripts/bazel-build-platforms.sh`, which builds every
non-test target for each platform. `scripts/bazel-create-status.sh` produces the build stamp and depends only on
Bash, Git and coreutils.

### Release Artifacts

`scripts/release-create.sh` builds every application for `linux/amd64` and `windows/amd64` and packages, per
target:

- `<app>_<os>_<arch>.tar.gz` with one binary, `LICENSE` and `README.md`;
- `pertry_<os>_<arch>.tar.gz` with every binary;
- `CHECKSUMS_SHA256.txt` covering every tarball.

Binaries inside an archive carry their bare command name (`sicht`, or `sicht.exe` on Windows), and archive entries
are owned by root. File names carry no version, so `releases/latest/download/<file>` always names the newest
release.

### Release Process and Versioning

Versions follow Conventional Commits through semantic-release. The project is in 0.x: features and breaking changes
increment the minor version and fixes the patch version. The release script refuses any version that is not a plain
`0.x.y`.

Releasing is manual and gated: the Release workflow acts only when the `PERTRY_RELEASE_ENABLED` repository variable
is `true` and the workflow is dispatched, and it refuses a branch that has advanced past the commit the Testing
workflow verified. Prepared artifacts are retained for 14 days, so a partial publication can be completed without
rebuilding. The release procedure is in [Contributing][contributing_releases].

### Platform Support

Every application is supported on `linux/amd64`, including WSL2, and `windows/amd64`. sicht expects its workspace
on the native filesystem of the system it runs on — ext4 under WSL, NTFS under Windows — since a Windows drive
reached from WSL is markedly slower. Under WSL with a Windows-hosted Ollama, mirrored networking is required for
loopback to reach it.

## Privacy Notes

- **sicht** keeps everything local. It makes no network calls other than to `OLLAMA_HOST` and Ollama's model
  downloads, sends no telemetry, and logs OCR text and stage payloads only at `debug` level, into `.sicht/logs/`.
  `doctor` states whether inference is local from the resolved host.
- **bookscape** contacts every bookmarked host during `check`, which reveals the bookmark list to those hosts and to
  the network path. The model pass sends URLs, titles and folder paths to the configured provider; with
  `classify.provider: ollama` nothing leaves the machine.
- **formr** reads the module directory and runs the configured Terraform or OpenTofu executable in it. With
  `--init` that executable may download providers and modules from their registries; without it, formr makes no
  network calls of its own. It never configures a provider, so no provider talks to its API.
- **No application** collects data, and bookscape and sicht store their databases locally. Bookmark exports and image
  archives are personal data, and in an image archive often other people's; the repository's test fixtures are
  synthetic.

<!-- File references -->

[bookscape_readme]: ../cmd/bookscape/README.md
[sicht_readme]: ../cmd/sicht/README.md
[formr_readme]: ../cmd/formr/README.md
[contributing]: CONTRIBUTING.md
[contributing_releases]: CONTRIBUTING.md#-releases
[security]: SECURITY.md
[benchmarks]: benchmarks/README.md
[benchmark_kind]: benchmarks/sicht-kind-2026-10-13.md
