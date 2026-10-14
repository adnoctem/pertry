# ✅ TODOs - `pertry`

## ➕ Additions

- [ ] `sicht serve`: a local web interface over `internal/sicht/core` with a dashboard (counts per facet value and
      stage status, stages run with live progress over SSE), a lazy-loaded gallery filterable by facet, value,
      confidence and source, a keyboard-driven review queue, taxonomy curation that writes `taxonomies` back to
      `sicht.yaml`, batch preview with execute and undo, and the `doctor` checks rendered. Server-rendered with
      templ and htmx 4, assets vendored and embedded, no JavaScript framework. It binds to loopback only (a
      non-loopback address only behind an explicit override, with a warning), accepts only `127.0.0.1:<port>` and
      `localhost:<port>` as `Host`, wraps every state-changing handler in `http.CrossOriginProtection`, sends a
      Content Security Policy allowing only self-hosted scripts and styles, and serves images by hash only. Each of
      those controls is covered by a test. No session token.
- [ ] Thumbnails: 384 px on the long edge, JPEG quality 80, cached by hash under `.sicht/thumbs/`, generated on first
      request or by `scan --thumbs`, with a placeholder for HEIC.
- [ ] `sicht`'s `content` stage, with the `platform` facet passed as context, a `category` taxonomy and an `action`
      decision (`check_out`, `remember`, `discard`).
- [ ] Near-duplicate grouping from the stored perceptual hashes, shown in the gallery.
- [ ] `formr` exception annotations next to the HCL (`# formr:exception field=… policy=…` with a required
      `# formr:reason`), bound to their declaration and provider, rejecting unknown fields, duplicates, stale
      references and wildcards; then the `complete-resource-interface` policy that requires a public mapping or an
      exception for every configurable resource leaf, and an export or an exception for every readable one.
- [ ] `formr check` comparison by row identity (added, removed and changed fields) instead of by line.
- [ ] `formr`: a temporary schema-only root for modules whose providers must be passed in through
      `configuration_aliases`, carrying the lock file's selections.
- [ ] `formr`: remote child modules resolved from initialised module metadata; `.tf.json` and override files.
- [ ] Port the Hetzner server module's local API harness (`scripts/test-local-api.py`) to Go with `httptest`,
      keeping its request and plan assertions; decide whether it belongs to `formr` or to `tests/`.
- [ ] Opt-in hosted inference for `sicht`, for archives that hold nothing confidential: a `model.provider` key
      defaulting to `ollama`; no use of a non-local backend unless named explicitly, and no fallback to one when
      Ollama is unreachable; a startup warning naming what leaves the machine; `doctor` reporting the provider and
      its locality; and an estimate of cost before a run. The backends already exist in `pkg/infer`, and a
      `Decider` would replace logprob-derived confidence for closed facets. Only after the local path has been used
      on a full archive.

## ✏️ Planned Changes

- [ ] Run the `platform` stage open → `taxonomy` → closed on a real screenshot set, with confidence and prompt
      staleness checked end to end. This is the outstanding part of validating the model stages.
- [ ] Re-measure the `kind` stage on a year of the archive its prompt was not tuned on, with fresh blind labels. The
      published 92% is in-sample.
- [ ] Measure screenshot and `scan_document` accuracy; the existing ground truth holds one screenshot and no
      documents.
- [ ] Compare `tall_strategy` `tile` and `scale` against `crop` on an archive that contains scrolling captures.
- [ ] Measure `bookscape`'s model pass against a local non-thinking Ollama model.
- [ ] Verify whether Linkwarden's interface sorts or displays links by `createdAt`, which both importers set to the
      import time, against the target instance.
- [ ] Report Linkwarden's HTML importer upstream: it reads a description from a `<DD>` position no real bookmark
      file uses.
- [ ] Run `formr` against OpenTofu on the pilot module, and add an `integration`-tagged test of the engine
      adapter against installed providers.
- [ ] Generate and review the Hetzner server module's `SCHEMA.md`, then retire its Python coverage script and
      manifest once `formr check` covers every structural-drift error the script can raise.
- [ ] Run `sicht` natively on Windows, including `apply` and `undo` across volumes.
- [ ] Performance: scan 10 000 files in well under ten minutes on an RTX 4080 class machine, excluding exiftool's
      first start.
- [ ] Remove the Release workflow's `bootstrap` input once `v0.1.0` exists, and narrow the Checkov `CKV_GHA_7`
      suppression in `.github/linters/.checkov.yaml` to the remaining `sha` input.

## 💡 Ideas

- [ ] HEIC preview through an optional external decoder, reported by `doctor` like exiftool, since cgo libheif is
      ruled out.
- [ ] Index video with frame extraction, if a cgo-free decoder becomes available.
- [ ] Detect the operating system and architecture in the README's one-line install instead of naming
      `linux_amd64`.

## 🔗 Links

- [Linkwarden backup importer (v2.16.3)](https://github.com/linkwarden/linkwarden/blob/v2.16.3/apps/web/lib/api/controllers/migration/importFromLinkwarden.ts)
- [Linkwarden HTML importer (v2.16.3)](https://github.com/linkwarden/linkwarden/blob/v2.16.3/apps/web/lib/api/controllers/migration/importFromHTMLFile.ts)
- [Ollama API reference](https://github.com/ollama/ollama/blob/main/docs/api.md)
