<p align="center">
    <!-- pertry -->
    <img src="https://github.com/adnoctem/artwork/blob/8a9d84ca8d91a61bd1f69fdc1ba6362ccbcb5060/projects/pertry/icon/pertry.svg?raw=true" alt="pertry Logo" width="256">
    <h1 align="center">pertry</h1>
</p>

[![License](https://img.shields.io/github/license/adnoctem/pertry?label=License)][license]
[![go.mod version](https://img.shields.io/github/go-mod/go-version/adnoctem/pertry?logo=go)][go]
[![Language](https://img.shields.io/github/languages/top/adnoctem/pertry?label=Go&logo=go)][go]
[![Testing](https://github.com/adnoctem/pertry/actions/workflows/testing.yaml/badge.svg)][ci_testing_workflow]
[![GitHub Release](https://img.shields.io/github/v/release/adnoctem/pertry?label=Release)][github_releases]
[![GitHub Activity](https://img.shields.io/github/commit-activity/m/adnoctem/pertry?label=Commits)][github_commits]
[![Bazel](https://img.shields.io/badge/Bazel-built-brightgreen?logo=bazel&logoColor=43A047)][bazel]
[![Renovate](https://img.shields.io/badge/Renovate-enabled-brightgreen?logo=renovate&logoColor=1A1F6C)][renovate]
[![PreCommit](https://img.shields.io/badge/PreCommit-enabled-brightgreen?logo=precommit&logoColor=FAB040)][precommit]

`pertry` is an open-source [MIT][license]-licensed collection of local-first command-line applications, written
in [Go][go]. Each one is a single static executable with no runtime
dependencies, published for Linux and Windows. The project is built using Google's [Bazel][bazel] build system
in combination with their first-party [Gazelle][gazelle] `BUILD` file generator.

## 📖 Overview

Years of bookmarks in a folder tree nobody can navigate? [`bookscape`][bookscape] reads the export your browser
already produces, collapses the duplicates, checks what is still reachable, classifies the rest into a small
flat set of collections, and emits a document for import into Linkwarden - it reads one format and converts
between none, so it is a filter rather than a converter. Tens of thousands of photographs
in one directory from a phone backup? [`sicht`][sicht] sorts them by sifting: EXIF metadata and deterministic
rules first, a local vision model only for what the rules cannot decide, a review pass for what the model was
unsure about, and only then any file movement - recorded in a manifest that can be undone. Maintaining Terraform
modules that are meant to cover a provider's whole API? [`formr`][formr] lists every field of every provider
resource and data source a module uses, says what the module does with each, and fails CI when that report goes
stale.

Neither archive tool sends your archive anywhere. `bookscape` talks to the open web to find out whether a bookmarked page
still answers, and to a cloud model only if you configure one; `sicht` runs its inference against a locally
installed [Ollama][ollama] and is wired to nothing else, because an image archive holds other people's faces and
documents as well as your own.

## ✨ TL;DR

```shell
# Install one application - bookscape, sicht or formr - for linux or windows, amd64
curl -fsSL https://github.com/adnoctem/pertry/releases/latest/download/sicht_linux_amd64.tar.gz \
  | sudo tar -xzf - -C /usr/local/bin sicht

# Or all of them at once
curl -fsSL https://github.com/adnoctem/pertry/releases/latest/download/pertry_linux_amd64.tar.gz \
  | sudo tar -xzf - -C /usr/local/bin bookscape sicht formr

# Then check what you got
sicht version
```

Prefer to check what you install first? Every release publishes a checksum file covering every tarball:

```shell
base=https://github.com/adnoctem/pertry/releases/latest/download
curl -fsSLO "$base/sicht_linux_amd64.tar.gz"
curl -fsSL "$base/CHECKSUMS_SHA256.txt" | sha256sum -c --ignore-missing
tar -xzf sicht_linux_amd64.tar.gz sicht
```

Linux and Windows are both supported, on `amd64`. The Windows build is not an afterthought: it is why this
repository bans cgo outright, and the cross-compile for it is checked on every change. `sicht` additionally
needs [exiftool][exiftool] and a running [Ollama][ollama] - run `sicht doctor`, which reports on every one of
its dependencies and says what to do about each. `formr` needs a `terraform` or `tofu` executable.

## 🛠️ Applications

Like most modern [Go][go] projects the executables are located within the [cmd][cmd] directory, each with its
own README documenting how to install, configure and run it. Here's a quick-reference list as an overview:

- [`bookscape`][bookscape]: a bookmark archival and enrichment pipeline that emits for Linkwarden
- [`sicht`][sicht]: a local-first iterative image sorter, with a vision model for what rules cannot decide
- [`formr`][formr]: a provider-schema coverage report and CI check for Terraform modules

All are pre-release. `bookscape` covers its whole pipeline; `sicht` scans, applies rules, classifies, reviews
and moves files from the command line, and has no web interface yet; `formr` generates and checks reports, and has
no exception annotations yet.

How each one works, what they share and where their limits are is described in the [architecture][design].

### 🔃 Contributing

Refer to our [documentation for contributors][contributing] for building and testing, the repository layout,
dependency rules, commit message formats and how a release is made.

### 📥 Maintainers

This project is owned and maintained by [Ad Noctem Collective][org] refer to the [`AUTHORS`][authors] or
[`CODEOWNERS`][owners] for more information. You may also use the linked contact details to reach out directly.

### ©️ Copyright

- _Assets created by:_ **[Ad Noctem Collective][org]**
- _Sources provided by:_ **[Ad Noctem Collective][org]** under the **[MIT License][license]**

<!-- INTERNAL REFERENCES -->

<!-- Project references -->

[cmd]: cmd
[bookscape]: cmd/bookscape
[sicht]: cmd/sicht
[formr]: cmd/formr

<!-- File references -->

[license]: LICENSE
[contributing]: docs/CONTRIBUTING.md
[design]: docs/ARCHITECTURE.md
[authors]: .github/AUTHORS
[owners]: .github/CODEOWNERS
[ci_testing_workflow]: https://github.com/adnoctem/pertry/actions/workflows/testing.yaml

<!-- General links -->

[org]: https://github.com/adnoctem
[go]: https://go.dev
[bazel]: https://bazel.build
[gazelle]: https://github.com/bazelbuild/bazel-gazelle
[github_releases]: https://github.com/adnoctem/pertry/releases
[github_commits]: https://github.com/adnoctem/pertry/commits/main/

<!-- Third-party -->

[ollama]: https://ollama.com
[exiftool]: https://exiftool.org
[renovate]: https://renovatebot.com/
[precommit]: https://pre-commit.com/
