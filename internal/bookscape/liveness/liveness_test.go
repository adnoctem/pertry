package liveness_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/liveness"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// recorder is an in-memory Recorder. Every test here runs against a local test server, so the
// store is the only other dependency and there is no reason to involve SQLite.
type recorder struct {
	mu      sync.Mutex
	results map[int64]store.Liveness
	err     error
}

func newRecorder() *recorder {
	return &recorder{results: make(map[int64]store.Liveness)}
}

func (r *recorder) RecordLiveness(_ context.Context, id int64, result store.Liveness) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return r.err
	}

	r.results[id] = result

	return nil
}

func (r *recorder) get(id int64) store.Liveness {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.results[id]
}

func (r *recorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.results)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() config.Check {
	cfg := config.Default().Check
	// Keep the suite fast; the production defaults are a budget for several thousand real
	// links, not for a loopback server.
	cfg.Timeout = 2 * time.Second
	cfg.Concurrency = 4
	cfg.PerHostConcurrency = 2

	return cfg
}

// TestRunClassifiesResponses covers the status table in docs/ARCHITECTURE.md#liveness.
func TestRunClassifiesResponses(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/created":
			w.WriteHeader(http.StatusCreated)
		case "/gone":
			w.WriteHeader(http.StatusGone)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "/teapot":
			w.WriteHeader(http.StatusTeapot)
		case "/boom":
			w.WriteHeader(http.StatusInternalServerError)
		case "/ratelimited":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tests := []struct {
		id   int64
		path string
		want store.Status
	}{
		{id: 1, path: "/ok", want: store.StatusAlive},
		{id: 2, path: "/created", want: store.StatusAlive},
		{id: 3, path: "/gone", want: store.StatusDead},
		{id: 4, path: "/missing", want: store.StatusDead},
		{id: 5, path: "/forbidden", want: store.StatusUncertain},
		{id: 6, path: "/boom", want: store.StatusUncertain},
		{id: 7, path: "/ratelimited", want: store.StatusUncertain},
		// Not named in the table. Uncertain is the non-destructive reading: it keeps the
		// link in the export rather than dropping it on a status nobody classified.
		{id: 8, path: "/teapot", want: store.StatusUncertain},
	}

	rec := newRecorder()
	targets := make([]liveness.Target, 0, len(tests))

	for _, tc := range tests {
		targets = append(targets, liveness.Target{ID: tc.id, URL: srv.URL + tc.path})
	}

	stats, err := liveness.New(testConfig(), rec, discardLogger()).Run(context.Background(), targets)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	for _, tc := range tests {
		if got := rec.get(tc.id).Status; got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.path, got, tc.want)
		}
	}

	if stats.Total() != len(tests) {
		t.Errorf("Stats.Total() = %d, want %d", stats.Total(), len(tests))
	}

	if stats.Alive != 2 || stats.Dead != 2 || stats.Uncertain != 4 {
		t.Errorf("Stats = %+v, want alive=2 dead=2 uncertain=4", stats)
	}
}

