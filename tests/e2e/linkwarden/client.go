//go:build e2e

package linkwarden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Client is an authenticated Linkwarden API session.
//
// Authentication is the NextAuth session cookie rather than a bearer token. Linkwarden resolves
// either - a bearer token wins when present, the cookie is the fallback - but its bearer tokens
// are JWE-encrypted NextAuth JWTs, so minting one would mean reimplementing NextAuth's crypto.
// Obtaining a cookie is three ordinary HTTP calls.
type Client struct {
	BaseURL string

	// Username is what the credentials provider authenticates against. Linkwarden looks a
	// user up by email as well as username only when an email provider is configured
	// (EMAIL_FROM and EMAIL_SERVER both set); with neither set, as here, the lookup is
	// `username` only - so signing in with the email address silently fails as "Invalid
	// credentials".
	Username string
	Email    string
	Password string

	// UserID is filled in after sign-in.
	UserID int64

	http *http.Client
}

// NewClient registers a user, signs in, and returns the authenticated session.
//
// Registration doubles as the readiness probe for the database. Linkwarden runs Prisma migrations
// while booting, and until they finish this endpoint fails; so it is retried until the handler
// demonstrably reached the schema. See Register.
func NewClient(ctx context.Context, t *testing.T, s *Stack) *Client {
	t.Helper()

	return NewClientFor(ctx, t, s, "")
}

// NewClientFor registers a distinctly named user on the same stack.
//
// A second account exists because importFromLinkwarden creates every collection unconditionally -
// it never looks for an existing one of the same name - so importing a second document into the
// first account would double every collection and make the read-back ambiguous. A fresh account
// is cheaper and clearer than deleting the first one's contents.
//
// Only the first registered user is the instance admin, which nothing here needs.
func NewClientFor(ctx context.Context, t *testing.T, s *Stack, suffix string) *Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}

	c := &Client{
		BaseURL: s.BaseURL,
		// The first user created gets id 1, which is NEXT_PUBLIC_ADMIN's default, so this
		// account is automatically the instance admin. No bootstrap step is needed, and
		// none is possible: no environment variable creates a user.
		//
		// Lowercase on purpose: authorize() lowercases the submitted username before the
		// lookup, so a mixed-case one here would never match.
		Username: "bookscapee2e" + suffix,
		Email:    "e2e" + suffix + "@example.test",
		Password: "bookscape-e2e-" + secret(t, 16),
		http: &http.Client{
			Jar:     jar,
			Timeout: 2 * time.Minute,
			// Follow NextAuth's redirects but keep the jar, which is the default.
		},
	}

	rejected := c.Register(ctx, t, s)
	c.signIn(ctx, t, rejected)

	return c
}

// Register creates the test user, retrying while the schema is still being migrated. It returns
// the rejection body when Linkwarden refused the registration, or the empty string on success.
//
// This call doubles as the readiness probe for the database, which is why it is the first real
// request rather than a synthetic one: a probe that passes while the first real call still fails
// is worse than no probe. Two outcomes prove the migrations finished - a 2xx, or a 4xx, which
// means the handler ran and reached the schema before objecting. A 5xx or a dropped connection
// means Prisma is still working, so keep waiting.
//
// A 4xx is not failed here. On a fresh stack it means the payload is wrong; on an adopted stack it
// usually means the user already exists from a previous run. Those are indistinguishable from the
// body without matching on Linkwarden's own wording, so the question is settled behaviourally
// instead: signIn either works, and the user existed, or it does not, and it reports this
// rejection alongside its own.
func (c *Client) Register(ctx context.Context, t *testing.T, s *Stack) string {
	t.Helper()

	body := map[string]any{
		"name":     "bookscape e2e",
		"username": c.Username,
		"email":    c.Email,
		"password": c.Password,
	}

	var last string

	for {
		status, payload, err := c.do(ctx, http.MethodPost, "/api/v1/users", body, nil)

		switch {
		case err != nil:
			last = err.Error()
		case status == http.StatusOK || status == http.StatusCreated:
			return ""
		case status >= 400 && status < 500:
			return fmt.Sprintf("HTTP %d: %s", status, truncateForLog(payload))
		default:
			last = fmt.Sprintf("HTTP %d: %s", status, truncateForLog(payload))
		}

		select {
		case <-ctx.Done():
			t.Fatalf("registration never succeeded (last: %s): %v%s", last, ctx.Err(), s.diagnostics(ctx))
		case <-time.After(3 * time.Second):
		}
	}
}

