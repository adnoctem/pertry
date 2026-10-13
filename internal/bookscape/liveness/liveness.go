// Package liveness checks whether bookmarked URLs still resolve.
//
// The result is deliberately three-state. A decade-old collection contains plenty of links
// that are neither clearly alive nor clearly gone: bot protection, rate limits, servers that
// reject HEAD outright, hosts that are down today and up tomorrow. Collapsing those into
// "dead" would quietly discard links the export filter should still ship, so they get their own
// bucket and `export` decides what to do with them (docs/ARCHITECTURE.md#liveness).
package liveness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// Target is one bookmark to probe.
type Target struct {
	ID  int64
	URL string
}

// Recorder persists one result. The checker writes per result rather than batching at the end:
// a liveness pass over several thousand links takes long enough that a crash or a Ctrl-C part
// way through must not discard the work already done.
type Recorder interface {
	RecordLiveness(ctx context.Context, bookmarkID int64, result store.Liveness) error
}

// Checker probes targets with bounded concurrency.
type Checker struct {
	cfg    config.Check
	client *http.Client
	store  Recorder
	log    *slog.Logger

	hosts *hostLimiter
}

// New builds a Checker.
func New(cfg config.Check, recorder Recorder, log *slog.Logger) *Checker {
	return &Checker{
		cfg:    cfg,
		client: newClient(cfg),
		store:  recorder,
		log:    log,
		hosts:  newHostLimiter(cfg.PerHostConcurrency),
	}
}

// newClient builds the HTTP client. A hand-configured transport rather than http.DefaultClient:
// the timeout, the redirect cap and the connection limits are all part of the check's budget.
func newClient(cfg config.Check) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = cfg.Concurrency * 2
	transport.MaxIdleConnsPerHost = cfg.PerHostConcurrency
	transport.MaxConnsPerHost = cfg.PerHostConcurrency
	// A bookmark check only needs the response status, never the body, so there is no reason
	// to negotiate compression and spend CPU on it.
	transport.DisableCompression = true

	return &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !cfg.FollowRedirects {
				// Hand the 3xx back rather than following it. The response is then
				// classified on its own terms, which is why probe treats an
				// unfollowed 3xx as alive.
				return http.ErrUseLastResponse
			}

			if len(via) > cfg.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", cfg.MaxRedirects)
			}

			return nil
		},
	}
}

// Stats counts results by status.
type Stats struct {
	Alive     int
	Dead      int
	Uncertain int
	Skipped   int
}

// Total is how many targets produced a result.
func (s Stats) Total() int {
	return s.Alive + s.Dead + s.Uncertain + s.Skipped
}

