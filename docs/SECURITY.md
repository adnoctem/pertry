# pertry Security Policy

## Reporting a Vulnerability

Report suspected vulnerabilities privately to [info+pertry@adnoctem.co][report_contact]. Do not open a public issue
with exploit details, credentials or private data before maintainers have had an opportunity to assess the report.

Useful reports include:

- The affected application and version (`<app> version`), the operating system, and whether it runs natively or
  under WSL.
- For `sicht`, the Ollama version and whether `OLLAMA_HOST` points at this machine; for `bookscape`, the configured
  classification provider.
- A minimal reproduction using synthetic data, the expected behaviour and the observed behaviour.
- The likely impact and the access required to trigger it.
- A contact address for follow-up and your disclosure or acknowledgement preferences.

Remove API keys, bookmark lists, image file names and unrelated personal information from examples and logs. A
`--log-level debug` log names every file or URL it touches. If a credential has already been exposed, revoke or
rotate it at the provider that issued it; deleting a file or log does not invalidate it.

Maintainers will assess the report, discuss reproduction or mitigation as needed, and coordinate a fix and public
disclosure when appropriate. Please coordinate publication while the issue is being addressed. This project is
maintained on a best-effort basis and does not promise a fixed response or remediation deadline.

If a problem originates in a dependency, Ollama, exiftool, Linkwarden or a model provider, report it to that project
as well, and let pertry's maintainers know when a default or a documented setup is affected.

## Versions and Fixes

pertry is in pre-1.0 development. Fixes target the current development line and are published in a new release;
earlier releases do not receive backports. Minor releases may contain breaking changes — read the release notes
before upgrading.

Pin a release version for reproducible installs and update deliberately to receive fixes. `releases/latest`
download URLs always move to the newest release.

## Verifying Releases

Every release publishes `CHECKSUMS_SHA256.txt` covering its tarballs, served over HTTPS from GitHub. Verify a
download against it before installing; the [README][readme] shows how. Checksums detect corruption and tampering in
transit; they do not authenticate the publisher independently of GitHub. Release tarballs are not signed.

## Trust and Privilege Boundaries

Both applications are personal tools that run on the machine holding the data, as the user who owns it. Neither
runs a network listener, and neither elevates privileges.

- **sicht and file moves.** `sicht apply` moves files within the workspace according to a plan. Targets are built
  from facet values, which a model can produce, so every path segment is sanitised and absolute paths, `..`
  segments and drive letters are refused before any file moves. `apply` writes nothing without `--execute`, records
  every move in a manifest and never deletes a file.
- **sicht and inference.** sicht sends images only to the Ollama server at `OLLAMA_HOST`. `sicht doctor` warns when
  that host is not loopback, both because Ollama has no authentication on its model-management endpoints and
  because inference would no longer be local. Run Ollama bound to loopback unless it is deliberately shared.
- **sicht and exiftool.** sicht runs the `exiftool` found on `PATH` over every scanned file. Use a trusted
  installation; a crafted image is parsed by exiftool, not by sicht.
- **bookscape and outbound requests.** `bookscape check` sends requests to every bookmarked host, with TLS
  certificate verification always enabled. Those hosts and the network path learn which links are being checked.
  The model pass sends URLs, titles and folder paths to the configured provider; with `classify.provider: ollama`
  nothing leaves the machine.
- **bookscape and Linkwarden.** bookscape writes a file for import and does not connect to Linkwarden. Review an
  export before importing it, since an import cannot be undone in bulk.
- **Planned web interface.** sicht has no web interface yet. When it exists it is to bind to loopback only, validate
  the `Host` header, reject cross-origin state-changing requests and send a restrictive Content Security Policy;
  it will not use a session token, so those four controls are what protect it from web pages in the user's own
  browser.

## Secrets and Diagnostics

- `bookscape`'s provider API keys come from the environment or the configuration file and are never accepted as
  command-line flags. Protect the configuration file accordingly.
- sicht has no credentials. Its logs omit OCR text and stage payloads below `debug` level; debug logs stay in the
  workspace's `.sicht/logs/`.
- Both databases are unencrypted SQLite files holding personal data — a bookmark history or an image archive's
  metadata and model descriptions. Protect them like the source data.
- Secret scanning runs on every commit but does not guarantee that every sensitive value is detected.

## Development Environment

Tests use synthetic fixtures. Integration tests that read a real archive take its path from `SICHT_TEST_ARCHIVE` and
skip without it. The end-to-end suite starts a Linkwarden stack through the local Docker engine with credentials
generated per run, and binds a local port; run it on a trusted machine. The scripts source code from `LIBSH_DIR`;
use a trusted libsh installation of a pinned release.

<!-- File references -->

[readme]: ../README.md

<!-- Contact links -->

[report_contact]: mailto:info+pertry@adnoctem.co