// signIn performs the NextAuth credentials flow and leaves the session cookie in the jar.
//
// registrationRejection, when non-empty, is reported alongside any sign-in failure: together they
// say whether the account could not be created or could not be used, which one body alone does
// not.
func (c *Client) signIn(ctx context.Context, t *testing.T, registrationRejection string) {
	t.Helper()

	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if registrationRejection != "" {
			msg += fmt.Sprintf("\n\nregistration had also been rejected with %s\n"+
				"so either the payload this test sends is wrong - the body above is "+
				"Linkwarden's own validation message and names the field - or the account "+
				"exists from an earlier run with a different password",
				registrationRejection)
		}

		t.Fatal(msg)
	}

	// NextAuth requires its CSRF token, and issues the matching cookie in the same response.
	status, payload, err := c.do(ctx, http.MethodGet, "/api/v1/auth/csrf", nil, nil)
	if err != nil || status != http.StatusOK {
		fail("fetch CSRF token: status %d, err %v, body %s", status, err, truncateForLog(payload))
	}

	var csrf struct {
		Token string `json:"csrfToken"`
	}

	if err := json.Unmarshal([]byte(payload), &csrf); err != nil || csrf.Token == "" {
		fail("parse CSRF token from %s: %v", truncateForLog(payload), err)
	}

	form := url.Values{
		"csrfToken": {csrf.Token},
		"username":  {c.Username},
		"password":  {c.Password},
		// Ask for a JSON reply instead of a 302 to a browser page.
		"json": {"true"},
	}

	status, payload, err = c.form(ctx, "/api/v1/auth/callback/credentials", form)
	if err != nil {
		fail("sign in: %v", err)
	}

	if status != http.StatusOK {
		fail("sign in returned HTTP %d: %s", status, truncateForLog(payload))
	}

	// NextAuth answers a failed credential check with 200 and a URL carrying ?error=, so the
	// status alone does not prove anything.
	if strings.Contains(payload, "error=") {
		fail("sign in rejected the credentials: %s", truncateForLog(payload))
	}

	// Confirm the cookie actually authenticates, so a later failure is about the thing being
	// tested rather than about authentication. The session carries only the user id - not the
	// username - so this checks for an id rather than matching on the credentials.
	status, payload, err = c.do(ctx, http.MethodGet, "/api/v1/auth/session", nil, nil)
	if err != nil || status != http.StatusOK {
		fail("fetch session: status %d, err %v, body %s", status, err, truncateForLog(payload))
	}

	var session struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}

	if err := json.Unmarshal([]byte(payload), &session); err != nil || session.User.ID == 0 {
		fail("session is not authenticated after sign in: %s", truncateForLog(payload))
	}

	// Worth recording: NEXT_PUBLIC_ADMIN defaults to 1, so a first user with id 1 is the
	// instance admin without any bootstrap step. If this is ever not 1, the stack was reused.
	c.UserID = session.User.ID
	t.Logf("signed in as user id %d", c.UserID)
}

// formatHTMLFile selects Linkwarden's Netscape HTML importer.
//
// A number, not the string "htmlFile": MigrationFormat is a TypeScript enum declared without
// explicit values, so it serialises as an ordinal. 0 is the Linkwarden-JSON importer and 1 is the
// HTML one, established by probing a live instance.
//
// Getting this wrong is remarkably unpleasant to debug, which is why it is a named constant with
// this comment attached. The handler's format checks are a chain of `else if` with no final
// `else`, so an unrecognised value means no branch runs, no response is ever sent, and the client
// hangs until its own timeout with nothing in the server log.
const formatHTMLFile = 1

// formatLinkwarden selects Linkwarden's own JSON backup importer. Same enum as formatHTMLFile,
// and 0 is its first member - see that comment for why these are numbers.
const formatLinkwarden = 0

// ImportHTML posts a Netscape bookmark document to the migration endpoint.
func (c *Client) ImportHTML(ctx context.Context, t *testing.T, document []byte) {
	t.Helper()

	c.importDocument(ctx, t, formatHTMLFile, document)
}

// ImportBackup posts a Linkwarden JSON backup document to the migration endpoint.
//
// A 200 from this call proves almost nothing, which is why nothing is asserted on it beyond the
// status. importFromLinkwarden runs the whole import inside a transaction whose rejection is
// swallowed by `.catch(err => console.log(err))`, and the handler then returns 200 "Success."
// regardless - so a document it chokes on imports nothing and reports success. Only the read-back
// says whether anything landed.
func (c *Client) ImportBackup(ctx context.Context, t *testing.T, document []byte) {
	t.Helper()

	c.importDocument(ctx, t, formatLinkwarden, document)
}

