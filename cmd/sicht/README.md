# sicht

Sort a large image archive by sifting it.

Metadata and deterministic rules first, a local vision model only for what the rules cannot decide,
a human review pass for what the model was unsure about, and only then any file movement — recorded
in a manifest that can be undone.

**Nothing leaves this machine.** Inference runs against a locally installed Ollama, and `doctor`
says so out loud rather than leaving you to infer it from the absence of an API key.

> **Status.** Everything documented below is implemented: `init`, `doctor`, `scan`, `rules`,
> `stats`, `apply`, `undo`, `export`, `model`, `run`, `taxonomy`, `review` and `version`. There is
> no web interface yet — see [Not yet implemented](#not-yet-implemented). How sicht works, and where
> its limits are, is described in [docs/ARCHITECTURE.md](../../docs/ARCHITECTURE.md#sicht).

## Contents

- [Install](#install)
- [The two-minute version](#the-two-minute-version)
- [The workspace](#the-workspace)
- [Commands](#commands)
  - [`init [directory]`](#init-directory)
  - [`doctor [--json]`](#doctor---json)
  - [`scan`](#scan)
  - [`rules`](#rules)
  - [`stats`](#stats)
  - [`apply`](#apply)
  - [`undo`](#undo)
  - [`export`](#export)
  - [`model info` / `model pull`](#model-info--model-pull)
  - [`run <stage>`](#run-stage)
  - [`taxonomy <facet>`](#taxonomy-facet)
  - [`review <facet>`](#review-facet)
  - [`version [--json]`](#version---json)
- [Configuration](#configuration)
  - [Everything, with defaults](#everything-with-defaults)
  - [The key that is deliberately not filled in, and the model](#the-key-that-is-deliberately-not-filled-in-and-the-model)
  - [Prompts](#prompts)
- [Environment variables](#environment-variables)
- [Exit codes](#exit-codes)
- [Running under WSL](#running-under-wsl)
- [What is stored, and where](#what-is-stored-and-where)
- [Not yet implemented](#not-yet-implemented)

## Install

```sh
# From the repository root.
go build -o sicht ./cmd/sicht

# Or with Bazel, which is what CI uses.
bazel build //cmd/sicht:sicht
```

Two external dependencies, neither vendored, neither started for you:

| Dependency                                     | Needed for                   | Install                                                                                               |
| ---------------------------------------------- | ---------------------------- | ----------------------------------------------------------------------------------------------------- |
| [exiftool](https://exiftool.org) 12.0 or newer | metadata extraction (`scan`) | `apt install libimage-exiftool-perl` · `brew install exiftool` · `winget install OliverBetz.ExifTool` |
| [Ollama](https://ollama.com) 0.12.11 or newer  | the vision stages (`run`)    | see ollama.com. `sicht` never starts it.                                                              |

Neither is needed for `init`. `doctor` reports on both, and tells you how to fix what is missing.

## The two-minute version

```sh
cd /path/to/your/archive
sicht init                                        # create .sicht/
sicht doctor                                      # is this machine ready?
sicht scan                                        # index everything
sicht rules                                       # decide what can be decided for free
sicht stats dims --where '!has_tag(exif, "Make")' # which sizes are screenshots?
sicht apply --plan default                        # a dry run: writes a manifest, moves nothing
sicht apply --plan default --execute              # actually file them
sicht undo <batch-id> --execute                   # change your mind
```

Nothing before `apply --execute` modifies a single one of your files. `init` creates `.sicht/` beside your images and touches nothing else;
`doctor` tells you whether the machine is ready for the commands that do the work.

A real session, on a machine with exiftool not yet installed:

```console
$ sicht init
Workspace at /home/you/archive
  /home/you/archive/.sicht
  sicht.yaml                   written
  prompts/kind.md              written
  prompts/platform.md          written
  sicht.db                     created

One thing in the default configuration is deliberately not filled in:
  screen_resolutions is empty. Rules must not match invented device dimensions;
  populate it from your own files once you have scanned them.

model.name is qwen3-vl:8b-instruct, chosen by benchmark. If your GPU cannot hold it,
qwen3-vl:4b-instruct is the smaller choice - not the plain qwen3-vl tags, which are
thinking variants that can reason until there is no room left to answer.

Next: sicht doctor

$ sicht doctor
  ok    environment      linux/amd64 (WSL)
 FAIL   exiftool         not found on PATH
                         install it: `apt install libimage-exiftool-perl`, `brew install exiftool`,
                         or `winget install OliverBetz.ExifTool`. Metadata extraction has no fallback.
  ok    workspace        /home/you/archive/.sicht, schema current
  ok    disk             832.4 GiB free
 FAIL   ollama           not reachable at http://127.0.0.1:11434
                         start Ollama. Under WSL, confirm networkingMode=mirrored in .wslconfig, or
                         point OLLAMA_HOST at the Windows host explicitly. sicht never starts it for you.
  ok    ollama exposure  http://127.0.0.1:11434 is loopback; nothing leaves this machine
 skip   inference        backend unavailable
 skip   model            Ollama is not reachable
 skip   vram             Ollama is not reachable

4 ok, 0 warn, 2 fail, 3 skipped
$ echo $?
3
```

Note the `skip`s. A check whose precondition failed is skipped, not failed — one unreachable server
should not produce six failures that all mean the same thing, and the first failure you read should
be the one that explains the rest.

## The workspace

Everything `sicht` knows lives in `.sicht/`, in the directory holding your archive. Your images are
never moved, renamed or modified by anything other than `apply --execute`, which writes a manifest
first.

```text
archive/
├── 2019/                     your files, untouched
├── IMG_0001.HEIC
└── .sicht/
    ├── sicht.db              SQLite: hashes, metadata, results, batches
    ├── sicht.yaml            configuration, rules and stage definitions
    ├── prompts/              prompt templates the stages refer to by name
    │   ├── kind.md
    │   └── platform.md
    ├── thumbs/               reserved for thumbnails
    ├── manifests/            one JSON file per apply or undo batch
    └── logs/
```

Every later command finds the workspace by **walking up from the current directory**, the way `git`
finds `.git`. So this works:

```console
$ cd /home/you/archive/2019/holiday/raw
$ sicht doctor --json | jq -r '.results[] | select(.name=="workspace") | .detail'
/home/you/archive/.sicht, schema current
```

`--workspace <dir>` overrides the search. The difference matters: a `--workspace` that does not
exist is a **usage error** (exit 2), because you named it, while finding no workspace by walking up
is not an error at all — `doctor` still reports every environment check, which is exactly what you
want before you run `init`.

Paths in the database and in manifests are stored relative to the workspace root with forward
slashes on every platform, so a manifest written by the Windows build still resolves under WSL
against the same files.

## Commands

Global flags, accepted by every subcommand:

| Flag              | Default                         | Meaning                                                |
| ----------------- | ------------------------------- | ------------------------------------------------------ |
| `--workspace DIR` | discovered by walking up        | Workspace root. Must exist if given.                   |
| `--config FILE`   | `<workspace>/.sicht/sicht.yaml` | Configuration file.                                    |
| `--log-level L`   | `info`                          | `debug`, `info`, `warn` or `error`. Logs go to stderr. |
| `--no-color`      | off                             | Suppress ANSI colour. `NO_COLOR` does the same.        |

Logs go to **stderr** and output to **stdout**, so `doctor --json | jq` works at any log level.

### `init [directory]`

Create a workspace. The directory defaults to `.` and is the workspace root.

```sh
sicht init                      # the current directory
sicht init /mnt/photos/archive  # somewhere else
```

Creates `.sicht/` with the default `sicht.yaml`, the prompt templates the default stages refer to,
and an empty database with its schema applied. Opening the database at `init` time rather than
lazily means `doctor` has something to report on and the first real command does not pay for it.

**Idempotent, and specifically so.** Re-running `init` creates whatever is missing and overwrites
nothing:

| Already present  | What `init` does   | Why                                                                        |
| ---------------- | ------------------ | -------------------------------------------------------------------------- |
| `sicht.yaml`     | leaves it alone    | By the time you re-run `init` it holds a curated taxonomy and tuned rules. |
| a prompt file    | leaves it alone    | A tuned prompt is your work too, and every result records a prompt hash.   |
| a missing prompt | writes it          | A stage refers to it by name and would otherwise fail at run time.         |
| `sicht.db`       | applies migrations | Upgrading is the same operation as creating.                               |
| your images      | nothing, ever      | `init` writes only under `.sicht/`.                                        |

### `doctor [--json]`

Check that the machine can do the work, and say what to do about anything that cannot.

```sh
sicht doctor              # human-readable, coloured
sicht doctor --no-color   # for a log or a file
sicht doctor --json       # machine-readable
```

Four statuses:

| Status | Meaning                                                                    | Affects the exit code |
| ------ | -------------------------------------------------------------------------- | --------------------- |
| `ok`   | Checked and fine.                                                          | no                    |
| `warn` | Works, but something will be slower, less accurate or less private.        | no                    |
| `FAIL` | Will not work. The hint says what to do.                                   | **yes — exit 3**      |
| `skip` | Not checked, because something it depends on failed, or it does not apply. | no                    |

A warning never changes the exit code. Exiting non-zero on advice would make `doctor` unusable in a
script on a machine that works fine.

Every check, in the order it is reported:

| Check             | `ok`                                     | `warn`                                                                                                                           | `FAIL`                                                    |
| ----------------- | ---------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| `environment`     | OS, architecture, and WSL if detected    | the workspace is under `/mnt/` on WSL (9P: slow enough to look hung)                                                             | —                                                         |
| `exiftool`        | on PATH, 12.0 or newer                   | older than 12.0, or present but `-ver` fails                                                                                     | not on PATH — metadata extraction has no fallback         |
| `workspace`       | writable, database opens, schema current | —                                                                                                                                | not writable, will not open, or a migration did not apply |
| `disk`            | at least 2 GiB free                      | under 2 GiB                                                                                                                      | —                                                         |
| `ollama`          | reachable, 0.12.11 or newer              | older (no logprobs, so confidence falls back to agreement mode; several advisories are only fixed later), or no version reported | not reachable                                             |
| `ollama exposure` | `OLLAMA_HOST` is loopback                | it is not — Ollama has no auth on its model-management endpoints                                                                 | —                                                         |
| `inference`       | the backend is on this machine           | it is not, so image data and OCR text would leave the machine                                                                    | —                                                         |
| `model`           | present, reports the vision capability   | not pulled (`run` would fetch it anyway)                                                                                         | present but no vision capability                          |
| `vram`            | the loaded model fits in VRAM            | partly or wholly on the CPU                                                                                                      | —                                                         |

`--json` emits nothing but JSON, and emits every check whatever its outcome, so a consumer can rely
on the shape:

```console
$ sicht doctor --json | jq -c '.results[] | {name, status}'
{"name":"environment","status":"ok"}
{"name":"exiftool","status":"fail"}
{"name":"workspace","status":"ok"}
...
$ sicht doctor --json | jq -r '.results[] | select(.status=="fail") | "\(.name): \(.hint)"'
exiftool: install it: `apt install libimage-exiftool-perl`, ...
```

The `hint` field is omitted when empty, so "no advice" is distinguishable from an empty string.

### `scan`

Index the archive: content hashes, metadata, Takeout sidecars, perceptual hashes.

```sh
sicht scan                      # the whole workspace
sicht scan --root 2019          # one subdirectory
sicht scan --full               # re-read everything, even unchanged files
sicht scan --prune              # forget files that are no longer on disk
```

Your files are only read. Everything written goes under `.sicht/`.

**Re-running is cheap**, and this is what makes the pipeline usable. A file whose path, size and
modification time are unchanged since the last scan is skipped without being opened. Measured on
the archive this was developed against — 1337 files, 1.5 GB, over a 9P-mounted Windows drive:

| Run                      | Time   |
| ------------------------ | ------ |
| First scan               | 25.9 s |
| Re-scan, nothing changed | 0.79 s |

| Flag             | Default             | Meaning                                                                |
| ---------------- | ------------------- | ---------------------------------------------------------------------- |
| `--root DIR`     | the whole workspace | Limit the scan to a subdirectory.                                      |
| `--full`         | off                 | Re-read every file. Use after upgrading, or if you suspect a bad read. |
| `--prune`        | off                 | Remove database entries for files no longer present.                   |
| `--workers N`    | one per CPU         | Concurrent hashing and decoding.                                       |
| `--batch-size N` | 200                 | Files per exiftool call and per transaction.                           |

`--prune` is off by default on purpose, and is scoped to what the run actually walked: scanning one
subdirectory must not delete what it did not look at, and an unplugged drive must not erase a year
of decisions.

What the report tells you:

```console
$ sicht scan
  candidates         1337
  scanned            1337
  skipped            0
  sidecars paired    1337
  unclaimed sidecars 220  (a sidecar for a file whose extension is not indexed, e.g. a video)
  no visual hash     12  (not decodable in a pure-Go build, e.g. HEIC)
  elapsed            25.892s
```

- **unclaimed sidecars** — a `.json` no indexed file claimed. On a Takeout export the expected
  cause is a video, since videos are not in the default `extensions` list. A large number would
  mean a sidecar naming variant this build does not recognise.
- **no visual hash** — files that could not be decoded, so near-duplicate detection cannot see
  them. Expect this to be your real HEIC count plus any animated WebP.

### `rules`

Decide what can be decided without a model: expressions over metadata, no inference, no cost.

```sh
sicht rules                 # evaluate and record
sicht rules --dry-run       # report what would be decided, write nothing
sicht rules --limit 100     # just the first hundred files
```

Run this before any AI stage. What the rules decide is what the model never has to look at, and
the report ends with the number that sizes — and prices — the model pass:

```console
$ sicht rules
files evaluated        1337
files decided          411
decisions              411
previous retracted     399
elapsed                257ms

Per rule, in order. Earlier rules win, so a later rule for the same facet only
sees what the earlier ones left.

  [0]     111     kind = screenshot  !has_tag(exif, "Make") && "Screenshot" in tags(exif, "UserComment")
  [1]       0  <- kind = screenshot  !has_tag(exif, "Make") && dims in screen_resolutions
  [2]     300     kind = photo  has_tag(exif, "Make") && has_tag(exif, "FNumber")

Per facet:

  kind
    photo                       300   22.4%
    screenshot                  111    8.3%
    (undecided)                 926   69.3%   <- what a model pass would have to look at
```

The `<-` marks a rule that fired zero times. That is the most useful thing in the report: in a
configuration file, a rule that matches nothing looks exactly like a correct one. Rule `[1]` is
expected to be dead until you populate `screen_resolutions` — see [`stats dims`](#stats).

**Previous rule results are retracted first.** An edited rule set changes what each rule index
means, so a result from `rule:3` must not outlive the rule that produced it. A human decision or a
model result is never touched: precedence is resolved when decisions are read, not by deleting
anything.

#### Writing rules

Rules live in `sicht.yaml` and are `expr-lang` expressions. The available variables:

| Variable                     | Type            | Notes                                                                              |
| ---------------------------- | --------------- | ---------------------------------------------------------------------------------- |
| `format`                     | string          | The real format from the file's bytes, e.g. `jpg`. **Prefer this over `ext`.**     |
| `ext`                        | string          | What the filename claims. Wrong for about one file in seventy in a Takeout export. |
| `mime`                       | string          | e.g. `image/jpeg`.                                                                 |
| `format_mismatch`            | bool            | `ext` and `format` disagree.                                                       |
| `exif`                       | map             | exiftool's tags under group-qualified names, e.g. `exif["IFD0:Make"]`.             |
| `dims`                       | string          | `"1242x2688"`. Empty when unknown.                                                 |
| `width`, `height`            | int             | Zero when unknown.                                                                 |
| `megapixels`                 | float           |                                                                                    |
| `portrait`, `landscape`      | bool            | A square image is neither.                                                         |
| `size`                       | int             | Bytes.                                                                             |
| `name`, `path`, `dir`        | string          | Workspace-relative, forward slashes.                                               |
| `camera`                     | string          | Make and model joined, `""` when none.                                             |
| `software`                   | string          | `IFD0:Software`. Evidence that a re-encoder touched the file.                      |
| `has_sidecar`                | bool            | A Takeout sidecar was found.                                                       |
| `taken`, `has_taken`, `year` | time, bool, int | Capture time, from the sidecar's `photoTakenTime` or EXIF.                         |
| `screen_resolutions`         | list            | The list from `sicht.yaml`.                                                        |

And the functions:

| Function                    | Meaning                                                                         |
| --------------------------- | ------------------------------------------------------------------------------- |
| `tags(exif, "UserComment")` | Every group's value for that tag, as a list. **Use this for an equality test.** |
| `tag(exif, "Make")`         | The first value, in sorted group order.                                         |
| `has_tag(exif, "Make")`     | Present in any group.                                                           |
| `has(exif, "IFD0:Make")`    | Present under that exact group-qualified key.                                   |

**Two traps worth knowing**, both of which cost real files in the archive this was developed
against:

1. **The extension lies.** 16 files named `.HEIC` there are plain JPEGs that Google transcoded
   without renaming, and 3 named `.gif` are WebP. A rule written `ext == "png"` silently does not
   fire for them. Use `format`.
2. **The group prefix matters.** The iOS screenshot marker lives in _both_ `ExifIFD:UserComment`
   (100 files) and `XMP-exif:UserComment` (105 files), union 111. A rule naming one group saw 99 of 111. Use `tags(...)`.

### `stats`

```sh
sicht stats              # overview: counts, sizes, extensions, what is decided
sicht stats facets       # what has been decided, by facet and value
sicht stats facets --all # every opinion ever recorded, including ones precedence discards
sicht stats dims         # pixel sizes, with the evidence to judge them
sicht stats dupes        # the same content, or the same photo, more than once
```

#### `stats dims`

This is the one to reach for, because it is how `screen_resolutions` gets filled in. sicht ships
that list **empty** — a rule must not match an invented device resolution — and this derives it
from your own files.

```console
$ sicht stats dims --where '!has_tag(exif, "Make")'
Filter: !has_tag(exif, "Make")
1037 of 1337 files match.

size           files    known  other unknown  evidence
1242x2688         83       82      0       1    98.8%  safe for a rule
750x1334          34       14      0      20    41.2%  only 41% evidence - do not use
1920x1080         21        0      0      21       -
800x800           20        0      0      20       -   square
```

| Column     | Meaning                                                                      |
| ---------- | ---------------------------------------------------------------------------- |
| `files`    | How many files have that size, after `--where`.                              |
| `known`    | How many are already decided as the target value by independent means.       |
| `other`    | How many are decided as something else. **One of these rules the size out.** |
| `unknown`  | How many have no decision — what a size rule would _newly_ decide.           |
| `evidence` | `known / files`, over **every** file at that size.                           |

**Why `evidence` is computed over all files, not just decided ones:** 750×1334 above has 14 known
screenshots and 20 files with no evidence either way. Measured over the decided files alone it
would report 100% and look perfectly safe. Measured honestly it is 41%, and a rule matching it
would mislabel twenty files. A size that is merely _common_ is not a safe one.

`--yaml` emits a pasteable block containing only the sizes that clear the bar:

```console
$ sicht stats dims --where '!has_tag(exif, "Make")' --yaml
screen_resolutions:
  - "1242x2688"   # 82 of 83 files are known screenshot (99%); the rule would newly decide 1
  - "2688x1242"   # 10 of 10 files are known screenshot (100%); the rule would newly decide 0
#
# Left out, backed by fewer than 5 files each (likely crops, not device sizes):
#   750x1217  (1 file)
```

| Flag               | Default      | Meaning                                                    |
| ------------------ | ------------ | ---------------------------------------------------------- |
| `--where EXPR`     | none         | Filter with the same language a rule uses.                 |
| `--facet NAME`     | `kind`       | Which facet to measure purity against.                     |
| `--value VALUE`    | `screenshot` | Which value a size should mean.                            |
| `--min-evidence F` | `0.95`       | `--yaml` includes only sizes at least this well supported. |
| `--min-files N`    | `5`          | `--yaml` ignores sizes backed by fewer known files.        |
| `--top N`          | `25`         | How many rows to show.                                     |

The `--min-files` floor earns its place: without it the reference archive suggested five sizes with
100% evidence on a sample of **one** each, every one of them a _cropped_ screenshot. Offering a
crop size as a device resolution is how a future photograph cropped to that size gets silently
mislabelled.

#### `stats dupes`

Two separate reports, neither acted on:

- **Byte-identical files** — one file in several places. `apply` moves the first path and records
  the rest.
- **Same stem, several formats** — the same photo exported more than once, e.g. `IMG_0007` as both
  `.heic` and `.jpeg`. Their bytes differ, so their hashes differ, so they are genuinely separate
  files with separate decisions. Reported so you can see them; sicht does not collapse them,
  because that would mean deciding one of your originals does not count.

`model`, `run`, `taxonomy` and `review` are where a model gets involved, and nothing about your images leaves the machine: inference runs against a locally
installed Ollama, and `doctor` says so out loud rather than leaving you to infer it.

> **Check `doctor` first, and look at the `vram` line.** A CPU-only Ollama build works and is
> about twenty times slower — 50 s per image instead of a few. The Homebrew formula is CPU-only;
> the official release bundles the CUDA runners. `doctor` reports "loaded entirely on the CPU"
> when this is happening, which is the only symptom you will get.

### `model info` / `model pull`

```sh
sicht model info    # what the server knows, and whether it can report logprobs
sicht model pull    # fetch the configured model deliberately
```

`run` pulls the model automatically on first use; `model pull` exists so that six gigabytes do not
download in the middle of a run.

`model info` reports whether the server returns **logprobs**, which decides whether confidence
scores are available at all. Without them nothing can be prioritised for review.

### `run <stage>`

```sh
sicht run kind                # the files the rules could not decide
sicht run kind --dry-run      # how many would be sent, without sending any
sicht run kind --limit 20     # a sample first
sicht run kind --rerun        # re-process files whose answer is already current
```

Run `sicht rules` first. What the rules decide is what the model never has to look at.

**Re-running is cheap.** Every result records the model, the exact weights, the prompt hash and the
schema hash. A file whose answer was produced under the current four is skipped. Edit the prompt
and they all become stale — which is the point, because an answer from an older prompt is not
comparable with a new one.

```console
$ sicht run kind
stage                    kind
files selected           87
sent to the model        86
decided                  86
for review               27  (below the review threshold, or an invented value)
undecodable              1  (no pure-Go decoder, e.g. a real HEIC)
elapsed                  1m11.624s
per image                833ms

model                    qwen3-vl:8b-instruct
weights                  0533d74300e4f9bc367…
prompt                   sha256:4c6f067c4d3c…
schema                   sha256:1351bcec998a…

To look at what needs review:
  sicht review list kind
```

That is a real run, over a copy of one year of the reference archive with the rules' decisions
removed so that every file reached the model. The same run on the plain `qwen3-vl:8b` tag answered 64 of 87 and failed the
other 22 outright, at about 24 seconds an image — see
the model section above for why.

The hashes are not decoration. They are what makes "I re-ran it after changing the prompt" mean
something.

#### Confidence, and why it is not the obvious calculation

A confidence score comes from the model's token log probabilities, and getting it right took a
measurement. Ollama reports logprobs **before** the grammar mask: at the position where the JSON
object must open, the forced token scored −15.80 while the model's own preference — a newline the
schema forbade — scored −0.00. So

- **only the decision value's own tokens are scored.** Including the structural tokens would report
  near-zero confidence for a perfectly correct answer, because it would be measuring how surprised
  the model was by JSON syntax.
- **the score is renormalised over the values the grammar allowed.** The raw figure understates the
  model's confidence given the choice it actually had. Both are kept; the gap between them is how
  much work the schema was doing.

A result with **no** score is not a result with zero confidence. The two are stored differently,
because conflating them would send your whole archive to the review queue.

### `taxonomy <facet>`

```sh
sicht taxonomy platform           # every label, grouped
sicht taxonomy platform --yaml    # a pasteable block to curate
```

For a stage in `open` mode, where the model writes a free label and the set of labels is discovered
rather than declared. The workflow:

1. Run the stage in `open` mode, perhaps with `--limit`.
2. `sicht taxonomy <facet>` groups the labels. Normalisation is lowercase, trim and punctuation —
   so `Twitter / X`, `twitter/x` and `twitter x` group together. Nothing cleverer; a near-miss the
   tool cannot see is one you will see in the list.
3. Curate by hand into `sicht.yaml` under `taxonomies.<facet>`: merge, rename, drop.
4. Switch the stage to `mode: closed` and `sicht run <facet> --rerun`.

A closed stage gets `other` plus a free-text `<facet>_other` companion, so the model can decline
rather than pick the closest wrong answer. A value it had to invent that way is **always** queued
for review, whatever its confidence: it means the taxonomy does not cover that file, which is a
fact about the taxonomy rather than about the model.

A facet is capped at 50 values. Beyond that, split it into separate stages rather than widening one
enum.

### `review <facet>`

```sh
sicht review list kind                            # least confident first
sicht review list kind --below 0.9 --limit 50
sicht review set 2019/IMG_0001.png kind=screenshot  # record your own decision
sicht review set abc123 kind=photo                  # or by hash prefix
```

Review is command-line only for now; a web interface for it is planned.

```console
$ sicht review list kind
12 item(s) for "kind" below 0.75, least confident first.

  31.2%  photo          2021/IMG_0944.JPG
                        or: screenshot 29%
                        visible_ui: a thin bar at the top, possibly a status bar
  44.8%  screenshot     2019/IMG_1182.PNG
                        or: photo 38%
```

The alternatives and the model's own observations are shown because they turn a correction into a
keystroke rather than a decision from scratch.

**A decision you record is final.** It is stored as a human decision and always wins for that
facet. Nothing is deleted: `sicht stats facets --all` still shows what the model said, which is how
you measure where it was wrong. A human decision carries no confidence value, because it is not a
probability — storing 1.0 would make it indistinguishable from a model that was very sure.

### `apply`

File your images into directories derived from what has been decided about them.

```sh
sicht apply --plan default            # DRY RUN: writes a manifest, moves nothing
sicht apply --plan default --execute  # actually move the files
sicht apply --plan default --limit 50 # plan only the first fifty
```

**A dry run by default.** It writes a complete manifest describing every move it would make and
touches nothing. Read it, then re-run with `--execute`.

```console
$ sicht apply --plan default
plan                     default
batch                    2026-10-12T10-28-33Z_default_f036
files selected           411
companions               612  (sidecars and Live Photo motion files)
unclaimed companions     218  (no indexed owner, so left alone)
operations planned       1241
manifest                 .../.sicht/manifests/2026-10-12T10-28-33Z_default_f036.json

Nothing was moved. Read the manifest, then:
  sicht apply --plan default --execute
```

Four behaviours worth knowing:

- **Every target is validated before anything moves.** A plan that would produce one unusable path
  fails without having already moved a thousand files.
- **Companions travel with their file.** A Takeout sidecar and a Live Photo motion file follow the
  image they belong to, so the pairing survives. 201 stills in the reference archive have a motion
  companion; moving the stills alone would break every one of them silently. A companion belongs to
  exactly one file, chosen by a fixed extension preference so the choice never varies between runs.
- **The same content is moved once.** If a hash exists at several paths, the first is moved and the
  others are recorded in the manifest as `duplicate_left`.
- **Nothing is overwritten.** The conflict policy is `suffix` by default, giving `IMG_1 (2).png`;
  `skip` and `fail` are the alternatives.

#### Writing a plan

```yaml
plans:
  default:
    where: 'decided("kind")'
    target: 'sorted/{{ .kind }}/{{ .platform | default "unknown" }}'
    mode: move # or copy
    on_conflict: suffix # or skip, fail
    include_companions: true
```

`where` sees everything a rule sees, plus `facets`, `decided("kind")`, `undecided("platform")` and
`facet("kind")`.

`target` is a Go template over the facet values, with `default`, `lower`, `upper`, `title`, `trim`,
`replace` and `slug` available. File attributes are under `.file`, e.g. `{{ .file.year }}`, so a
facet can never be shadowed by one.

> **Until the `platform` stage has run, the default target files everything under
> `<kind>/unknown`.** That is correct but not very useful; `target: 'sorted/{{ .kind }}'` is the
> sensible plan until then.

Every path segment is sanitised: characters Windows rejects, trailing dots and spaces, reserved
device names like `NUL`, and anything over 255 bytes. A target that tries to escape the workspace
with `..` is refused outright — a facet value can come from a model, so this is a boundary and not
a convenience.

### `undo`

```sh
sicht undo <batch-id>            # DRY RUN
sicht undo <batch-id> --execute  # put the files back
sicht undo <batch-id> --force    # move a file back even if it changed since
```

The batch id may be abbreviated to any unique prefix. `sicht export batches` lists them.

```console
$ sicht undo 2026-10-12T10-29 --execute
batch                    2026-10-12T10-29-42Z_default_a7ae
reversible operations    105
restored                 105
directories removed      5  (emptied by the undo)
```

- **A file edited since it was filed is reported and skipped, not moved back over the newer
  version.** `--force` overrides that, which is almost never what you want.
- **Only moves are reversed.** A copy is left alone: undoing a copy would mean deleting a file, and
  sicht does not delete files.
- Directories the batch created and the undo emptied are removed, so the layout really is restored.
  A directory you put something in is never touched.

Verified on 206 real files: `apply --execute` moved 105, `undo --execute` restored 105, and every
file hash and every directory was identical before and after.

### `export`

```sh
sicht export batches                          # every apply and undo run
sicht export manifest <batch-id>              # the canonical JSON
sicht export manifest <batch-id> --format yaml # the same, readable
sicht export results                          # one JSON object per file, per line
sicht export results --format csv             # for a spreadsheet
sicht export results --where 'year == 2019'   # narrowed, same language as a rule
```

Everything goes to stdout and logs go to stderr, so it all pipes:

```sh
sicht export results --where 'facet("kind") == "screenshot"' | jq -r .path
```

### `version [--json]`

Print the build this binary came from. Works with no workspace and with an unparsable
configuration, which is frequently when it is wanted.

```console
$ sicht version
application      sicht
version          0.3.0
commit           fa27264
branch           main
built            2026-10-12 18:06:09+02:00
built on         Ubuntu 24.04.5 LTS
built for        linux/amd64
go (toolchain)   1.26.9
go (runtime)     go1.26.9
```

Every field comes from Bazel's `--stamp`, fed by `scripts/bazel-create-status.sh`. A binary built
without it reports `redacted` for all of them and says so rather than passing a placeholder off as
a release version - which is the first thing worth checking on a bug report.

`--json` emits the same fields as an object, for a report template or a fleet inventory.

---

## Configuration

`.sicht/sicht.yaml`, written by `init`. A partial file is layered over the defaults, so you can
write three lines and change one thing:

```yaml
model:
  name: qwen3-vl:32b
  workers: 4
```

### Everything, with defaults

```yaml
model:
  # The Ollama model tag. Chosen by benchmark; read the model section below before changing it,
  # because the obvious alternative - the plain qwen3-vl:8b tag - is the one that fails.
  name: qwen3-vl:8b-instruct

  # How long Ollama holds the model in memory between requests. Reloading a vision model per
  # image otherwise dominates the runtime; holding it longer keeps its VRAM reserved after a run.
  keep_alive: 5m

  # Passed to Ollama verbatim. temperature 0 and a fixed seed make a re-run reproducible, which
  # is what lets a prompt change be attributed to the prompt.
  options:
    temperature: 0
    seed: 42
    num_ctx: 4096 # the image, the prompt and the answer share this window

  # Downscale an image's long edge before inference. Higher is slower, not obviously better.
  max_image_edge: 1280

  # How many requests are in flight client-side. Server-side parallelism is Ollama's own
  # OLLAMA_NUM_PARALLEL and is not set from here.
  workers: 2

  # How an image taller than 1:3 is sent: crop keeps the top, tile sends overlapping tiles,
  # scale shrinks the whole image.
  tall_strategy: crop

  # Confidence below which a result goes to the review queue, for a stage that sets none.
  review_below: 0.75

  # Turn a reasoning model's thinking phase on or off. Unset leaves it to the model; use a
  # non-thinking model rather than relying on this.
  # think: false

# The extensions `scan` indexes, without a leading dot.
extensions: [jpg, jpeg, png, heic, heif, webp, gif]

# Device dimensions rules match against. Ships EMPTY on purpose - see below.
screen_resolutions: []

# Deterministic rules, evaluated before any model call. First match wins per facet.
rules:
  - facet: kind
    value: screenshot
    when: '!has_tag(exif, "Make") && "Screenshot" in tags(exif, "UserComment")'
    confidence: 0.99
  - facet: kind
    value: screenshot
    when: '!has_tag(exif, "Make") && dims in screen_resolutions'
    confidence: 0.9
  - facet: kind
    value: photo
    when: has_tag(exif, "Make") && has_tag(exif, "FNumber")
    confidence: 0.98

# AI stages, each one pass over a selected set of files.
stages:
  - name: kind
    mode: closed # closed: the enum is fixed. open: free text, curated later.
    input: undecided("kind") # which files this stage runs over
    prompt_file: prompts/kind.md
    review_below: 0 # 0 inherits model.review_below; set it to override for this stage
    fields:
      # Observational fields come first and are stored as stage payload. Asking the model to
      # describe what it sees before it decides is what makes the decision better.
      - { name: visible_ui, type: string, max: 200 }
      # Exactly one field per stage is the decision: what lands in results as a facet value.
      - name: kind
        type: enum
        values: [screenshot, photo, scan_document, graphic]
        decision: true

  - name: platform
    mode: open
    input: kind == "screenshot"
    prompt_file: prompts/platform.md
    fields:
      - { name: visible_text_excerpt, type: string, max: 300 }
      - { name: visible_url, type: string, nullable: true }
      - { name: ui_description, type: string, max: 200 }
      - { name: platform, type: enum, taxonomy: platform, decision: true }

# Curated value sets for closed-mode enum fields, keyed by facet. Populated by `sicht taxonomy`.
taxonomies: {}

# File moves, as templates over facet values.
plans:
  default:
    where: decided("kind")
    target: sorted/{{ .kind }}/{{ .platform | default "unknown" }}
    mode: move # or copy
    on_conflict: suffix # or skip, fail
    include_companions: true # move the Takeout JSON sidecar and the Live Photo motion file too
```

Validation is strict about the things that would waste a run:

- A stage with no `decision: true` field is rejected: it would spend inference and record nothing.
- An enum needs either `values` or `taxonomy`, never both.
- A `closed` stage over an empty taxonomy is rejected, with the way out in the message: run the
  stage in `open` mode, curate with `sicht taxonomy <facet>`, then close it.
- An enum or taxonomy over 50 values is rejected. Split the facet into separate stages instead.
- An extension written with a leading dot is rejected rather than silently matching nothing.

### The key that is deliberately not filled in, and the model

`init` prints both. For `screen_resolutions` a plausible-looking value would be worse than an
obviously empty one; for the model, the plausible-looking alternative is the one that fails.

**`screen_resolutions: []`.** The second `kind` rule matches a file's dimensions against this list.
Populating it with invented or googled device resolutions would make that rule fire wrongly and
silently — it would classify photographs as screenshots and you would not find out. Fill it from
your own files, with `sicht stats dims`.

**`model.name: qwen3-vl:8b-instruct`.** Chosen by benchmark, against 85 hand-labelled files from one
year of the reference archive, every file sent to the model. The full write-up is in
[`docs/benchmarks`](../../docs/benchmarks).

| Model                  | Answered    | Correct, shipped `kind` stage | Per image | VRAM     |
| ---------------------- | ----------- | ----------------------------- | --------- | -------- |
| `qwen3-vl:8b-instruct` | 86 / 87     | **78 / 85 (92%)**             | 0.8 s     | ~8 GiB   |
| `qwen3-vl:4b-instruct` | 86 / 87     | 74 / 85 (87%)                 | 0.57 s    | ~5.5 GiB |
| `qwen3-vl:8b`          | **64 / 87** | not measured — eliminated     | 24 s      | ~8 GiB   |
| `qwen3-vl:4b`          | **61 / 87** | not measured — eliminated     | 24 s      | ~5.5 GiB |
| `gemma3:4b`            | 86 / 87     | not measured — eliminated     | 0.9 s     | ~4.5 GiB |

The 8b over the 4b on accuracy: both are right about everything but saved images, and the 4b files
5 of the 21 photographs as graphics where the 8b files none. That 92% is in-sample — the `kind`
prompt was revised after seeing that year's labels — and is owed a check on a year it was not
tuned on.

**Do not use the plain `qwen3-vl` tags.** They are the _thinking_ variants — Ollama's
`qwen3-vl:8b` is byte-identical to `qwen3-vl:8b-thinking` — and they reason before answering. The
reasoning shares the context window with the image and the answer, so on an ambiguous image the
model deliberates until the window is full and never writes the answer at all: a quarter of the
files, at thirty times the cost. Setting `think: false` does not help; Ollama 0.40.3 runs the
reasoning anyway. `sicht doctor` warns if the configured model reports the `thinking` capability.

`gemma3:4b` looks good on that table and is not a choice: changing two sentences of the prompt
took it from 12 to 6 of 13 correct against the rules, wrong at confidence 1.0 while sending
nothing to review. A model whose confidence stays at 1.0 when it is wrong cannot drive a
confidence-gated review queue.

If your GPU cannot hold the 8b, use `qwen3-vl:4b-instruct`. The VRAM column is the model's share
on top of whatever the desktop already holds, at the shipped `num_ctx: 4096`.

**The `kind` stage's values matter as much as the model.** Its fourth value, `graphic`, covers saved
wallpapers, logos, cover art and other images that were made rather than captured, and the default
prompt describes it. A catch-all named for an absence, such as `other`, is rarely chosen by a model,
which then files those images as photos or screenshots with high confidence; the
[benchmark](../../docs/benchmarks/sicht-kind-2026-10-13.md) measures the difference. If you add a
catch-all value of your own, give it a name that says what belongs in it.

### Prompts

`.sicht/prompts/*.md`, referenced by a stage's `prompt_file`. Plain Markdown, no templating; the
schema is generated from the stage's `fields`, so a prompt describes _how to look_ and never what
shape to answer in.

Both defaults are written to ask for observations before a decision — "describe the interface you
can see before you decide" — because a model that has just described depth of field and lens noise
is measurably less likely to then call a photograph a screenshot.

Edit them freely. Every result records a hash of the prompt that produced it, so a result from an
older prompt is reported as stale rather than silently conflated with a newer one, and `init` never
overwrites a prompt you have touched.

## Environment variables

| Variable      | Effect                                                                                                                                                                       |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `OLLAMA_HOST` | Where to reach Ollama. Default `http://127.0.0.1:11434`. A non-loopback value makes `doctor` warn twice: once for exposure, once because inference would no longer be local. |
| `NO_COLOR`    | Any value suppresses ANSI colour, like `--no-color`.                                                                                                                         |

## Exit codes

| Code | Meaning               | When                                                                                  |
| ---- | --------------------- | ------------------------------------------------------------------------------------- |
| `0`  | success               |                                                                                       |
| `1`  | generic error         | The work was attempted and failed.                                                    |
| `2`  | usage error           | An unknown subcommand or flag, a bad flag value, a `--workspace` that does not exist. |
| `3`  | environment not ready | `doctor` reported at least one `FAIL`.                                                |
| `4`  | partial failure       | Some items failed and some succeeded. Reserved; nothing emits it yet.                 |

`3` exists so a wrapper can run `doctor` and branch on the answer without parsing output:

```sh
sicht doctor >/dev/null 2>&1 || { echo "not ready"; sicht doctor; exit 1; }
```

`4` exists so that a partial pass over ten thousand images is not indistinguishable from a total
failure.

## Running under WSL

Both halves of this are things `doctor` will tell you.

**Keep the archive on the Linux filesystem.** A workspace under `/mnt/c` goes over 9P, and hashing
a few thousand images across it is slow enough to look like a hang. `doctor` warns when it sees
one. Either move the archive onto the WSL ext4 disk, or build for Windows and run that against it —
there is no cgo anywhere in this repo, so `GOOS=windows go build ./cmd/sicht` is a clean
cross-compile.

**Ollama on the Windows host is not on WSL's localhost** unless mirrored networking is on. Either
set `networkingMode=mirrored` in `.wslconfig`, or point `OLLAMA_HOST` at the Windows host
explicitly — and note that the second option makes inference non-local by the definition `doctor`
uses, so it will say so.

## What is stored, and where

Nothing outside the workspace, and nothing about your images leaves the machine.

| What                              | Where                                              |
| --------------------------------- | -------------------------------------------------- |
| content hashes, sizes, dimensions | `.sicht/sicht.db` → `files`                        |
| where each file was seen          | `.sicht/sicht.db` → `locations`                    |
| EXIF and sidecar JSON             | `.sicht/sicht.db` → `metadata`                     |
| facet values and provenance       | `.sicht/sicht.db` → `results`                      |
| apply batches                     | `.sicht/sicht.db` → `batches`, `.sicht/manifests/` |

Results are keyed by **content hash, never by path**, which is what lets a result survive the file
being moved — including by `apply` itself.

Every result records where it came from: `human`, `rule:<index>` or `model:<name>`, plus the model
digest, prompt hash and schema hash for a model result. A `decisions` view resolves the winner per
facet — **human beats rule beats model** — so a human correction always takes effect without
anything rewriting or deleting what the model said. What the model thought is kept, because the
review UI needs to show it and `stats` needs to measure agreement.

---

## Not yet implemented

Planned in [docs/TODO.md](../../docs/TODO.md):

| Command | What it will do                                  |
| ------- | ------------------------------------------------ |
| `serve` | A local web interface for review, loopback only. |
