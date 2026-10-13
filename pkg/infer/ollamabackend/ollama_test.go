package ollamabackend_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/pkg/infer"
	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
)

// stub serves the Ollama endpoints this backend uses. Testing against a real HTTP server rather
// than a mocked client is possible only because the backend talks to the REST API directly; the
// SDK kept its transport unexported.
func stub(t *testing.T, routes map[string]http.HandlerFunc) *ollamabackend.Backend {
	t.Helper()

	mux := http.NewServeMux()
	for pattern, handler := range routes {
		mux.HandleFunc(pattern, handler)
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}

	return ollamabackend.NewAt(base)
}

func TestComplete(t *testing.T) {
	t.Parallel()

	var got struct {
		Model    string            `json:"model"`
		Stream   bool              `json:"stream"`
		Format   json.RawMessage   `json:"format"`
		Messages []json.RawMessage `json:"messages"`
	}

	backend := stub(t, map[string]http.HandlerFunc{
		"/api/chat": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode request: %v", err)
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":   "qwen3-vl:8b",
				"message": map[string]string{"role": "assistant", "content": `{"kind":"screenshot"}`},
				"done":    true,
			})
		},
	})

	resp, err := backend.Complete(context.Background(), infer.Request{
		Model:  "qwen3-vl:8b",
		System: "you classify images",
		Prompt: "what is this",
		Schema: map[string]any{"type": "object"},
		Images: []infer.Image{{MediaType: "image/png", Data: []byte("not really a png")}},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v, want nil", err)
	}

	if diff := cmp.Diff(`{"kind":"screenshot"}`, string(resp.JSON)); diff != "" {
		t.Errorf("JSON mismatch (-want +got):\n%s", diff)
	}

	if resp.Model != "qwen3-vl:8b" {
		t.Errorf("Model = %q, want the model the server reported", resp.Model)
	}

	t.Run("one response, not a stream", func(t *testing.T) {
		// The caller wants a complete JSON document; reassembling one from token deltas
		// buys nothing and risks truncation going unnoticed.
		if got.Stream {
			t.Error("stream = true, want false")
		}
	})

	t.Run("the schema is sent as format", func(t *testing.T) {
		// This is what makes Ollama constrain decoding. Without it the model is merely
		// asked nicely for JSON.
		if len(got.Format) == 0 {
			t.Error("format is empty, so decoding would not be constrained to the schema")
		}
	})

	t.Run("the system prompt leads", func(t *testing.T) {
		if len(got.Messages) != 2 {
			t.Fatalf("messages = %d, want 2 (system then user)", len(got.Messages))
		}

		var first struct {
			Role string `json:"role"`
		}

		if err := json.Unmarshal(got.Messages[0], &first); err != nil {
			t.Fatalf("decode first message: %v", err)
		}

		if first.Role != "system" {
			t.Errorf("first message role = %q, want %q", first.Role, "system")
		}
	})

	t.Run("the image is base64 on the wire", func(t *testing.T) {
		var user struct {
			Images []string `json:"images"`
		}

		if err := json.Unmarshal(got.Messages[1], &user); err != nil {
			t.Fatalf("decode user message: %v", err)
		}

		if len(user.Images) != 1 {
			t.Fatalf("images = %d, want 1", len(user.Images))
		}

		if want := "bm90IHJlYWxseSBhIHBuZw=="; user.Images[0] != want {
			t.Errorf("image = %q, want %q", user.Images[0], want)
		}
	})
}

// TestCompleteRejectsNonJSON matters because the whole point of sending a schema is a typed
// answer. A model that ignores it should fail here rather than at the caller's Unmarshal.
func TestCompleteRejectsNonJSON(t *testing.T) {
	t.Parallel()

	backend := stub(t, map[string]http.HandlerFunc{
		"/api/chat": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]string{"content": "Sure! Here is the answer:"},
			})
		},
	})

	_, err := backend.Complete(context.Background(), infer.Request{Model: "m", Prompt: "p"})
	if err == nil {
		t.Fatal("Complete() error = nil, want an error for a non-JSON response")
	}

	if !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("Complete() error = %q, want it to say the response was not JSON", err)
	}
}

