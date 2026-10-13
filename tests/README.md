# tests

End-to-end tests that need something the unit tests deliberately do not: a container runtime, a
third-party service, a real binary on `PATH`.

Nothing here runs by default. `go test ./...`, `bazel test //...` and both cross-compiles stay
green and Docker-free, and `tests/` is excluded from the Bazel graph entirely
(`# gazelle:exclude tests` in the root `BUILD.bazel`) — a Docker-dependent test has no business in
a hermetic build, and the sandbox would block the socket anyway.

## Build tags

| Tag           | Means                                                            | Used by                                 |
| ------------- | ---------------------------------------------------------------- | --------------------------------------- |
| `e2e`         | Needs a Docker daemon; brings containers up and tears them down. | `tests/e2e/`                            |
| `integration` | Needs a local binary or service already installed and running.   | reserved for `sicht` (exiftool, Ollama) |

Both are linted — `.golangci.yml` lists them under `run.build-tags` — so tagged code does not
quietly rot outside the linter's view.

## `tests/e2e/linkwarden`

Proves that a document emitted by `bookscape export` imports correctly into a real
[Linkwarden](https://linkwarden.app/).

A document can be well-formed and still fail downstream without an error: Linkwarden's HTML
importer discards `<DD>` descriptions, and its backup importer reports success after a failed
transaction. No unit test can catch either, because the other half of the round trip is a
different program. The suite therefore exports through the real CLI in both formats, imports each
into its own account on a live instance, and reads the account back, asserting on what landed.
[docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md#export-and-linkwarden) describes the importers'
behaviour.

```bash
go test -tags e2e -timeout 20m ./tests/e2e/linkwarden/...
```

The non-default timeout is required: three containers have to start and the Linkwarden image runs
its Prisma migrations during boot, which does not fit Go's 10-minute default. A cold run pulls
roughly 1.5 GB of images.

The test starts and removes its own stack, generates every credential per run from `crypto/rand`,
and publishes a random loopback port rather than 3000 — so it will not collide with a Linkwarden
you run yourself, and there is no secret to configure anywhere.

### Iterating locally

Bringing the stack up for every run is slow. Start one by hand and let the test adopt it:

```bash
cd tests/e2e/linkwarden
LINKWARDEN_PORT=3000 \
  POSTGRES_PASSWORD=$(openssl rand -hex 16) \
  NEXTAUTH_SECRET=$(openssl rand -hex 32) \
  MEILI_MASTER_KEY=$(openssl rand -hex 32) \
  docker compose up -d --wait

cd -
BOOKSCAPE_E2E_LINKWARDEN_URL=http://127.0.0.1:3000 \
  go test -tags e2e -timeout 20m ./tests/e2e/linkwarden/...
```

With `BOOKSCAPE_E2E_LINKWARDEN_URL` set the test manages no containers and registers no cleanup.
It reuses the account from the previous run, so repeated runs are additive — if an assertion about
link counts starts failing, that is why; recreate the stack with `down -v`.

### Cleaning up after a killed run

The test removes its stack via `t.Cleanup`, which a `SIGKILL` or a runner cancellation can cut
short. Every project is named `bookscape-e2e-<pid>-<random>`, so leftovers are easy to find:

```bash
docker ps --all --filter 'name=bookscape-e2e-'

for p in $(docker ps --all --filter 'name=bookscape-e2e-' \
    --format '{{.Label "com.docker.compose.project"}}' | sort -u); do
  docker compose --project-name "$p" down --volumes --remove-orphans
done
```

### If `ghcr.io` denies the pull

A stale credential in your Docker credential store beats anonymous access: the daemon offers the
token, GHCR rejects it, and you get a bare `denied` even though the image is public. Check with
`docker logout ghcr.io`, or re-login with a token carrying `read:packages`.

### Images

| Image                                      | Why pinned there                                                                                         |
| ------------------------------------------ | -------------------------------------------------------------------------------------------------------- |
| `ghcr.io/linkwarden/linkwarden:v2.16.3`    | The version whose `importFromHTMLFile.ts` was read to write the assertions. `:latest` would change them. |
| `mirror.gcr.io/library/postgres:16-alpine` | Pulled through a mirror: anonymous Docker Hub pulls are rate limited per IP, which breaks on CI runners. |
| `mirror.gcr.io/getmeili/meilisearch:…`     | Same reason. Meilisearch is required — Linkwarden's `depends_on` lists it.                               |

### In CI

`.github/workflows/e2e.yaml` runs nightly, on `workflow_dispatch`, and on a pull request **only**
when the `e2e` label is applied. It is not on every PR: a docs-only or `sicht`-only change cannot
break it, and the run costs 4–6 minutes of runner time.