// Run probes every target, recording each result as it completes.
//
// A probe failure is a result, not an error: a dead link is the normal case, not an exception.
// Run returns an error only when something structural goes wrong - the database refuses a
// write, or the context is cancelled.
func (c *Checker) Run(ctx context.Context, targets []Target) (Stats, error) {
	var (
		mu    sync.Mutex
		stats Stats
	)

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(c.cfg.Concurrency)

	for _, t := range targets {
		g.Go(func() error {
			result := c.check(ctx, t)

			// Cancellation is not a result. Recording "uncertain" for every
			// remaining link after a Ctrl-C would overwrite good data from an earlier
			// run with noise from an interrupted one.
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if err := c.store.RecordLiveness(ctx, t.ID, result); err != nil {
				return err
			}

			mu.Lock()
			defer mu.Unlock()

			switch result.Status {
			case store.StatusAlive:
				stats.Alive++
			case store.StatusDead:
				stats.Dead++
			case store.StatusUncertain:
				stats.Uncertain++
			case store.StatusSkipped:
				stats.Skipped++
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return stats, fmt.Errorf("liveness pass: %w", err)
	}

	return stats, nil
}

// check probes one target, retrying an uncertain HEAD with a GET if configured.
func (c *Checker) check(ctx context.Context, t Target) store.Liveness {
	host, err := hostOf(t.URL)
	if err != nil {
		// Not a probe failure: this URL can never be fetched, so there is nothing to be
		// uncertain about.
		return store.Liveness{Status: store.StatusSkipped, Error: err.Error()}
	}

	release, err := c.hosts.acquire(ctx, host)
	if err != nil {
		return store.Liveness{Status: store.StatusSkipped, Error: err.Error()}
	}
	defer release()

	// HEAD first because it is cheap. A meaningful fraction of sites either reject it
	// outright or sit behind bot protection that only clears on a full request, which is
	// what the GET retry is for.
	result := c.probe(ctx, t.URL, http.MethodHead)

	if result.Status == store.StatusUncertain && c.cfg.RetryUncertainWithGET {
		c.log.Debug("retrying uncertain HEAD with GET",
			"url", t.URL, "http_status", result.HTTPStatus, "error", result.Error)

		if retry := c.probe(ctx, t.URL, http.MethodGet); retry.Status != store.StatusUncertain {
			return retry
		} else if retry.HTTPStatus != 0 {
			// Keep the GET's evidence even when it is also inconclusive: a 403 from a
			// full request is more informative than a 405 from a HEAD.
			return retry
		}
	}

	return result
}

// probe issues one request and classifies the outcome.
func (c *Checker) probe(ctx context.Context, rawURL, method string) store.Liveness {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return store.Liveness{Status: store.StatusSkipped, Method: method, Error: err.Error()}
	}

	req.Header.Set("User-Agent", c.cfg.UserAgent)
	// Ask for HTML so that content negotiation does not hand back something odd, and accept
	// anything as a fallback.
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")

	resp, err := c.client.Do(req)
	if err != nil {
		return store.Liveness{Status: classifyError(err), Method: method, Error: errorSummary(err)}
	}
	defer func() {
		// The body is never read, but it has to be closed for the connection to be
		// reused rather than leaked.
		_ = resp.Body.Close()
	}()

	result := store.Liveness{
		Status:     classifyStatus(resp.StatusCode),
		HTTPStatus: resp.StatusCode,
		Method:     method,
	}

	// Record where a redirect chain actually landed. A large number of these collapsing to
	// the same destination - a parked-domain page, a generic 404 served with a 200 - is
	// itself a useful signal, which is what `stats --redirect-targets` reports on.
	if final := resp.Request.URL.String(); final != rawURL {
		result.FinalURL = final
	}

	return result
}

// classifyStatus maps an HTTP status to a liveness state, per the table in
// docs/ARCHITECTURE.md#liveness.
func classifyStatus(code int) store.Status {
	switch {
	case code >= 200 && code < 300:
		return store.StatusAlive

	case code >= 300 && code < 400:
		// Only reachable with follow_redirects disabled, in which case the server did
		// respond and did point somewhere. Treating it as alive is the honest reading;
		// the table's "3xx resolving to 2xx" case is handled by the client following it.
		return store.StatusAlive

	case code == http.StatusNotFound, code == http.StatusGone:
		return store.StatusDead

	default:
		// Everything else is inconclusive, which covers the table's 401/403/405/429 and
		// 5xx rows and any other 4xx the table does not name. Erring toward uncertain
		// rather than dead keeps a link in the export by default, which is the
		// non-destructive direction.
		return store.StatusUncertain
	}
}

// classifyError maps a transport-level failure to a liveness state.
func classifyError(err error) store.Status {
	// A timeout says nothing about whether the page exists.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return store.StatusUncertain
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return store.StatusUncertain
	}

	// A host that does not resolve is gone. A temporary resolver failure is not.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return store.StatusDead
		}

		return store.StatusUncertain
	}

	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) {
		return store.StatusDead
	}

	return store.StatusUncertain
}

// errorSummary trims an error to something readable in a table. net/http errors carry the full
// request URL, which is already stored alongside the result.
func errorSummary(err error) string {
	msg := err.Error()

	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		msg = urlErr.Err.Error()
	}

	const limit = 200
	if len(msg) > limit {
		msg = msg[:limit]
	}

	return strings.TrimSpace(msg)
}

// hostOf extracts the host a URL will be fetched from, rejecting anything that cannot be
// fetched over HTTP at all.
func hostOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("unfetchable URL: %w", err)
	}

	switch u.Scheme {
	case "http", "https":
	case "":
		return "", errors.New("no URL scheme")
	default:
		// javascript:, file:, chrome:, place: and friends. Real bookmark collections
		// contain all of them.
		return "", fmt.Errorf("scheme %q is not fetchable over HTTP", u.Scheme)
	}

	if u.Host == "" {
		return "", errors.New("no host")
	}

	return strings.ToLower(u.Host), nil
}

// hostLimiter caps in-flight requests per host.
//
// A global concurrency cap is not enough. A bookmark collection is heavily skewed toward a
// handful of domains, so a cap of 32 would point all 32 at github.com - which is both rude and
// a fast route to a 429 that pollutes the results with uncertainty that says nothing about the
// links (docs/ARCHITECTURE.md#liveness).
type hostLimiter struct {
	limit int

	mu    sync.Mutex
	slots map[string]chan struct{}
}

func newHostLimiter(limit int) *hostLimiter {
	if limit < 1 {
		limit = 1
	}

	return &hostLimiter{limit: limit, slots: make(map[string]chan struct{})}
}

// acquire blocks until the host has a free slot, returning the function that releases it.
func (h *hostLimiter) acquire(ctx context.Context, host string) (func(), error) {
	h.mu.Lock()

	slot, ok := h.slots[host]
	if !ok {
		slot = make(chan struct{}, h.limit)
		h.slots[host] = slot
	}

	h.mu.Unlock()

	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