// TestCompleteSurfacesTheServerMessage keeps the actionable half of an Ollama error - "model not
// found" - rather than reporting only a status code.
func TestCompleteSurfacesTheServerMessage(t *testing.T) {
	t.Parallel()

	backend := stub(t, map[string]http.HandlerFunc{
		"/api/chat": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": `model "nope" not found`})
		},
	})

	_, err := backend.Complete(context.Background(), infer.Request{Model: "nope", Prompt: "p"})
	if err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}

	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("Complete() error = %q, want it to carry the server's own message", err)
	}

	// And it should point at the one command that can diagnose this.
	if !strings.Contains(err.Error(), "sicht doctor") {
		t.Errorf("Complete() error = %q, want a hint naming `sicht doctor`", err)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	t.Run("reachable and versioned", func(t *testing.T) {
		t.Parallel()

		backend := stub(t, map[string]http.HandlerFunc{
			"/": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("Ollama is running"))
			},
			"/api/version": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.40.3"})
			},
		})

		info, err := backend.Health(context.Background())
		if err != nil {
			t.Fatalf("Health() error = %v, want nil", err)
		}

		if info.Version != "0.40.3" {
			t.Errorf("Version = %q, want %q", info.Version, "0.40.3")
		}

		// sicht's design makes "nothing leaves the machine" load-bearing, so doctor has
		// to be able to state it rather than imply it.
		if !info.Local {
			t.Error("Local = false, want true for an Ollama backend")
		}
	})

	t.Run("version failure is not fatal", func(t *testing.T) {
		t.Parallel()

		// A server that answers the heartbeat but not the version query is still usable;
		// the version check is advisory.
		backend := stub(t, map[string]http.HandlerFunc{
			"/": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("Ollama is running"))
			},
			"/api/version": func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		})

		info, err := backend.Health(context.Background())
		if err != nil {
			t.Fatalf("Health() error = %v, want nil", err)
		}

		if info.Version != "" {
			t.Errorf("Version = %q, want empty", info.Version)
		}
	})

	t.Run("unreachable says how to fix it", func(t *testing.T) {
		t.Parallel()

		// Nothing listening: the backend must not try to start Ollama, and must say what
		// to check - including the WSL networking case.
		base, err := url.Parse("http://127.0.0.1:1")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		_, err = ollamabackend.NewAt(base).Health(context.Background())
		if err == nil {
			t.Fatal("Health() error = nil, want an error")
		}

		for _, want := range []string{"not reachable", "mirrored"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Health() error = %q, want it to mention %q", err, want)
			}
		}
	})
}

func TestModelInfo(t *testing.T) {
	t.Parallel()

	backend := stub(t, map[string]http.HandlerFunc{
		"/api/show": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"capabilities": []string{"completion", "vision"},
			})
		},
		"/api/ps": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{
				"name":      "qwen3-vl:8b",
				"digest":    "sha256:abc",
				"size":      9_000_000_000,
				"size_vram": 4_000_000_000,
			}}})
		},
	})

	info, err := backend.ModelInfo(context.Background(), "qwen3-vl:8b")
	if err != nil {
		t.Fatalf("ModelInfo() error = %v, want nil", err)
	}

	if !info.HasCapability(ollamabackend.CapabilityVision) {
		t.Errorf("Capabilities = %v, want it to include vision", info.Capabilities)
	}

	if info.Digest != "sha256:abc" {
		t.Errorf("Digest = %q, want it from the running-model list", info.Digest)
	}

	// SizeVRAM below Size means part of the model is on the CPU, which doctor warns about.
	if info.SizeVRAM >= info.Size {
		t.Errorf("SizeVRAM = %d, Size = %d; want the partial-offload case to be visible",
			info.SizeVRAM, info.Size)
	}
}

// TestModelInfoFallsBackToTags covers a model that is present but not loaded: it has no VRAM
// footprint, so the digest has to come from the tag list instead.
func TestModelInfoFallsBackToTags(t *testing.T) {
	t.Parallel()

	backend := stub(t, map[string]http.HandlerFunc{
		"/api/show": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"completion"}})
		},
		"/api/ps": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{}})
		},
		"/api/tags": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{
				"model":  "cold:latest",
				"digest": "sha256:cold",
				"size":   1_000_000,
			}}})
		},
	})

	info, err := backend.ModelInfo(context.Background(), "cold:latest")
	if err != nil {
		t.Fatalf("ModelInfo() error = %v, want nil", err)
	}

	if info.Digest != "sha256:cold" {
		t.Errorf("Digest = %q, want it from the tag list", info.Digest)
	}

	if info.SizeVRAM != 0 {
		t.Errorf("SizeVRAM = %d, want 0 for a model that is not loaded", info.SizeVRAM)
	}
}

