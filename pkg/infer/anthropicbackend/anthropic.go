// Package anthropicbackend implements infer.Completer against the Anthropic Messages API.
//
// Named for what it is rather than imported as `anthropic`, so that the SDK keeps that name at
// every call site and nobody has to work out which `anthropic` a given line means.
package anthropicbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/adnoctem/pertry/pkg/infer"
)

// defaultMaxTokens bounds a response when the caller does not. Generous enough for a batch of
// several dozen classifications without letting a runaway response cost more than the batch is
// worth.
const defaultMaxTokens = 16000

// Backend talks to the Anthropic Messages API.
type Backend struct {
	client anthropic.Client
}

// New builds a Backend.
//
// Credentials come from the SDK's own resolution: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or a
// profile written by `ant auth login`. Only the first two are visible from here, which is why
// HasCredentialsEnv is advisory rather than a precondition.
func New(opts ...option.RequestOption) *Backend {
	return &Backend{client: anthropic.NewClient(opts...)}
}

// Name identifies the backend.
func (b *Backend) Name() string { return "anthropic" }

// HasCredentialsEnv reports whether an API key is in the environment. It is only used to decide
// whether to print a hint before a call that is about to fail: a configured `ant auth login`
// profile works without one, so a false result is not an error.
func HasCredentialsEnv() bool {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if os.Getenv(name) != "" {
			return true
		}
	}

	return false
}

// Complete sends one request.
func (b *Backend) Complete(ctx context.Context, req infer.Request) (infer.Response, error) {
	if len(req.Images) > 0 {
		// Deliberately refused rather than quietly dropped: this backend exists for
		// bookscape's text classification, and silently classifying an image by its
		// surrounding text would be worse than an error.
		return infer.Response{}, fmt.Errorf("anthropic backend does not send images: %w", infer.ErrUnsupported)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		// anthropic.Model is an alias for string, so a model id with no typed constant
		// yet - the normal case - passes through as the plain id.
		Model:     req.Model,
		MaxTokens: maxTokens,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
		},
	}

	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{
			Text: req.System,
			// The system prompt is identical across every request in a run, so caching
			// the prefix pays for itself from the second call on.
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}

	if req.Schema != nil {
		// Structured outputs constrain decoding to the schema. This replaces assistant
		// prefill, which is rejected with a 400 on current models.
		params.OutputConfig = anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: req.Schema},
		}
	}

	message, err := b.client.Messages.New(ctx, params)
	if err != nil {
		return infer.Response{}, fmt.Errorf("anthropic %s: %w", req.Model, err)
	}

	// A refusal is an HTTP 200 with no usable content, so the stop reason has to be checked
	// before the content is read.
	switch message.StopReason {
	case anthropic.StopReasonRefusal:
		return infer.Response{}, fmt.Errorf("anthropic %s declined the request (%s): %s",
			req.Model, message.StopDetails.Category, message.StopDetails.Explanation)
	case anthropic.StopReasonMaxTokens:
		return infer.Response{}, fmt.Errorf(
			"anthropic %s hit the %d token cap, so the JSON is truncated\nhint: send fewer items per request",
			req.Model, maxTokens)
	}

	var builder strings.Builder

	for _, block := range message.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			builder.WriteString(t.Text)
		}
	}

	text := builder.String()

	if text == "" {
		return infer.Response{}, fmt.Errorf("anthropic %s returned no text content (stop reason %q)",
			req.Model, message.StopReason)
	}

	if !json.Valid([]byte(text)) {
		return infer.Response{}, fmt.Errorf("anthropic %s returned text that is not JSON: %.200s", req.Model, text)
	}

	return infer.Response{
		JSON:  json.RawMessage(text),
		Model: message.Model,
		Usage: infer.Usage{
			InputTokens:  message.Usage.InputTokens,
			OutputTokens: message.Usage.OutputTokens,
		},
	}, nil
}

var _ infer.Completer = (*Backend)(nil)
