package ollamabackend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/adnoctem/pertry/pkg/infer"
)

// chatRequest is Ollama's POST /api/chat body, narrowed to the fields this backend sets.
type chatRequest struct {
	Model    string          `json:"model"`
	Messages []chatMessage   `json:"messages"`
	Stream   bool            `json:"stream"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`

	// KeepAlive is how long the server holds the model in memory after this request.
	// Reloading a multi-gigabyte vision model per image otherwise dominates the runtime.
	KeepAlive string `json:"keep_alive,omitempty"`

	// Logprobs and TopLogprobs are omitted entirely unless asked for, so that a server too
	// old to know them is not sent fields it would reject.
	Logprobs    bool `json:"logprobs,omitempty"`
	TopLogprobs int  `json:"top_logprobs,omitempty"`

	// Think is omitted when nil so that the model's own default applies; `false` has to be
	// sent explicitly, because a thinking model left to itself thinks.
	Think *bool `json:"think,omitempty"`
}

// chatMessage is one turn. Images are base64-encoded strings on the wire.
type chatMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type chatResponse struct {
	Model   string      `json:"model"`
	Message chatMessage `json:"message"`
	Done    bool        `json:"done"`

	// PromptEvalCount and EvalCount are Ollama's token accounting.
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`

	// Logprobs is present only when the request asked for them and the server supports
	// them. An older server simply omits the field, which is why nothing here fails when it
	// is absent.
	Logprobs []chatLogprob `json:"logprobs,omitempty"`
}

// chatLogprob is one token's probability as Ollama reports it.
type chatLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`

	// TopLogprobs is the alternatives at this position, when top_logprobs asked for them.
	TopLogprobs []chatLogprob `json:"top_logprobs,omitempty"`
}

// Complete sends one request, constraining the response to the schema.
func (b *Backend) Complete(ctx context.Context, req infer.Request) (infer.Response, error) {
	message := chatMessage{Role: "user", Content: req.Prompt}

	for i, img := range req.Images {
		if len(img.Data) == 0 {
			return infer.Response{}, fmt.Errorf("image %d has no Data: %w", i, infer.ErrUnsupported)
		}

		// Ollama sniffs the format itself, so MediaType is not sent - but it is still
		// required on infer.Image, because the other backends need it and an image with
		// no declared type is a caller bug worth catching consistently.
		message.Images = append(message.Images, base64Encode(img.Data))
	}

	messages := []chatMessage{message}
	if req.System != "" {
		messages = append([]chatMessage{{Role: "system", Content: req.System}}, messages...)
	}

	body := chatRequest{
		Model:    req.Model,
		Messages: messages,
		// One response rather than a token stream: the caller wants a complete JSON
		// document, and reassembling it from deltas buys nothing here.
		Stream: false,
	}

	if req.Schema != nil {
		encoded, err := json.Marshal(req.Schema)
		if err != nil {
			return infer.Response{}, fmt.Errorf("encode schema: %w", err)
		}

		// Ollama enforces `format` during constrained decoding.
		body.Format = encoded
	}

	// Configured options first, then the ones derived from the request, so that an explicit
	// MaxTokens wins over a num_predict in the configuration rather than being silently
	// overridden by it.
	if len(req.Options) > 0 || req.MaxTokens > 0 {
		body.Options = make(map[string]any, len(req.Options)+1)

		for k, v := range req.Options {
			body.Options[k] = v
		}

		if req.MaxTokens > 0 {
			body.Options["num_predict"] = req.MaxTokens
		}
	}

	if req.KeepAlive > 0 {
		// Ollama accepts a Go-style duration string here.
		body.KeepAlive = req.KeepAlive.String()
	}

	if req.Logprobs {
		body.Logprobs = true
		body.TopLogprobs = req.TopLogprobs
	}

	body.Think = req.Think

	var resp chatResponse
	if err := b.do(ctx, "POST", "/api/chat", body, &resp); err != nil {
		return infer.Response{}, fmt.Errorf("ollama chat with %s: %w\n"+
			"hint: run `sicht doctor` - Ollama is never started for you", req.Model, err)
	}

	text := strings.TrimSpace(resp.Message.Content)
	if text == "" {
		// Reported as ErrEmptyResponse rather than a generic failure, because it is
		// retryable: the request succeeded and the model simply said nothing, which a
		// second attempt usually fixes.
		//
		// Observed on qwen3-vl, which reasons before answering: roughly one request in
		// thirty returns reasoning and no content. Context exhaustion was the obvious
		// suspect and the measurements do not support it - a 1280 px image costs about
		// 1100 prompt tokens and the reasoning 300 to 1700, so a default 8192 has ample
		// room. The generated-token count is reported because it is the one piece of
		// evidence available about what the model did instead of answering.
		detail := ""
		if resp.EvalCount > 0 {
			detail = fmt.Sprintf(" after generating %d token(s), none of which were content",
				resp.EvalCount)
		}

		return infer.Response{}, fmt.Errorf("ollama %s: %w%s", req.Model, infer.ErrEmptyResponse, detail)
	}

	if !json.Valid([]byte(text)) {
		return infer.Response{}, fmt.Errorf("ollama %s returned text that is not JSON: %.200s", req.Model, text)
	}

	model := resp.Model
	if model == "" {
		model = req.Model
	}

	out := infer.Response{
		JSON:  json.RawMessage(text),
		Model: model,
		Usage: infer.Usage{
			InputTokens:  int64(resp.PromptEvalCount),
			OutputTokens: int64(resp.EvalCount),
		},
		Logprobs: convertLogprobs(resp.Logprobs),
	}

	return out, nil
}