// TestRunRetriesUncertainWithGET is why HEAD-first is safe: a server that rejects HEAD but
// serves GET must come back alive, not uncertain.
func TestRunRetriesUncertainWithGET(t *testing.T) {
	t.Parallel()

	var heads, gets atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		gets.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := newRecorder()

	if _, err := liveness.New(testConfig(), rec, discardLogger()).Run(
		context.Background(), []liveness.Target{{ID: 1, URL: srv.URL + "/head-rejected"}}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	got := rec.get(1)

	if got.Status != store.StatusAlive {
		t.Errorf("status = %q, want %q after the GET retry", got.Status, store.StatusAlive)
	}

	if got.Method != http.MethodGet {
		t.Errorf("method = %q, want %q; the recorded result should be the one that decided it",
			got.Method, http.MethodGet)
	}

	if heads.Load() != 1 || gets.Load() != 1 {
		t.Errorf("requests = %d HEAD, %d GET; want exactly one of each", heads.Load(), gets.Load())
	}
}

func TestRunWithoutGETRetry(t *testing.T) {
	t.Parallel()

	var gets atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		}

		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.RetryUncertainWithGET = false

	rec := newRecorder()

	if _, err := liveness.New(cfg, rec, discardLogger()).Run(
		context.Background(), []liveness.Target{{ID: 1, URL: srv.URL + "/x"}}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got := rec.get(1).Status; got != store.StatusUncertain {
		t.Errorf("status = %q, want %q", got, store.StatusUncertain)
	}

	if gets.Load() != 0 {
		t.Errorf("GET requests = %d, want 0 when retry_uncertain_with_get is off", gets.Load())
	}
}

// TestRunRecordsTheRedirectTarget matters because a large number of links collapsing to the
// same destination - a parked domain, a generic 404 served with a 200 - is itself a signal.
func TestRunRecordsTheRedirectTarget(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := newRecorder()

	if _, err := liveness.New(testConfig(), rec, discardLogger()).Run(
		context.Background(), []liveness.Target{{ID: 1, URL: srv.URL + "/old"}}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	got := rec.get(1)

	if got.Status != store.StatusAlive {
		t.Errorf("status = %q, want %q for a 301 resolving to 200", got.Status, store.StatusAlive)
	}

	if want := srv.URL + "/new"; got.FinalURL != want {
		t.Errorf("FinalURL = %q, want %q", got.FinalURL, want)
	}
}

// TestRunStopsAtTheRedirectCap guards against a redirect loop consuming the whole budget.
func TestRunStopsAtTheRedirectCap(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An endless chain.
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRedirects = 3

	rec := newRecorder()

	if _, err := liveness.New(cfg, rec, discardLogger()).Run(
		context.Background(), []liveness.Target{{ID: 1, URL: srv.URL + "/loop"}}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	got := rec.get(1)

	if got.Status != store.StatusUncertain {
		t.Errorf("status = %q, want %q for a redirect loop", got.Status, store.StatusUncertain)
	}

	if got.Error == "" {
		t.Error("Error is empty, want the redirect-cap message")
	}
}

func TestRunWithoutFollowingRedirects(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.FollowRedirects = false

	rec := newRecorder()

	if _, err := liveness.New(cfg, rec, discardLogger()).Run(
		context.Background(), []liveness.Target{{ID: 1, URL: srv.URL + "/old"}}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	got := rec.get(1)

	// The server responded and pointed somewhere, so the link resolves; we just did not
	// chase it.
	if got.Status != store.StatusAlive {
		t.Errorf("status = %q, want %q for an unfollowed 3xx", got.Status, store.StatusAlive)
	}

	if got.HTTPStatus != http.StatusMovedPermanently {
		t.Errorf("HTTPStatus = %d, want %d", got.HTTPStatus, http.StatusMovedPermanently)
	}
}

// TestRunSkipsUnfetchableURLs covers the junk a real bookmark collection contains. These are
// skipped rather than uncertain: there is nothing to be uncertain about.
func TestRunSkipsUnfetchableURLs(t *testing.T) {
	t.Parallel()

	targets := []liveness.Target{
		{ID: 1, URL: "javascript:void(0)"},
		{ID: 2, URL: "place:type=6&sort=14"},
		{ID: 3, URL: "file:///home/user/notes.html"},
		{ID: 4, URL: "chrome://bookmarks/"},
		{ID: 5, URL: "/relative/path"},
		{ID: 6, URL: "https://"},
	}

	rec := newRecorder()

	stats, err := liveness.New(testConfig(), rec, discardLogger()).Run(context.Background(), targets)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if stats.Skipped != len(targets) {
		t.Errorf("Stats.Skipped = %d, want %d", stats.Skipped, len(targets))
	}

	for _, target := range targets {
		got := rec.get(target.ID)

		if got.Status != store.StatusSkipped {
			t.Errorf("%s: status = %q, want %q", target.URL, got.Status, store.StatusSkipped)
		}

		if got.Error == "" {
			t.Errorf("%s: Error is empty, want a reason", target.URL)
		}
	}
}

// TestRunClassifiesADeadHost covers the "DNS failure" row of the table. .invalid is reserved
// by RFC 2606 and guaranteed never to resolve, so this needs no network.
func TestRunClassifiesADeadHost(t *testing.T) {
	t.Parallel()

	rec := newRecorder()

	if _, err := liveness.New(testConfig(), rec, discardLogger()).Run(
		context.Background(),
		[]liveness.Target{{ID: 1, URL: "https://this-host-does-not-exist.invalid/page"}}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	// A resolver that answers NXDOMAIN gives dead; one that times out or is unreachable
	// gives uncertain. Both are correct readings of what happened, and a test must not
	// depend on which the build environment produces.
	switch got := rec.get(1).Status; got {
	case store.StatusDead, store.StatusUncertain:
	default:
		t.Errorf("status = %q, want dead or uncertain for a host that cannot resolve", got)
	}
}

// TestRunRespectsPerHostConcurrency is the rate-limiting requirement: a bookmark collection is
// heavily skewed toward a handful of domains, so a global cap alone would point every worker at
// one host.
func TestRunRespectsPerHostConcurrency(t *testing.T) {
	t.Parallel()

	var (
		inFlight atomic.Int64
		peak     atomic.Int64
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		for {
			high := peak.Load()
			if now <= high || peak.CompareAndSwap(high, now) {
				break
			}
		}

		// Long enough that requests genuinely overlap if the limiter lets them.
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Concurrency = 16
	cfg.PerHostConcurrency = 2

	targets := make([]liveness.Target, 0, 12)
	for i := range 12 {
		targets = append(targets, liveness.Target{ID: int64(i + 1), URL: srv.URL + "/page"})
	}

	rec := newRecorder()

	if _, err := liveness.New(cfg, rec, discardLogger()).Run(context.Background(), targets); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got := peak.Load(); got > int64(cfg.PerHostConcurrency) {
		t.Errorf("peak concurrent requests to one host = %d, want at most %d",
			got, cfg.PerHostConcurrency)
	}

	if rec.len() != len(targets) {
		t.Errorf("recorded results = %d, want %d", rec.len(), len(targets))
	}
}

// TestRunSendsTheConfiguredUserAgent matters because a plausible one clears a meaningful amount
// of bot protection that would otherwise report a live page as uncertain.
func TestRunSendsTheConfiguredUserAgent(t *testing.T) {
	t.Parallel()

	var seen atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.UserAgent())
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.UserAgent = "bookscape-test/1.0"

	if _, err := liveness.New(cfg, newRecorder(), discardLogger()).Run(
		context.Background(), []liveness.Target{{ID: 1, URL: srv.URL + "/x"}}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got := seen.Load(); got != cfg.UserAgent {
		t.Errorf("User-Agent = %v, want %q", got, cfg.UserAgent)
	}
}

func TestRunTimesOut(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Timeout = 50 * time.Millisecond
	// Without this the GET retry doubles the wall-clock cost of the test for no gain.
	cfg.RetryUncertainWithGET = false

	rec := newRecorder()

	if _, err := liveness.New(cfg, rec, discardLogger()).Run(
		context.Background(), []liveness.Target{{ID: 1, URL: srv.URL + "/slow"}}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	// A timeout says nothing about whether the page exists.
	if got := rec.get(1).Status; got != store.StatusUncertain {
		t.Errorf("status = %q, want %q for a timeout", got, store.StatusUncertain)
	}
}

// TestRunPersistsResultsBeforeCancellation is the resumability property: an interrupted pass
// keeps what it finished rather than discarding the whole run.
func TestRunPersistsResultsBeforeCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var served atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served.Add(1) == 3 {
			cancel()
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Concurrency = 1
	cfg.PerHostConcurrency = 1

	targets := make([]liveness.Target, 0, 40)
	for i := range 40 {
		targets = append(targets, liveness.Target{ID: int64(i + 1), URL: srv.URL + "/page"})
	}

	rec := newRecorder()

	stats, err := liveness.New(cfg, rec, discardLogger()).Run(ctx, targets)
	if err == nil {
		t.Fatal("Run() error = nil, want a cancellation error")
	}

	if rec.len() == 0 {
		t.Error("no results were recorded, want the ones that completed before cancellation")
	}

	if rec.len() >= len(targets) {
		t.Errorf("recorded results = %d, want fewer than %d; the run was not actually cut short",
			rec.len(), len(targets))
	}

	if stats.Total() != rec.len() {
		t.Errorf("Stats.Total() = %d, want %d to match what was recorded", stats.Total(), rec.len())
	}
}
