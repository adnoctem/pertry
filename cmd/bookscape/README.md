# bookscape

Salvage a decade of browser bookmarks into something worth keeping.

`bookscape` reads a Netscape bookmark export — the file every browser's "export bookmarks to
HTML" produces — deduplicates and enriches it in SQLite, and writes a document back out for import
into [Linkwarden](https://linkwarden.app/).

It reads one format and nothing else: `bookscape` is a filter, not a converter, and SQLite is the
working substrate in the middle rather than a destination. On the way out there are two documents
to choose from, because Linkwarden reads two and only one of them keeps the descriptions — see
[`export`](#export---type-jsonhtml--o-file---force).

This page is how to use it. How it works, and where its limits are, is described in
[docs/ARCHITECTURE.md](../../docs/ARCHITECTURE.md#bookscape).

---

## Contents

- [Install](#install)
- [The five-minute version](#the-five-minute-version)
- [Configuration](#configuration)
- [Commands](#commands)
- [Input and output compared](#input-and-output-compared)
- [What Linkwarden does with the file](#what-linkwarden-does-with-the-file)
- [Exit codes](#exit-codes)
- [Design notes that affect use](#design-notes-that-affect-use)

---

## Install

```bash
bazel build //cmd/bookscape          # -> bazel-bin/cmd/bookscape/bookscape_/bookscape
go build -o bookscape ./cmd/bookscape
```

No cgo, so a `windows/amd64` build needs no C toolchain:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./cmd/bookscape
bazel build --config=windows-amd64 //cmd/bookscape
```

## The five-minute version

```bash
# In Chrome: Bookmark manager -> ⋮ -> Export bookmarks

bookscape audit bookmarks.html    # free: what is in it, what the rules will do
bookscape import bookmarks.html
bookscape dedupe --dry-run        # read the proposed merges before applying
bookscape dedupe
bookscape check                   # slow: one HTTP request per link
bookscape classify                # rules first, then the model
bookscape review                  # confirm anything the model was unsure about
bookscape export
```

Then import the resulting `enriched-bookmarks.html` into Linkwarden. **Try 50 links first** —
import creates collections as a side effect, so unwinding a bad full run by hand is tedious.

Every stage reads and writes the same database and can be re-run. The slow and expensive ones,
`check` and `classify`, are resumable and will not redo work they have already done.

**Start with `audit`.** It costs nothing and tells you whether the file is importable and how much
of it the rules will resolve for free. On a real 1269-link export the shipped rules resolved 24%;
ten minutes of tuning from the audit report roughly halves what the model pass is billed for.

## Configuration

`bookscape` looks for `bookscape.yaml` in the working directory. **It is optional** — the
built-in defaults are a complete, usable configuration. `--config <path>` names a different file,
which must then exist.

Only the keys you set are overridden; everything else keeps its default, including nested siblings.
A three-line file is normal.

### Everything, with defaults

```yaml
database: ./bookscape.db

normalize:
  # Normalisation exists only to make two URLs comparable for deduplication. The original URL is
  # always kept and is what gets exported; this derives a comparison key beside it.
  #
  # Every rule is switchable because every one of them can merge URLs that are genuinely
  # distinct on some site. Run `dedupe --dry-run` and read the proposed merges.
  strip_tracking_params: true # utm_*, fbclid, gclid, msclkid, mc_cid, igshid, ref_src…
  extra_strip_params: [ref, source, at_medium]
  force_https: true # compare http and https as the same page
  strip_www: true # "www.example.com" and "example.com" as one host
  strip_fragment: true # #section is the same page
  strip_trailing_slash: true # except for a bare "/"
  sort_query_params: true # canonicalise ?b=2&a=1 to ?a=1&b=2

check:
  concurrency: 32 # requests in flight overall
  per_host_concurrency: 4 # …and to any one host. Both matter: see below.
  timeout: 10s # per request, including redirects
  user_agent: Mozilla/5.0 (compatible; bookscape/0.1)
  retry_uncertain_with_get: true # re-probe an inconclusive HEAD with a full GET
  follow_redirects: true
  max_redirects: 5

classify:
  # anthropic (default), openai, or ollama. Only the model pass calls a model at all, so this
  # is the only place a provider is chosen. Ollama is a real option here - this pass classifies
  # text, not images - though the hosted models write better descriptions.
  provider: anthropic
  model: claude-sonnet-5
  batch_size: 75 # links per model request
  min_confidence: 0.6 # below this, a model result needs review before export

  # The single "where does this belong" axis. Keep it small and shallow, 8 to 15 entries, and put
  # everything else in tags. A collection outside this set is rejected wherever it appears.
  collections:
    [
      Development,
      Infrastructure,
      Reference,
      Reading,
      Media,
      Hardware,
      Finance,
      Archive,
    ]

  # The free, deterministic first pass. Tune these against your own corpus with `audit`.
  rules:
    - match_domain: [github.com, gitlab.com, codeberg.org]
      collection: Development
      tags: [repo]
    - match_domain: [stackoverflow.com, "*.stackexchange.com"]
      collection: Reference
      tags: [qa]
    - match_domain: [youtube.com, youtu.be, vimeo.com]
      collection: Media
      tags: [video]
    - match_source_path: [Dev/*, Programming/*]
      collection: Development

export:
  format: json # json (Linkwarden's backup format) or html (Netscape bookmarks)
  output: "" # empty: ./enriched-bookmarks with the format's extension
  include_dead: false # ship links the liveness pass concluded are gone
  include_uncertain: true # ship inconclusive ones. On by default — see `check`.
  max_description_length: 200 # 0 means no limit
  title: bookscape export
```

### How a rule matches

```yaml
- match_domain: [github.com, "*.stackexchange.com"]
  match_source_path: ["Dev/*", "Reading"]
  collection: Development
  tags: [repo, code]
```

| Aspect              | Behaviour                                                                                                                                                                        |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The two lists       | **Alternatives, not conditions.** The rule fires if any entry in either matches.                                                                                                 |
| Collection          | The **first** matching rule that names one wins — a link belongs in exactly one.                                                                                                 |
| Tags                | **Accumulate from every** matching rule. Tags are many-per-link by nature.                                                                                                       |
| Tag-only rule       | Leaves the link undecided and still bound for the model pass: the collection is the axis export projects onto.                                                                   |
| `match_domain`      | Compared against the host of the _normalised_ URL, so it inherits lowercasing and (by default) `www`-stripping. `github.com` matches a link saved as `https://www.GitHub.com/…`. |
| `*.example.com`     | Subdomains **only**. `*.stackexchange.com` matches `superuser.stackexchange.com`, not `stackexchange.com`. List both to cover both.                                              |
| `match_source_path` | `Dev/*` matches `Dev` itself **and** everything under it. A bare `Dev` matches only `Dev`.                                                                                       |
| Path case           | Matched case-insensitively — folder names are hand-typed and a decade of them is not consistent.                                                                                 |

A rule naming a collection that is not in `classify.collections` makes the config invalid. That is
the closed set doing its job: a typo would otherwise create a stray collection in Linkwarden on
import.

### Credentials

Only `classify --pass=model` needs any, and which ones depends on `classify.provider`:

| Provider    | Credentials                                                                     |
| ----------- | ------------------------------------------------------------------------------- |
| `anthropic` | `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, or a profile from `ant auth login` |
| `openai`    | `OPENAI_API_KEY`                                                                |
| `ollama`    | None. Honours `OLLAMA_HOST`; nothing leaves the machine                         |

Nothing else in `bookscape` authenticates to anything.

## Commands

Global flags, valid on every subcommand: `--config <file>`, `--database <path>` (overrides
`database` from the config), `--log-level debug|info|warn|error`. Logs go to stderr so output
stays pipeable.

### `audit <file.html>`

Read-only. Touches no database, makes no network call, calls no model. A pure function of the file
and the config, so re-run it freely while editing rules.

```bash
bookscape audit bookmarks.html
bookscape audit --top 40 --suggest 20 --min-links 5 bookmarks.html
bookscape audit --suggest 0 bookmarks.html          # inventory only, no proposals
```

| Flag          | Default | Does                                                             |
| ------------- | ------- | ---------------------------------------------------------------- |
| `--top`       | 20      | Entries per breakdown. 0 for all.                                |
| `--suggest`   | 12      | Rules to propose for the uncovered remainder. 0 disables.        |
| `--min-links` | 3       | Fewest links a domain or folder needs before it is worth a rule. |

It reports, in this order: whether the file is importable at all (the two conditions that make
`import` refuse); what is in it; what deduplication would do; top domains and folders with
cumulative shares; per-rule hit counts; the uncovered remainder; and a pasteable YAML block of
suggested rules with the projected coverage gain.

Two flags in the per-rule table are worth knowing:

- **`<- never matches`** — the rule matched nothing. Usually a rule written for a different corpus.
- **`<- always shadowed by an earlier rule`** — it matched but never _decided_, because an earlier
  rule always won. The ordering is wrong, not the rule.

### `import <file.html>`

Parses the export into the database, preserving folder paths and the original `ADD_DATE`.
Idempotent via the file's SHA-256 — re-importing the same file is a no-op, not a second copy.

```bash
bookscape import bookmarks.html
bookscape import --force bookmarks.html    # import again as a new batch
```

Two checks **stop** an import rather than warn:

- **The entry count is reconciled** against an independent count of the anchor tags in the file.
  The underlying parser matches element and attribute names case-sensitively and discards anything
  else _without an error_, so a successful parse is not evidence that anything was parsed.
- **Every date is range-checked.** An out-of-range timestamp is silently rescaled by the parser
  rather than rejected, so a value meaning 2020 can land in 1970 with nothing said.

A real browser export trips neither. Run `audit` first if you want to know in advance.

### `dedupe [--dry-run]`

Groups by the normalised comparison key, elects a survivor, points the rest at it.

```bash
bookscape dedupe --dry-run     # print proposed merges, write nothing
bookscape dedupe
```

**Nothing is deleted.** A collapsed row keeps its own URL and dates and gains a `canonical_id`;
the export filter decides not to ship it. The merge is recorded in `merge_log`.

The survivor takes the earliest date in the group, the **union** of every original folder path
(a link filed under two folders carries two classification signals), the longest title, the
longest description, the union of tags, and `private` if _any_ copy was private.

**Run `--dry-run` first and read it.** Stripping `www.`, forcing `https` and sorting query
parameters will each merge a small number of genuinely distinct URLs, and the dry run prints both
original URLs for every proposed collapse — the only way to see that.

### `check [--only=<scope>]`

Probes each surviving link and records `alive`, `dead`, `uncertain` or `skipped`.

```bash
bookscape check                      # only links with no result yet (resumable)
bookscape check --only=uncertain     # retry the inconclusive bucket later
bookscape check --only=all
```

Scopes: `unchecked` (default), `uncertain`, `dead`, `alive`, `all`.

The third state is the point. A decade-old collection is full of links that are neither clearly
alive nor clearly gone — bot protection, rate limits, servers that reject `HEAD`, hosts that
happen to be down. Those are `uncertain` and **ship by default**; only a 404, a 410 or a host that
does not resolve is `dead`. A `javascript:` or `place:` bookmark is `skipped`, since there is
nothing to be uncertain about.

Results are written per link as they complete, so Ctrl-C is progress rather than loss, and the
default scope resumes. `per_host_concurrency` matters as much as `concurrency`: a bookmark
collection is heavily skewed toward a handful of domains, and pointing all 32 workers at one of
them is both rude and a fast route to a 429 that tells you nothing.

### `classify [--pass=rules|model] [--limit=N] [--dry-run]`

```bash
bookscape classify                              # rules, then the model on the remainder
bookscape classify --pass=rules                 # free; re-run after editing rules
bookscape classify --pass=model --dry-run       # what it would send, and to which model
bookscape classify --pass=model --limit=50      # try a small batch first
```

The **rules pass** is free and instant. Re-running it picks up edits to `classify.rules` without
disturbing anything the model or a human already decided — a `manual` row is never overwritten by
any pass, and that is enforced in SQL, not just in Go.

The **model pass** handles only what the rules could not decide. Batched, with the collection list
supplied so the model assigns rather than invents, and validated again after decoding. Every result
records the model, the run id and a hash of the prompt and schema together, because the prompt will
be iterated on.

A failed batch is re-queued once rather than failing the run, and everything that completed is
persisted even on Ctrl-C — a paid pass must not throw away batches already paid for.

> The generated **description** is what this pass earns its cost with — and it does **not** reach
> Linkwarden. See [below](#what-linkwarden-does-with-the-file).

### `review [--only=<filter>] [--sample=N]`

```bash
bookscape review                                    # what is blocking the export
bookscape review --only=all --sample=100
bookscape review accept --all                       # "these are all fine"
bookscape review accept 412 413
bookscape review set 412 collection=Infrastructure
bookscape review set 412 tags=dns,networking description="The DNS RFC index"
bookscape review set 412 --replace-tags tags=dns
```

Filters: `pending` (default — only what blocks the export), `model`, `all`, `unclassified`.

This is the gate in front of the irreversible step. A model classification below
`classify.min_confidence` does not reach the export until a human deals with it here. Either action
writes `source = 'manual'`, which no later pass overwrites. `set` changes only the keys you pass,
so correcting a collection leaves a good generated description alone.

The table puts the original folder path next to the assigned collection, because that is the
comparison that exposes a wrong one.

### `stats [--top=N] [--redirect-targets]`

```bash
bookscape stats
bookscape stats --redirect-targets
```

Counts by domain, collection, tag and liveness state, plus a date histogram over the original save
dates — the view that makes a decade-old archive legible.

`--redirect-targets` lists destinations several links resolve to. A large count there usually means
a parked domain, a generic 404 served with a 200, or a site that moved everything to its front page.

### `export [--type json|html] [-o <file>] [--force]`

```bash
bookscape export                              # JSON, to ./enriched-bookmarks.json
bookscape export --type html                  # Netscape, to ./enriched-bookmarks.html
bookscape export -o /tmp/subset.json --force
```

Two formats, and **they are not equivalent**:

|                                        | `json` (default)                 | `html`                     |
| -------------------------------------- | -------------------------------- | -------------------------- |
| What it is                             | Linkwarden's own backup document | Netscape bookmarks         |
| Pick it in Linkwarden as               | "From Linkwarden"                | "From Bookmarks HTML file" |
| Generated descriptions                 | **Arrive**                       | **Discarded on import**    |
| Dates, titles, tags, collections, URLs | All arrive                       | All arrive                 |
| Readable by a browser                  | No                               | Yes                        |

`json` is the default because it is the only one of the two that preserves everything the pipeline
produces. The description `classify` generates is the field that pass earns its cost with, and
Linkwarden's HTML importer throws it away — the format carries it and `bookscape` writes it, but
the importer looks for it in a position no real bookmark file uses. Verified against a live
v2.16.3, both directions: through JSON, 49 of 50 test links arrive with their description
byte-identical; through HTML, all 50 arrive with it empty. `export --type html` says so on stdout,
so the loss is never silent.

Use `html` to import somewhere that is not Linkwarden, or back into a browser.

`export.format` sets the default and `--type` overrides it. `netscape` and `linkwarden` are
accepted as aliases, since those are the names the question usually comes with. With no `-o` the
output is `./enriched-bookmarks` plus the format's extension, so changing the format cannot
quietly write JSON into a file named `.html`; passing an extension that disagrees with the format
is allowed and noted on stdout.

One thing the JSON format cannot express is "date unknown", so a bookmark that carried no
`ADD_DATE` is given the export's own timestamp and the count is reported. Through HTML such a
bookmark arrives with a null date instead. The real corpus has none.

Held back by default, each reported with its count and its remedy:

| Held back                          | Clear it with                               |
| ---------------------------------- | ------------------------------------------- |
| Duplicates collapsed by `dedupe`   | nothing — this is the intent                |
| Bookmarks with no collection       | `bookscape classify`                        |
| Dead links                         | `export.include_dead: true`                 |
| Inconclusive liveness checks       | ship by default; `export.include_uncertain` |
| Below `min_confidence`, unreviewed | `bookscape review`                          |

An export count smaller than expected should be accounted for by that table rather than a mystery.
Written through a temp file and renamed, so an interrupted export leaves either the old file or the
complete new one. Overwriting needs `--force`, since a previous export may already have been
imported somewhere.

> Import 50 links first, whichever format you choose. Neither importer reuses an existing
> collection — both call create unconditionally — so importing the same document twice gives you
> two of every collection, and unwinding a bad full run by hand is genuinely tedious.

### `version [--json]`

Print the build this binary came from. Works with no workspace and with an unparsable
configuration, which is frequently when it is wanted.

```console
$ bookscape version
application      bookscape
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

## Input and output compared

A Chrome export in (abridged, and real exports carry a base64 `ICON` on most anchors):

```html
<!DOCTYPE NETSCAPE-Bookmark-file-1>
<META HTTP-EQUIV="Content-Type" CONTENT="text/html; charset=UTF-8">
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><H3 ADD_DATE="1374257895" PERSONAL_TOOLBAR_FOLDER="true">Bookmarks bar</H3>
    <DL><p>
        <DT><H3 ADD_DATE="1385000000">Dev</H3>
        <DL><p>
            <DT><A HREF="https://github.com/golang/go" ADD_DATE="1385000100"
                   ICON="data:image/png;base64,iVBORw0KGgoAAAANSUhEUg…">golang/go</A>
        </DL><p>
        <DT><A HREF="http://www.github.com/golang/go/" ADD_DATE="1600000100">golang/go</A>
    </DL><p>
</DL><p>
```

And out, with `--type html`:

```html
<!DOCTYPE NETSCAPE-Bookmark-file-1>
<!-- This is an automatically generated file.
     It will be read and overwritten.
     DO NOT EDIT! -->
<TITLE>bookscape export</TITLE>
<H1>bookscape export</H1>
<DL><p>
    <DT><H3>Development</H3>
    <DL><p>
        <DT><A HREF="https://github.com/golang/go" ADD_DATE="1385000100"
               LAST_MODIFIED="1600000100" PRIVATE="0" TAGS="repo">golang/go</A>
        <DD>The Go programming language's canonical repository.
    </DL><p>
</DL><p>
```

What changed, and why:

|                          | Input                                                                                   | Output                                                                                                                               |
| ------------------------ | --------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| Structure                | The browser's folder tree, 90 folders deep in places                                    | One flat level, one folder per collection; tags carry every other dimension                                                          |
| The two `golang/go` rows | Two entries, different URLs and dates, different folders                                | One. `ADD_DATE` is the **earliest** of the group, `LAST_MODIFIED` the latest, and both original folder paths informed classification |
| `URL`                    | `http://www.github.com/golang/go/` kept verbatim                                        | Verbatim too. Normalisation only ever derives a comparison key; it never rewrites what ships                                         |
| `ICON`                   | base64 favicon on most anchors — 1180 of 1269 in a real export, and most of its 1.18 MB | Gone. Discarded at import; Linkwarden fetches its own previews                                                                       |
| `TAGS`                   | Chrome writes none                                                                      | From rules and the model pass                                                                                                        |
| `<DD>`                   | Chrome writes none                                                                      | The generated description                                                                                                            |
| `PRIVATE`                | Chrome writes none                                                                      | Always written; `1` if any copy of the link was private                                                                              |
| Size                     | 1.18 MB for 1269 links                                                                  | 64 KB for the 275 that were classified                                                                                               |

Everything the Netscape format models is a typed field, and **no other attribute is emitted at
all** — which is also what makes it impossible to emit the same attribute twice.

The default format describes the same archive as JSON. The same two rows, after the same
deduplication and classification:

```json
{
  "collections": [
    {
      "name": "Development",
      "description": "",
      "links": [
        {
          "url": "https://github.com/golang/go",
          "name": "golang/go",
          "description": "The Go programming language's canonical repository.",
          "importDate": "2013-11-21T04:15:00Z",
          "createdAt": "2013-11-21T04:15:00Z",
          "tags": [{ "name": "repo" }]
        }
      ]
    }
  ],
  "pinnedLinks": []
}
```

Two differences are worth noticing. The description is a first-class field rather than a `<DD>`,
which is the whole reason this is the default. And `LAST_MODIFIED` and `PRIVATE` have no
counterpart — Linkwarden's importer reads neither from either format, so nothing is lost by their
absence, and both remain in `bookscape.db` where `review` can show them.

## What Linkwarden does with the file

Measured against a live Linkwarden v2.16.3 by the end-to-end test in
[`tests/e2e/linkwarden`](../../tests/e2e/linkwarden). These are properties of the importer on the
far side, not of `bookscape`.

> **`<DD>` descriptions do not import — through the HTML importer.** Seven placements were probed
> — sibling on the next line (what `bookscape` and every browser emit), same line, explicitly
> closed, inside the anchor, before the anchor, and a `DESCRIPTION` attribute — and every one
> arrives empty. A Chrome export behaves identically, so this is Linkwarden's limitation, not
> ours.
>
> **Through the JSON importer they arrive**, byte-identical, including at the 254-character
> truncation boundary. That is why `json` is the default.

| Channel     | Result                                                                                                              |
| ----------- | ------------------------------------------------------------------------------------------------------------------- |
| `ADD_DATE`  | Exact, to the second — into the `importDate` column, **not** `createdAt`. An undated entry arrives `null`, not 1970 |
| `createdAt` | Set to the import time. If Linkwarden's UI sorts by it, the chronology is preserved and simultaneously invisible    |
| Title       | Byte-identical, truncated at **254** characters. An empty title stays empty                                         |
| `TAGS`      | Split on commas, each truncated at **49**, shared tags reused rather than duplicated                                |
| URL         | Byte-identical, including query string, fragment and percent-encoding                                               |
| UTF-8       | Emoji, CJK and combining accents all survive — without the `<META charset>` tag `bookscape` omits                   |
| Folders     | One collection each, flat. The document `<H1>` does **not** become a collection                                     |
| `<DD>`      | **Discarded** by the HTML importer. The JSON importer reads `description` and keeps it, truncated at **254**        |

Also: `IMPORT_LIMIT` caps the upload at 10 MB by default (both formats; the JSON document is the
smaller of the two), and URLs truncate at 2047 characters. `max_description_length: 200` sits
safely under the 254-character cut, so our own default is never the binding constraint.

Three things about the JSON importer are worth knowing before you drive it by hand, because each
one discards the **entire** import while the endpoint answers `200 "Success."` — it runs the
import in a transaction and logs the rejection rather than reporting it:

- `pinnedLinks`, every collection's `links` and every link's `tags` must be arrays. A `null` is
  dereferenced without a guard.
- Every link needs `importDate` or `createdAt`. With neither, the date is invalid and the write
  is rejected.
- Neither importer reuses a collection, so a second import of the same document doubles them.

`bookscape export` cannot produce any of the first two, and there are tests to keep it that way.
If you are writing the document yourself, read
[`tests/e2e/linkwarden`](../../tests/e2e/linkwarden) first.

## Exit codes

| Code | Meaning                                                              |
| ---- | -------------------------------------------------------------------- |
| 0    | Success.                                                             |
| 1    | The run failed. Partial work is saved; re-running resumes.           |
| 2    | Usage error: a bad flag, a missing argument, an invalid config file. |

## Design notes that affect use

- **Nothing is ever deleted.** Deduplication marks and points; dead links are flagged. The export
  filter is the only thing that decides what ships, and it is entirely config-driven.
- **`bookscape.db` is a plain SQLite file.** Query it directly when `stats` does not answer your
  question. The tables are described in [docs/ARCHITECTURE.md](../../docs/ARCHITECTURE.md#bookscape-data-model).
- **No cgo anywhere.** SQLite is `modernc.org/sqlite`, so the `windows/amd64` build needs no C
  toolchain. `.bazelrc` enforces it by building everything with rules_go's `pure` setting.
- **Only two things reach the network**, and only when asked: `check` makes one request per link,
  and `classify --pass=model` calls the Anthropic API. `audit`, `import`, `dedupe`, `review`,
  `stats` and `export` make none.
