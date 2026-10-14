# formr

Find out how much of its providers' API a Terraform module actually exposes.

`formr` reads a module's HCL and the schemas of the providers it uses, and writes `SCHEMA.md`:
every field of every provider configuration, resource and data source the module uses — including
the ones the HCL never mentions — and what the module does with each. `formr check` then fails CI
when that report goes stale, or when something in it could not be analysed.

It never configures a provider, plans or applies, and it never prints a value from your module:
mappings are symbolic (`var.name`), and fixed values are reported as `fixed` without their
contents.

This page is how to use it. How it works, and where its limits are, is described in
[docs/ARCHITECTURE.md](../../docs/ARCHITECTURE.md#formr).

---

## Contents

- [Install](#install)
- [The two-minute version](#the-two-minute-version)
- [Reading the report](#reading-the-report)
- [Commands](#commands)
- [In CI](#in-ci)
- [Exit codes](#exit-codes)
- [Not yet implemented](#not-yet-implemented)

---

## Install

```bash
bazel build //cmd/formr              # -> bazel-bin/cmd/formr/formr_/formr
go build -o formr ./cmd/formr
```

formr needs a `terraform` (or `tofu`) executable to read schemas, unless you give it a snapshot
with `--schema-file`.

## The two-minute version

```bash
cd terraform-hetzner-server
formr schema . --init --output SCHEMA.md    # init once if the module is not initialised
git add SCHEMA.md

# later, and in CI
formr check . --report SCHEMA.md
```

`--init` runs `terraform init -backend=false -input=false`: it installs the providers the lock
file selects, without upgrading anything and without a backend. Leave it off when the module is
already initialised.

## Reading the report

The summary counts leaves — fields with no fields of their own — so a block is never counted
twice. Each declaration then gets a table with one row per field:

| Column       | Values                                                                                     |
| ------------ | ------------------------------------------------------------------------------------------ |
| Field        | `name`, `timeouts.create` (nested field), `network[].ip` (collection element), `labels{}`  |
| Input        | `direct`, `transformed`, `derived`, `fixed`, `omitted`, `unresolved`; `—` if computed-only |
| Availability | `unconditional`, `conditional` (a condition or `dynamic` block decides), `coupled`         |
| Result       | `exported` (a root output reaches it), `partially-exported`, `internal`, `unused`          |
| Review       | `needs-review` for anything omitted or unresolved                                          |
| Fingerprint  | A short hash of the field's schema; it changes when the provider changes the field         |

Input mappings, briefly:

- **`direct`** — one input variable reaches the field unchanged: `var.name`.
- **`transformed`** — variables reach it through a function or several are combined. A `merge`
  that sets literal keys says so: `transformed: var.labels; forces managed_by`.
- **`derived`** — it comes from another declaration, such as a data source's `id`.
- **`fixed`** — set, but no input reaches it.
- **`omitted`** — not set at all. This is usually what you are looking for.
- **`unresolved`** — formr could not follow the expression, and says why. It is never counted
  as covered.

Each declaration's header says what decides whether it exists at all (`Instances: conditional on
var.create_server`). That variable is deliberately not listed against every field: switching a
resource on does not configure its fields.

Provider configuration fields — `token`, `endpoint` — are `caller-owned` in a reusable module:
they belong to whoever configures the provider, and are not missing inputs.

## Commands

All commands take the module directory as their argument, defaulting to the current one.

### `schema [dir] [--output FILE]`

Writes the report to `--output`, or to standard output. It writes a truthful report even when the
module has gaps or something could not be analysed; diagnostics go to standard error with file
and line, and into the report without them.

### `check [dir] [--report SCHEMA.md] [--policy default]`

Regenerates the report in memory and compares it with `--report`, printing the lines that
differ. Then it evaluates the policy against what it just analysed, not against the file — so
editing `SCHEMA.md` by hand cannot make it pass. It writes nothing.

The `default` policy requires a complete inventory and no unresolved mappings.

### `snapshot [dir] --output FILE`

Saves the module's provider schemas together with the engine and provider releases that produced
them. Pass the file to `schema` or `check` as `--schema-file` to run without Terraform.

### `version [--json]`

Prints the version, commit and toolchain the binary was built from.

### Flags shared by `schema`, `check` and `snapshot`

| Flag            | Default     | Meaning                                      |
| --------------- | ----------- | -------------------------------------------- |
| `--init`        | off         | Run `init -backend=false -input=false` first |
| `--engine`      | `terraform` | `terraform`, `tofu`, or a path to either     |
| `--schema-file` | —           | Read schemas from a snapshot; no engine runs |
| `--timeout`     | `5m`        | Timeout for each engine command              |
| `--log-level`   | `info`      | `debug`, `info`, `warn` or `error`           |

## In CI

```bash
formr check . --init --report SCHEMA.md
```

The job needs network access for `--init` to download providers, and no cloud credentials:
formr never configures a provider. Commit the module's `.terraform.lock.hcl`, or pin providers
exactly: the report is about the releases it was generated with, and a fresh `init` against
broad constraints can legitimately select a newer one and report drift.

## Exit codes

| Code | Meaning                                                             |
| ---- | ------------------------------------------------------------------- |
| `0`  | Success                                                             |
| `1`  | `check`: the report is out of date, or the policy is not met        |
| `2`  | Bad arguments, the engine failed, the HCL did not parse, or similar |

## Not yet implemented

- Exception annotations (`# formr:exception …`) for recording that an omission is intended, and
  the `complete-resource-interface` policy that relies on them.
- Remote child modules, `.tf.json` and override files, which are reported as errors.
- A schema-only root for modules whose provider configurations must be passed in through
  `configuration_aliases`.