// convertLogprobs maps Ollama's shape onto the shared one. Returns nil when the server reported
// none, which is the normal case for a server that does not support them.
func convertLogprobs(in []chatLogprob) []infer.TokenLogprob {
	if len(in) == 0 {
		return nil
	}

	out := make([]infer.TokenLogprob, 0, len(in))

	for _, t := range in {
		converted := infer.TokenLogprob{Token: t.Token, Logprob: t.Logprob}

		for _, alt := range t.TopLogprobs {
			converted.Top = append(converted.Top, infer.Alternative{
				Token: alt.Token, Logprob: alt.Logprob,
			})
		}

		out = append(out, converted)
	}

	return out
}

// Unload asks the server to release a model from memory immediately.
//
// Worth having explicitly rather than relying on keep_alive to expire. A caller that works through
// several models in one run - the benchmark does - would otherwise hold all of them at once, and on
// a host with a hard memory ceiling that is how a run kills the machine rather than finishing. A
// keep_alive of zero on an empty request is Ollama's documented way to say "drop it now".
func (b *Backend) Unload(ctx context.Context, model string) error {
	body := chatRequest{
		Model:     model,
		Messages:  []chatMessage{},
		Stream:    false,
		KeepAlive: "0",
	}

	if err := b.do(ctx, "POST", "/api/chat", body, nil); err != nil {
		return fmt.Errorf("unload %s: %w", model, err)
	}

	return nil
}

// SupportsLogprobs reports whether a model and server combination actually returns logprobs.
//
// Asked rather than assumed, with a trivial request, because the answer is a property of the
// server build and not of the API: a server that does not support them omits the field instead of
// refusing the request, so the only way to find out is to try. docs/ARCHITECTURE.md#confidence's confidence scoring depends
// on the answer, and the fallback it describes is costly enough to be worth not entering by
// mistake.
func (b *Backend) SupportsLogprobs(ctx context.Context, model string) (bool, error) {
	resp, err := b.Complete(ctx, infer.Request{
		Model:  model,
		Prompt: "Reply with the JSON {\"ok\":true} and nothing else.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
			"required":   []string{"ok"},
		},
		Logprobs:    true,
		TopLogprobs: 2,
		MaxTokens:   16,
	})
	if err != nil {
		return false, err
	}

	return len(resp.Logprobs) > 0, nil
}