func TestEnsureModel(t *testing.T) {
	t.Parallel()

	t.Run("already present is a no-op", func(t *testing.T) {
		t.Parallel()

		pulled := false

		backend := stub(t, map[string]http.HandlerFunc{
			"/api/show": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"completion"}})
			},
			"/api/pull": func(http.ResponseWriter, *http.Request) { pulled = true },
		})

		if err := backend.EnsureModel(context.Background(), "present", nil); err != nil {
			t.Fatalf("EnsureModel() error = %v, want nil", err)
		}

		if pulled {
			t.Error("a model that is already present was pulled again")
		}
	})

	t.Run("missing is pulled, with progress", func(t *testing.T) {
		t.Parallel()

		backend := stub(t, map[string]http.HandlerFunc{
			"/api/show": func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "model not found"})
			},
			"/api/pull": func(w http.ResponseWriter, _ *http.Request) {
				// Newline-delimited JSON, which is how a pull reports progress.
				for _, line := range []string{
					`{"status":"pulling manifest"}`,
					`{"status":"downloading","completed":50,"total":100}`,
					`{"status":"success"}`,
				} {
					_, _ = w.Write([]byte(line + "\n"))
				}
			},
		})

		var updates []infer.Progress

		if err := backend.EnsureModel(context.Background(), "missing",
			func(p infer.Progress) { updates = append(updates, p) }); err != nil {
			t.Fatalf("EnsureModel() error = %v, want nil", err)
		}

		if len(updates) != 3 {
			t.Fatalf("progress updates = %d, want 3", len(updates))
		}

		if updates[1].Completed != 50 || updates[1].Total != 100 {
			t.Errorf("second update = %+v, want 50 of 100", updates[1])
		}
	})

	// A failed pull reports the error inside a 200 response body, so reading only the status
	// code would treat a failure as success.
	t.Run("an error inside the stream is an error", func(t *testing.T) {
		t.Parallel()

		backend := stub(t, map[string]http.HandlerFunc{
			"/api/show": func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			"/api/pull": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"pulling manifest"}` + "\n"))
				_, _ = w.Write([]byte(`{"error":"no space left on device"}` + "\n"))
			},
		})

		err := backend.EnsureModel(context.Background(), "missing", nil)
		if err == nil {
			t.Fatal("EnsureModel() error = nil, want the error from the stream body")
		}

		if !strings.Contains(err.Error(), "no space left") {
			t.Errorf("EnsureModel() error = %q, want the server's message", err)
		}
	})
}

func TestResolveHost(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ in, want string }{
		"empty is the local default": {in: "", want: "http://127.0.0.1:11434"},
		"bare host":                  {in: "myhost", want: "http://myhost:11434"},
		"host and port":              {in: "myhost:1234", want: "http://myhost:1234"},
		"explicit scheme":            {in: "http://myhost:1234", want: "http://myhost:1234"},
		"https gets 443":             {in: "https://ollama.example.com", want: "https://ollama.example.com:443"},
		"trailing slash trimmed":     {in: "http://myhost:1234/", want: "http://myhost:1234"},
		"ipv6":                       {in: "http://[::1]:11434", want: "http://[::1]:11434"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ollamabackend.ResolveHost(tc.in)
			if err != nil {
				t.Fatalf("ResolveHost(%q) error = %v, want nil", tc.in, err)
			}

			if got.String() != tc.want {
				t.Errorf("ResolveHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHostIsLoopback covers the check behind doctor's OLLAMA_HOST warning. Ollama has no
// authentication on its model-management endpoints - one of the eight open advisories against the
// server - so a non-loopback host means anyone who can reach it can pull, delete or run models
// (docs/ARCHITECTURE.md#doctor).
func TestHostIsLoopback(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		host string
		want bool
	}{
		"bare localhost":    {host: "localhost:11434", want: true},
		"loopback v4":       {host: "127.0.0.1:11434", want: true},
		"other 127 address": {host: "127.1.2.3:11434", want: true},
		"loopback v6":       {host: "http://[::1]:11434", want: true},
		"with scheme":       {host: "http://localhost:11434", want: true},
		"no port":           {host: "localhost", want: true},
		"all interfaces":    {host: "0.0.0.0:11434", want: false},
		"lan address":       {host: "192.168.1.50:11434", want: false},
		"public hostname":   {host: "https://ollama.example.com", want: false},
		"a host by name":    {host: "host.docker.internal:11434", want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ollamabackend.HostIsLoopback(tc.host)
			if err != nil {
				t.Fatalf("HostIsLoopback(%q) error = %v, want nil", tc.host, err)
			}

			if got != tc.want {
				t.Errorf("HostIsLoopback(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestHostIsLoopbackRejectsAnEmptyHost(t *testing.T) {
	t.Parallel()

	if _, err := ollamabackend.HostIsLoopback(""); err == nil {
		t.Error(`HostIsLoopback("") error = nil, want an error`)
	}
}

// TestCompletePassesConfiguredOptions is docs/ARCHITECTURE.md#model-stages's requirement that temperature, seed and num_ctx
// reach the server. A backend that quietly dropped them would make a run unreproducible while
// appearing to honour the configuration.
func TestCompletePassesConfiguredOptions(t *testing.T) {
	t.Parallel()

	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"{}"},"done":true}`))
	}))

	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	backend := ollamabackend.NewAt(base)

	if _, err := backend.Complete(context.Background(), infer.Request{
		Model:     "m",
		Prompt:    "hello",
		Options:   map[string]any{"temperature": 0, "seed": 42, "num_ctx": 8192},
		KeepAlive: 30 * time.Minute,
		MaxTokens: 128,
	}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	options, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("no options were sent: %v", body)
	}

	for key, want := range map[string]float64{"temperature": 0, "seed": 42, "num_ctx": 8192} {
		got, ok := options[key].(float64)
		if !ok || got != want {
			t.Errorf("options[%q] = %v, want %v", key, options[key], want)
		}
	}

	// An explicit MaxTokens wins over a num_predict in the configuration.
	if got, _ := options["num_predict"].(float64); got != 128 {
		t.Errorf("num_predict = %v, want 128", options["num_predict"])
	}

	if got, _ := body["keep_alive"].(string); got != "30m0s" {
		t.Errorf("keep_alive = %v, want 30m0s", body["keep_alive"])
	}
}

