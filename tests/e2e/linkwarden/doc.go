// Package linkwarden holds the end-to-end test that proves a document emitted by `bookscape
// export` imports correctly into a real Linkwarden instance.
//
// docs/ARCHITECTURE.md#process-model names hand-rolled Netscape HTML emission as "the single most likely
// cause of a silent import failure downstream", and docs/ARCHITECTURE.md#parsing records the specific reason: Linkwarden
// reads a bookmark's description from a <DD> parsed as a child of the <DT>, which is an assumption
// about two independent parsers agreeing on the same lenient HTML. When it is wrong the
// description simply arrives empty and nothing reports an error. That is what this test exists to
// catch, along with the headline property the whole project rests on — that the original ADD_DATE
// survives the round trip.
//
// # Running it
//
// The test is behind the `e2e` build tag and is skipped by default. It needs a Docker daemon and
// pulls roughly 1.5 GB across three images on a cold cache:
//
//	go test -tags e2e -timeout 20m ./tests/e2e/linkwarden/...
//
// The non-default timeout is required: the stack has to boot Postgres, Meilisearch and a Next.js
// application that runs Prisma migrations on startup, which does not fit Go's 10-minute default.
//
// Set BOOKSCAPE_E2E_LINKWARDEN_URL to point at an already-running instance and the test will adopt
// it instead of starting its own, registering no cleanup. That makes local iteration one
// `docker compose up -d` followed by as many fast runs as you like.
//
// # Conventions
//
// The `e2e` tag is deliberately distinct from `integration`, which docs/CONTRIBUTING.md
// reserves for tests needing a locally installed binary such as exiftool or a running Ollama.
// Someone exercising that path should not suddenly find themselves pulling Postgres.
//
// This package is not parallel-safe: there is one stack per `go test` invocation and no test here
// calls t.Parallel.
//
// This file carries no build tag on purpose. A package whose every file is excluded by a
// constraint makes `go build ./...` fail with "build constraints exclude all Go files", which
// would break both the go-build-repo-mod pre-commit hook and the `go vet ./...` step in CI.
package linkwarden