func (c *Client) importDocument(ctx context.Context, t *testing.T, format int, document []byte) {
	t.Helper()

	status, payload, err := c.do(ctx, http.MethodPost, "/api/v1/migration", map[string]any{
		"format": format,
		"data":   string(document),
	}, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("import returned HTTP %d: %s\n"+
			"a 400 here is usually IMPORT_LIMIT (10 MB by default) or a malformed document",
			status, truncateForLog(payload))
	}
}

// Backup is the account export the migration endpoint returns on GET: the whole object graph in
// one request, which is a far better read-back surface than paginating /api/v1/links.
//
// The user object is the top level of the response, not wrapped in anything. Linkwarden's
// exportData controller returns {response, status} internally, but the route handler unwraps it
// and sends the user object as the body.
type Backup struct {
	Collections []Collection `json:"collections"`
}

// Collection is one Linkwarden collection, with its links.
type Collection struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ParentID *int64 `json:"parentId"`
	Links    []Link `json:"links"`
}

// Link is one imported bookmark.
type Link struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description"`

	// ImportDate is where ADD_DATE lands. Not CreatedAt - that is Linkwarden's own record of
	// when the row was written, which for an import is "now".
	ImportDate *time.Time `json:"importDate"`
	CreatedAt  *time.Time `json:"createdAt"`

	Tags []struct {
		Name string `json:"name"`
	} `json:"tags"`
}

// TagNames returns the link's tag names.
func (l Link) TagNames() []string {
	out := make([]string, 0, len(l.Tags))
	for _, t := range l.Tags {
		out = append(out, t.Name)
	}

	return out
}

// Backup fetches the full account export.
func (c *Client) Backup(ctx context.Context, t *testing.T) Backup {
	t.Helper()

	status, payload, err := c.do(ctx, http.MethodGet, "/api/v1/migration", nil, nil)
	if err != nil {
		t.Fatalf("fetch backup: %v", err)
	}

	if status != http.StatusOK {
		t.Fatalf("backup returned HTTP %d: %s", status, truncateForLog(payload))
	}

	var b Backup
	if err := json.Unmarshal([]byte(payload), &b); err != nil {
		t.Fatalf("parse backup: %v\nbody: %s", err, truncateForLog(payload))
	}

	return b
}

// LinksByURL flattens the backup into a lookup keyed by URL, plus the collection each link sits
// in. A duplicate URL is reported, since "imported twice" is one of the failures worth catching.
func (b Backup) LinksByURL(t *testing.T) (map[string]Link, map[string]string) {
	t.Helper()

	links := make(map[string]Link)
	collections := make(map[string]string)

	for _, c := range b.Collections {
		for _, l := range c.Links {
			if _, seen := links[l.URL]; seen {
				t.Errorf("URL %q appears more than once in the account", l.URL)
			}

			links[l.URL] = l
			collections[l.URL] = c.Name
		}
	}

	return links, collections
}

// CollectionNames returns every collection name in the account.
func (b Backup) CollectionNames() []string {
	out := make([]string, 0, len(b.Collections))
	for _, c := range b.Collections {
		out = append(out, c.Name)
	}

	return out
}

// do issues a JSON request and returns the status and raw body.
func (c *Client) do(ctx context.Context, method, path string, body any, header http.Header) (int, string, error) {
	var reader io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, "", fmt.Errorf("encode %s %s body: %w", method, path, err)
		}

		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return 0, "", fmt.Errorf("build %s %s: %w", method, path, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	req.Header.Set("Accept", "application/json")

	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	return c.send(req, method, path)
}

// form issues a form-encoded POST, which is what the NextAuth callback expects.
func (c *Client) form(ctx context.Context, path string, values url.Values) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path,
		strings.NewReader(values.Encode()))
	if err != nil {
		return 0, "", fmt.Errorf("build POST %s: %w", path, err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	return c.send(req, http.MethodPost, path)
}

func (c *Client) send(req *http.Request, method, path string) (int, string, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("read %s %s response: %w", method, path, err)
	}

	return resp.StatusCode, string(payload), nil
}

// truncateForLog keeps a failure message readable when the body is an HTML error page.
func truncateForLog(s string) string {
	const limit = 600

	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}

	return s[:limit] + fmt.Sprintf("… (%d bytes total)", len(s))
}