// TestCompleteOmitsOptionsWhenThereAreNone keeps the request minimal for a server that might not
// know a field, which is the same reason logprobs are omitted unless asked for.
func TestCompleteOmitsOptionsWhenThereAreNone(t *testing.T) {
	t.Parallel()

	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"{}"},"done":true}`))
	}))

	t.Cleanup(srv.Close)

	base, _ := url.Parse(srv.URL)

	if _, err := ollamabackend.NewAt(base).Complete(context.Background(), infer.Request{
		Model: "m", Prompt: "hello",
	}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	for _, key := range []string{"options", "keep_alive", "logprobs", "top_logprobs"} {
		if _, present := body[key]; present {
			t.Errorf("%q was sent although nothing asked for it: %v", key, body[key])
		}
	}
}

// TestCompleteSendsThinkOnlyWhenSet pins the three states of Request.Think. Omitted and false are
// not the same request: qwen3-vl thinks by default, so a request that leaves the field out gets
// the reasoning, and the reasoning shares the context window with the answer - on the 2017
// benchmark it exhausted that window on 22 of 87 files before any answer was written.
func TestCompleteSendsThinkOnlyWhenSet(t *testing.T) {
	t.Parallel()

	off, on := false, true

	tests := map[string]struct {
		think   *bool
		present bool
		want    bool
	}{
		"unset leaves it to the model": {think: nil, present: false},
		"false is sent explicitly":     {think: &off, present: true, want: false},
		"true is sent explicitly":      {think: &on, present: true, want: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var body map[string]any

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"{}"},"done":true}`))
			}))

			t.Cleanup(srv.Close)

			base, _ := url.Parse(srv.URL)

			if _, err := ollamabackend.NewAt(base).Complete(context.Background(), infer.Request{
				Model: "m", Prompt: "hello", Think: tt.think,
			}); err != nil {
				t.Fatalf("Complete() error = %v", err)
			}

			got, present := body["think"]
			if present != tt.present {
				t.Fatalf("think present = %v, want %v (body %v)", present, tt.present, body)
			}

			if present && got != tt.want {
				t.Errorf("think = %v, want %v", got, tt.want)
			}
		})
	}
}