// Health reports whether the server is reachable and what version it is.
func (b *Backend) Health(ctx context.Context) (infer.ServerInfo, error) {
	// Locality is derived from the host, not assumed. Ollama is the local backend by
	// convention, but OLLAMA_HOST can point anywhere, and a caller asking whether inference is
	// local is asking a privacy question: answering "yes" for a server on another machine
	// would be worse than not answering. An unparsable host is treated as non-local, because
	// the safe reading of "I cannot tell" is "assume it leaves".
	local, err := HostIsLoopback(b.Host())
	if err != nil {
		local = false
	}

	info := infer.ServerInfo{Name: b.Name(), Local: local}

	// GET / is Ollama's heartbeat: it answers "Ollama is running" and touches no model.
	if err := b.do(ctx, "GET", "/", nil, nil); err != nil {
		// A zero ServerInfo rather than a partially populated one: nothing was learned
		// about the server, so a caller must not be able to read a locality claim out of a
		// failed health check.
		return infer.ServerInfo{}, fmt.Errorf("ollama at %s is not reachable: %w\n"+
			"hint: start Ollama. Under WSL, check that networkingMode=mirrored is set, or point "+
			"OLLAMA_HOST at the Windows host explicitly", b.Host(), err)
	}

	var version struct {
		Version string `json:"version"`
	}

	if err := b.do(ctx, "GET", "/api/version", nil, &version); err != nil {
		//nolint:nilerr // Reachable but not forthcoming about its version. Not fatal:
		// the heartbeat determines usability and the version is advisory.
		return info, nil
	}

	info.Version = version.Version

	return info, nil
}

// showResponse is POST /api/show, narrowed.
type showResponse struct {
	Capabilities []string `json:"capabilities"`
	Details      struct {
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

// modelEntry is one entry of GET /api/tags or GET /api/ps.
type modelEntry struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	SizeVRAM int64  `json:"size_vram"`
}

type modelList struct {
	Models []modelEntry `json:"models"`
}

// ModelInfo describes a model the server already has.
func (b *Backend) ModelInfo(ctx context.Context, name string) (infer.ModelInfo, error) {
	var shown showResponse
	if err := b.do(ctx, "POST", "/api/show", map[string]string{"model": name}, &shown); err != nil {
		return infer.ModelInfo{}, fmt.Errorf("ollama does not have %s: %w\nhint: `sicht model pull`", name, err)
	}

	info := infer.ModelInfo{Name: name, Capabilities: shown.Capabilities}

	// Sizes come from the running-model list rather than from show: a VRAM footprint only
	// exists once the weights are loaded, so a present-but-cold model has none to report. A
	// failure here leaves the sizes zero rather than failing the call.
	if running, err := b.listModels(ctx, "/api/ps"); err == nil {
		if m, ok := findModel(running, name); ok {
			info.Digest, info.Size, info.SizeVRAM = m.Digest, m.Size, m.SizeVRAM
			info.Loaded = true
		}
	}

	if info.Digest == "" {
		if all, err := b.listModels(ctx, "/api/tags"); err == nil {
			if m, ok := findModel(all, name); ok {
				info.Digest, info.Size = m.Digest, m.Size
			}
		}
	}

	return info, nil
}

func (b *Backend) listModels(ctx context.Context, path string) ([]modelEntry, error) {
	var list modelList
	if err := b.do(ctx, "GET", path, nil, &list); err != nil {
		return nil, err
	}

	return list.Models, nil
}

// findModel matches on either name, because Ollama reports the tag in one field and the resolved
// model in the other depending on the endpoint.
func findModel(entries []modelEntry, name string) (modelEntry, bool) {
	for _, m := range entries {
		if m.Name == name || m.Model == name {
			return m, true
		}
	}

	return modelEntry{}, false
}

// EnsureModel pulls the model if the server does not already have it.
func (b *Backend) EnsureModel(ctx context.Context, name string, progress func(infer.Progress)) error {
	if err := b.do(ctx, "POST", "/api/show", map[string]string{"model": name}, nil); err == nil {
		return nil
	}

	// Streaming so that a multi-gigabyte pull can report progress rather than appearing to
	// hang. Each line is a complete JSON object.
	return b.stream(ctx, "/api/pull", map[string]any{"model": name, "stream": true}, func(line []byte) error {
		var update struct {
			Status    string `json:"status"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
			Error     string `json:"error"`
		}

		if err := json.Unmarshal(line, &update); err != nil {
			return fmt.Errorf("decode pull progress: %w", err)
		}

		// A pull reports failure in the stream body with a 200 status, so the error has
		// to be read out of the payload rather than from the response code.
		if update.Error != "" {
			return fmt.Errorf("pull %s: %s", name, update.Error)
		}

		if progress != nil {
			progress(infer.Progress{
				Status:    update.Status,
				Completed: update.Completed,
				Total:     update.Total,
			})
		}

		return nil
	})
}

var (
	_ infer.Completer    = (*Backend)(nil)
	_ infer.ModelManager = (*Backend)(nil)
)
