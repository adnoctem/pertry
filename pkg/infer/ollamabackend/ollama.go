// Package ollamabackend implements infer.Completer and infer.ModelManager against a locally
// installed Ollama, over its REST API.
//
// This is the only backend `sicht` wires up, and that is a design property rather than a default:
// the archive it sorts contains third parties' personal data visible in screenshots, and
// docs/ARCHITECTURE.md#sicht-scope and docs/ARCHITECTURE.md#privacy-notes require that nothing leaves the machine. Health therefore
// reports Local: true, so `sicht doctor` can state the guarantee rather than imply it.
//
// It implements ModelManager because Ollama hosts its own weights: it can be asked what it has,
// what those weights are, and to fetch what is missing. The hosted backends cannot answer any of
// those questions, which is why ModelManager is a separate interface callers assert for.
//
// # Why there is no SDK here
//
// docs/CONTRIBUTING.md originally named `github.com/ollama/ollama/api`, and docs/ARCHITECTURE.md#inference-layer anticipated
// that the module carries published advisories while reasoning that "govulncheck reports reachable
// symbols only". In practice it does not help: the eight open advisories are all server-side
// (denial of service, divide by zero, out-of-bounds read, missing authentication on model
// management), none has a fixed version, and their affected-symbol sets cover the module broadly
// enough that any use of the client package is reported as reachable — one trace even ran through
// tablewriter to an error's Error method, which is interface over-approximation rather than a real
// path. Importing the SDK and keeping govulncheck green were therefore mutually exclusive and
// permanently so.
//
// Talking to the REST API directly costs little: the surface this needs is seven endpoints of
// plain JSON, and the SDK was being used as a thin wrapper over exactly that. It also drops a
// dependency whose own go.mod requires cgo SQLite and tree-sitter, which this repository avoids
// everywhere else, and removes a third party's floor on our Go version.
//
// The advisories themselves do not go away — they are properties of the Ollama server the user
// runs. That is the right place to address them, and `sicht doctor` does: it checks the server
// version against a pinned minimum and warns when OLLAMA_HOST is not loopback (docs/ARCHITECTURE.md#doctor).
package ollamabackend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// CapabilityVision is the capability string Ollama reports for a model that accepts images.
const CapabilityVision = "vision"

// CapabilityThinking is reported by a model that reasons before it answers.
//
// For sicht that is a liability rather than a feature: the reasoning shares the context window
// with the image and the answer, and on an ambiguous image it can fill the window before any
// answer is written. qwen3-vl's default tags are the thinking variants.
const CapabilityThinking = "thinking"

// defaultPort is Ollama's own default, applied when OLLAMA_HOST names no port.
const defaultPort = "11434"

// Backend talks to an Ollama server over its REST API.
type Backend struct {
	base *url.URL
	http *http.Client
}

// New builds a Backend from the environment, honouring OLLAMA_HOST.
//
// It does not check reachability: `sicht` never starts Ollama and wants a clear message from
// `doctor` rather than a failure at construction (docs/ARCHITECTURE.md#inference-backend-and-model-choice).
func New() (*Backend, error) {
	base, err := ResolveHost(os.Getenv("OLLAMA_HOST"))
	if err != nil {
		return nil, err
	}

	return NewAt(base), nil
}

// NewAt builds a Backend for an explicit address, used by the tests against a stub server.
func NewAt(base *url.URL) *Backend {
	return &Backend{
		base: base,
		// No overall timeout: a model pull can legitimately run for many minutes, and a
		// cold vision model's first response is slow. Callers bound their own work with a
		// context, which is the only bound that can distinguish the two.
		http: &http.Client{},
	}
}

// ResolveHost applies Ollama's own interpretation of OLLAMA_HOST: an empty value means the local
// default, a bare host or host:port means http, and a missing port means 11434.
//
// Reimplemented rather than imported so that the resolved address is knowable here — `doctor` has
// to report which address it is talking to, and whether that address is loopback.
func ResolveHost(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", defaultPort)}, nil
	}

	if !strings.Contains(value, "://") {
		value = "http://" + value
	}

	u, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse OLLAMA_HOST %q: %w", value, err)
	}

	if u.Hostname() == "" {
		return nil, fmt.Errorf("OLLAMA_HOST %q names no host", value)
	}

	if u.Port() == "" {
		port := defaultPort

		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			// Ollama uses 80 for an explicit http scheme with no port, and 11434 only
			// when the scheme was inferred. Treating both as 11434 would be wrong for
			// someone fronting it with a reverse proxy.
			if strings.HasPrefix(value, "http://") && !strings.Contains(value, ":"+defaultPort) {
				port = defaultPort
			}
		}

		u.Host = net.JoinHostPort(u.Hostname(), port)
	}

	u.Path = strings.TrimSuffix(u.Path, "/")

	return u, nil
}

// Name identifies the backend.
func (b *Backend) Name() string { return "ollama" }

// Host reports the server address in use.
func (b *Backend) Host() string {
	if b.base == nil {
		return ""
	}

	return b.base.String()
}

// EnvHost returns OLLAMA_HOST as set, for reporting what the environment asked for rather than
// what it resolved to.
func EnvHost() string { return os.Getenv("OLLAMA_HOST") }

// HostIsLoopback reports whether an address keeps traffic on this machine.
//
// Ollama has no authentication on its model-management endpoints, so a non-loopback OLLAMA_HOST
// means anyone who can reach it can pull, delete or run models. That is one of the eight open
// advisories against the server, and `sicht doctor` warns on it (docs/ARCHITECTURE.md#doctor).
func HostIsLoopback(host string) (bool, error) {
	if strings.TrimSpace(host) == "" {
		return false, errors.New("no host configured")
	}

	u, err := ResolveHost(host)
	if err != nil {
		return false, err
	}

	name := u.Hostname()

	if ip := net.ParseIP(name); ip != nil {
		return ip.IsLoopback(), nil
	}

	return name == "localhost", nil
}

// do sends a JSON request and decodes a JSON response. A nil out skips decoding.
func (b *Backend) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader

	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode %s %s request: %w", method, path, err)
		}

		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.base.String()+path, body)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}

	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		// Ollama reports errors as {"error": "..."}, and the message is usually the
		// actionable part - "model not found", "invalid model name".
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		var wrapped struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(payload, &wrapped) == nil && wrapped.Error != "" {
			return fmt.Errorf("%s %s: %s (HTTP %d)", method, path, wrapped.Error, resp.StatusCode)
		}

		return fmt.Errorf("%s %s: HTTP %d: %.200s", method, path, resp.StatusCode, payload)
	}

	if out == nil {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}

	return nil
}

// stream sends a JSON request and calls onLine for each newline-delimited JSON object, which is
// how Ollama reports pull progress.
func (b *Backend) stream(ctx context.Context, path string, in any, onLine func([]byte) error) error {
	encoded, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode POST %s request: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base.String()+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build POST %s: %w", path, err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return fmt.Errorf("POST %s: HTTP %d: %.200s", path, resp.StatusCode, payload)
	}

	decoder := json.NewDecoder(resp.Body)

	for {
		var raw json.RawMessage

		switch err := decoder.Decode(&raw); {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("decode POST %s stream: %w", path, err)
		}

		if err := onLine(raw); err != nil {
			return err
		}
	}
}

// base64Encode is split out so capabilities.go does not need the import for one call.
func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
