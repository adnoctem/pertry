//go:build e2e

package linkwarden

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// AdoptEnv names an already-running Linkwarden. When it is set the test talks to that instance
// instead of starting its own and registers no cleanup, which turns local iteration into one
// `docker compose up -d` followed by as many fast runs as you like.
const AdoptEnv = "BOOKSCAPE_E2E_LINKWARDEN_URL"

// readyTimeout bounds the wait for the stack to serve requests. Three containers have to start,
// and the Linkwarden image runs its Prisma migrations during boot, so this is minutes rather than
// seconds on a cold cache.
const readyTimeout = 5 * time.Minute

// Stack is a Linkwarden deployment the test can talk to.
type Stack struct {
	// BaseURL is the loopback origin, e.g. http://127.0.0.1:34567.
	BaseURL string

	// project is the compose project name, empty when the stack was adopted rather than
	// started. A unique name per run means a stack leaked by a SIGKILLed test is identifiable
	// and that `down -v` removes only this run's volumes.
	project string

	// env carries the generated credentials and the published port through to every compose
	// invocation.
	env []string
}

// Start brings the stack up and waits until Linkwarden is serving and its schema is migrated.
func Start(t *testing.T) *Stack {
	t.Helper()

	if adopted := strings.TrimSpace(os.Getenv(AdoptEnv)); adopted != "" {
		t.Logf("adopting the Linkwarden instance at %s (%s is set); this run will not manage containers",
			adopted, AdoptEnv)

		return &Stack{BaseURL: strings.TrimSuffix(adopted, "/")}
	}

	requireDocker(t)

	port := freePort(t)

	s := &Stack{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		project: fmt.Sprintf("bookscape-e2e-%d-%s", os.Getpid(), secret(t, 4)),
		env: []string{
			"LINKWARDEN_PORT=" + strconv.Itoa(port),
			// Generated per run rather than written into compose.yaml: nothing for
			// gitleaks to find, no throwaway password in the repository, and one run
			// cannot authenticate against another's leftovers.
			"POSTGRES_PASSWORD=" + secret(t, 16),
			"NEXTAUTH_SECRET=" + secret(t, 32),
			"MEILI_MASTER_KEY=" + secret(t, 32),
		},
	}

	t.Cleanup(func() { s.down(t) })

	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()

	t.Logf("starting Linkwarden stack (project %s) on %s", s.project, s.BaseURL)

	// --wait blocks until every service with a healthcheck reports healthy, which covers
	// Postgres and Meilisearch. Linkwarden deliberately has none - "the port is open" and "the
	// schema exists" are different questions - so waitReady handles it below.
	if out, err := s.compose(ctx, "up", "--detach", "--wait", "--wait-timeout", "240"); err != nil {
		t.Fatalf("docker compose up failed: %v\n%s\n%s", err, out, s.diagnostics(ctx))
	}

	if err := s.waitReady(ctx); err != nil {
		t.Fatalf("Linkwarden never became ready: %v\n%s", err, s.diagnostics(ctx))
	}

	return s
}

// requireDocker skips rather than fails when there is no daemon, so that `go test -tags e2e ./...`
// on a machine without Docker reports honestly instead of looking broken.
func requireDocker(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Skipf("no usable Docker daemon, skipping: %v\n%s", err, out)
	}
}

// compose runs one docker compose command against this stack's project.
func (s *Stack) compose(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"compose", "--project-name", s.project, "--file", "compose.yaml"}, args...)

	cmd := exec.CommandContext(ctx, "docker", full...)
	// The compose file interpolates LINKWARDEN_PORT and the three secrets, and docker itself
	// needs the ambient environment (PATH, DOCKER_HOST, HOME).
	cmd.Env = append(os.Environ(), s.env...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w", strings.Join(full, " "), err)
	}

	return string(out), nil
}

// waitReady polls until Linkwarden both serves requests and has a migrated schema.
//
// Two phases, because they fail differently. /api/v1/auth/csrf is a NextAuth route that touches no
// table, so it isolates "Next.js is up" from "Prisma has finished". The second phase is the
// caller's first real request: a synthetic probe that passes while the first real call fails is
// worse than no probe at all, so readiness is proven by registration succeeding rather than
// guessed at here. See Client.Register.
func (s *Stack) waitReady(ctx context.Context) error {
	client := &http.Client{Timeout: 10 * time.Second}
	url := s.BaseURL + "/api/v1/auth/csrf"

	var last string

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("gave up waiting for %s (last: %s): %w", url, last, err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build readiness request: %w", err)
		}

		resp, err := client.Do(req)
		switch {
		case err != nil:
			last = err.Error()
		case resp.StatusCode == http.StatusOK:
			_ = resp.Body.Close()

			return nil
		default:
			last = "HTTP " + resp.Status
			_ = resp.Body.Close()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for %s (last: %s): %w", url, last, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// diagnostics returns the container state and recent logs, for inlining into a failure message.
// A CI failure has to be diagnosable from the log alone, without re-running a five-minute test.
func (s *Stack) diagnostics(ctx context.Context) string {
	if s.project == "" {
		return "(adopted instance; no container diagnostics available)"
	}

	// A fresh context: the one that timed out cannot run anything.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()

	var b strings.Builder

	b.WriteString("\n--- docker compose ps ---\n")

	ps, _ := s.compose(ctx, "ps", "--all")
	b.WriteString(ps)

	b.WriteString("\n--- docker compose logs (tail) ---\n")

	logs, _ := s.compose(ctx, "logs", "--no-color", "--tail", "200")
	b.WriteString(logs)

	return b.String()
}

// down removes the stack and its volumes.
func (s *Stack) down(t *testing.T) {
	t.Helper()

	if s.project == "" {
		return
	}

	// Not the test's context: teardown has to run even when the test failed on a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if out, err := s.compose(ctx, "down", "--volumes", "--remove-orphans", "--timeout", "10"); err != nil {
		// A failure here leaks containers, so say exactly how to find them rather than
		// failing the test a second time for a cleanup problem.
		t.Errorf("docker compose down failed, so this run may have leaked containers: %v\n%s\n"+
			"clean up with: docker compose --project-name %s down --volumes --remove-orphans",
			err, out, s.project)
	}
}

// freePort reserves a loopback port and returns it.
//
// Never 3000: a developer running their own Linkwarden should not have this test collide with it.
// The reservation is advisory - the listener is closed so compose can bind the port, which leaves
// a window - but it is the only way to know the port before compose starts, and NEXTAUTH_URL has
// to carry the same number or NextAuth rejects the request host. A lost race surfaces as a compose
// bind error naming the port, which is self-diagnosing.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}

	port := l.Addr().(*net.TCPAddr).Port

	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	return port
}

// secret returns n cryptographically random bytes, hex encoded.
func secret(t *testing.T, n int) string {
	t.Helper()

	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate %d random bytes: %v", n, err)
	}

	return hex.EncodeToString(b)
}
