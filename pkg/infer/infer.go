// Package infer is the provider-agnostic inference layer shared by the applications in this
// repository.
//
// It exists because both applications need the same thing from a model — a typed answer, with
// provenance, that can be validated — while needing it from different providers for different
// reasons. `bookscape` classifies text against a hosted model. `sicht` classifies images against
// a model on the local machine, and must keep doing so: the archive it sorts contains third
// parties' personal data, and docs/ARCHITECTURE.md#sicht-scope and docs/ARCHITECTURE.md#privacy-notes make local-only inference a
// property of the design rather than a default. Nothing here weakens that; which backends an
// application wires up is the application's decision.
//
// # Two capabilities, not one
//
// The interfaces are split because the providers genuinely differ in kind, not just in vendor.
//
//   - A [Completer] takes a prompt and a JSON schema and returns JSON. It can produce free text,
//     so it is the only way to get a generated description or an observational field.
//   - A [Decider] takes evidence and a set of bounded questions and returns typed answers with
//     calibrated probabilities. It cannot produce free text at all.
//
// Pretending these are one interface would mean a lowest common denominator that serves neither.
// A backend implements whichever it can, and a caller asks for what it needs.
//
// # What this package does not do
//
// It is a transport. It does not validate a response against the schema it sent, and it does not
// interpret the answer: a schema can constrain the shape of a value but not its meaning, so
// checking that a returned collection is one of the configured collections stays with the caller
// that knows what the configured collections are. Constrained decoding is not a substitute for
// that check (docs/ARCHITECTURE.md#classification).
package infer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrUnsupported is returned by a backend asked for something it cannot do — images sent to a
// text-only model, or a question type a provider does not implement. Callers can branch on it to
// fall back rather than fail.
var ErrUnsupported = errors.New("unsupported by this backend")

// ErrEmptyResponse is returned when a provider answered successfully but said nothing.
//
// Worth its own sentinel because it is **retryable** while most failures are not. It is what a
// context exhausted by the model's own reasoning looks like from the outside: the request
// succeeds, the token budget is spent before the answer begins, and the content comes back empty
// rather than truncated. A caller that treated this like a transport failure would discard a file
// that a second attempt, or a larger context, would answer perfectly well.
var ErrEmptyResponse = errors.New("the provider returned no content")

// Image is one image to send alongside a prompt.
type Image struct {
	// MediaType is an IANA type such as "image/png" or "image/jpeg". Required: providers
	// disagree about sniffing, and guessing wrong fails at the far end with a poor message.
	MediaType string

	// Data is the raw encoded image, not base64. Each backend encodes for its own wire format.
	Data []byte
}

// Request is one completion request.
type Request struct {
	// Model is the provider's own model identifier. Deliberately a plain string: every
	// provider names models differently and a shared enum would be stale within a month.
	Model string

	// System is the system prompt. Where a provider has no separate channel for it, the
	// backend prepends it to the prompt.
	System string

	// Prompt is the user turn.
	Prompt string

	// Images are optional. A backend without vision returns ErrUnsupported rather than
	// silently dropping them, because silently classifying an image by its filename is worse
	// than failing.
	Images []Image

	// Schema is a JSON Schema the response must satisfy. Backends constrain decoding to it
	// where the provider supports that; the caller validates the result either way.
	Schema map[string]any

	// DecisionFields names the schema properties that carry a decision rather than an
	// observation, so that a backend able to report per-field confidence knows which fields
	// to report on. Optional.
	DecisionFields []string

	// MaxTokens bounds the response. Zero lets the backend choose.
	MaxTokens int64

	// Options are provider-specific generation settings, passed through verbatim.
	//
	// docs/ARCHITECTURE.md#model-stages of the sicht design requires this: temperature, seed and num_ctx are configured by
	// the user and a backend that quietly ignored them would make a run unreproducible while
	// appearing to honour the configuration. A backend merges these over its own defaults.
	Options map[string]any

	// KeepAlive is how long a self-hosting provider should keep the model in memory after the
	// request. Ignored by a hosted one. Reloading a multi-gigabyte vision model per image
	// otherwise dominates the runtime.
	KeepAlive time.Duration

	// Logprobs asks the backend to report per-token log probabilities.
	//
	// Requested rather than assumed, because it is not free: the provider returns a
	// probability for every token, which for a long structured answer is a great deal more
	// response than the answer itself. A backend that cannot report them leaves
	// Response.Logprobs empty rather than failing - the caller is asking for a nicety, not a
	// requirement.
	Logprobs bool

	// TopLogprobs asks for this many alternatives per token, which is what makes a review
	// queue able to offer "did you mean" rather than only "this looks uncertain". Ignored
	// unless Logprobs is set.
	TopLogprobs int

	// Think turns a reasoning model's thinking phase on or off. Nil leaves it to the model's
	// own default, which is not the same as false: qwen3-vl thinks by default, and a
	// request that says nothing gets the reasoning, the latency and the token cost of it.
	//
	// A pointer because the three states are all meaningful. A backend whose provider has
	// no such switch ignores it.
	Think *bool
}

// Response is one completion.
type Response struct {
	// JSON is the model's answer. Shaped by Request.Schema, but not validated against it
	// here.
	JSON json.RawMessage

	// Model is what actually answered, which may differ from what was asked for when a
	// provider aliases or substitutes.
	Model string

	// ModelDigest identifies the exact weights, where the provider exposes it. Ollama does;
	// hosted providers generally do not. Recorded with results so that a re-run under
	// different weights is distinguishable.
	ModelDigest string

	// Confidence is per-field, keyed by the names given in Request.DecisionFields, and is
	// best-effort: empty when the backend cannot derive one. Never assume it is populated.
	Confidence map[string]float64

	// Usage is what the request cost, where the provider reports it.
	Usage Usage

	// Logprobs are the per-token log probabilities, in generation order, when they were
	// requested and the backend can report them. Empty otherwise - never assume it is
	// populated.
	Logprobs []TokenLogprob
}

// TokenLogprob is one generated token and how likely the model thought it was.
type TokenLogprob struct {
	// Token is the text the model emitted. Raw: it may be a fragment of a word, a quote
	// character, or part of a multi-byte rune, because a tokeniser owes nobody tidy pieces.
	Token string

	// Logprob is the natural log of the probability. Zero means certainty, and the value is
	// always negative or zero.
	Logprob float64

	// Top are the alternatives the model considered at this position, most likely first,
	// when TopLogprobs asked for them.
	Top []Alternative
}

// Alternative is one token the model might have emitted instead.
type Alternative struct {
	Token   string
	Logprob float64
}

// Probability converts a log probability to a probability.
func (t TokenLogprob) Probability() float64 { return math.Exp(t.Logprob) }

// Probability converts a log probability to a probability.
func (a Alternative) Probability() float64 { return math.Exp(a.Logprob) }

// Usage is the token accounting for one request.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Completer turns a prompt and a schema into JSON.
type Completer interface {
	// Name identifies the backend in logs and in stored provenance.
	Name() string

	// Complete sends one request.
	Complete(ctx context.Context, req Request) (Response, error)
}

// QuestionKind is the shape of a bounded question.
type QuestionKind string

const (
	// KindPredicate asks whether a condition holds, and answers with a probability.
	KindPredicate QuestionKind = "predicate"

	// KindChoice asks which of a fixed set of options applies.
	KindChoice QuestionKind = "choice"

	// KindScore asks where the evidence sits on an ordered set of levels.
	KindScore QuestionKind = "score"
)

// Question is one bounded question about the evidence.
type Question struct {
	// Key is the caller's identifier, echoed back on the answer so a response can be matched
	// to its question without relying on order.
	Key string

	Kind QuestionKind

	// Instructions tells the model what to evaluate.
	Instructions string

	// Choices are the allowed options for KindChoice.
	Choices []string

	// Levels are the ordered levels for KindScore, lowest first.
	Levels []string
}

// Validate checks the question is answerable.
func (q Question) Validate() error {
	if q.Key == "" {
		return errors.New("question has no key")
	}

	switch q.Kind {
	case KindPredicate:
	case KindChoice:
		if len(q.Choices) < 2 {
			return fmt.Errorf("question %q is a choice with %d options, which needs at least 2", q.Key, len(q.Choices))
		}
	case KindScore:
		if len(q.Levels) < 2 {
			return fmt.Errorf("question %q is a score with %d levels, which needs at least 2", q.Key, len(q.Levels))
		}
	default:
		return fmt.Errorf("question %q has unknown kind %q", q.Key, q.Kind)
	}

	return nil
}

// DecisionRequest is evidence plus the questions to answer about it.
type DecisionRequest struct {
	Model string

	// Evidence is the shared context every question is evaluated against.
	Evidence string

	// Images are optional additional evidence.
	Images []Image

	Questions []Question
}

// Validate checks every question.
func (d DecisionRequest) Validate() error {
	if len(d.Questions) == 0 {
		return errors.New("no questions to answer")
	}

	seen := make(map[string]struct{}, len(d.Questions))

	for _, q := range d.Questions {
		if err := q.Validate(); err != nil {
			return err
		}

		if _, dup := seen[q.Key]; dup {
			return fmt.Errorf("question key %q appears twice", q.Key)
		}

		seen[q.Key] = struct{}{}
	}

	return nil
}

// Answer is one typed answer.
type Answer struct {
	// Key echoes the question's key.
	Key string

	// Value is the chosen option, the chosen level, or "true"/"false" for a predicate.
	Value string

	// Probability is the backend's confidence in Value, from 0 to 1. Unlike
	// Response.Confidence this is the point of the call, so a Decider that cannot supply it
	// should not claim to be one.
	Probability float64

	// Alternatives are the other candidates and their probabilities, for a review UI to
	// offer. Empty when the backend reports only the winner.
	Alternatives map[string]float64
}

// Decider answers bounded questions about evidence.
type Decider interface {
	Name() string

	// Decide answers every question in one call, in the order asked.
	Decide(ctx context.Context, req DecisionRequest) ([]Answer, error)
}

// ServerInfo describes a reachable inference server.
type ServerInfo struct {
	// Name is the backend's name.
	Name string

	// Version is the server's reported version, empty where the provider has no such
	// concept.
	Version string

	// Local reports whether inference happens on this machine. `sicht doctor` surfaces this,
	// because "is anything leaving the machine" is a question its design makes load-bearing.
	Local bool
}

// ModelInfo describes one model as a backend sees it.
type ModelInfo struct {
	Name   string
	Digest string

	// Capabilities are provider-specific capability strings, e.g. "vision".
	Capabilities []string

	// Loaded reports whether the weights are in memory right now.
	//
	// Needed to tell "not loaded" from "loaded with no VRAM", which look identical in the
	// sizes alone: a cold model reports its on-disk size and a VRAM footprint of zero, which
	// is exactly what a model running wholly on the CPU also reports. Conflating them made
	// `doctor` warn that a perfectly healthy GPU setup was running on the CPU.
	Loaded bool

	// Size and SizeVRAM are bytes, reported where the provider hosts the weights itself. A
	// SizeVRAM below Size means part of the model is on the CPU and inference will be slow.
	// Both are meaningful only when Loaded is true.
	Size     int64
	SizeVRAM int64
}

// HasCapability reports whether the model advertises a capability.
func (m ModelInfo) HasCapability(name string) bool {
	for _, c := range m.Capabilities {
		if c == name {
			return true
		}
	}

	return false
}

// Progress reports a model download.
type Progress struct {
	Status    string
	Completed int64
	Total     int64
}

// ModelManager is implemented by backends that host their own weights and can therefore report
// on them, or fetch them. Cloud providers do not implement it, so callers type-assert:
//
//	if m, ok := backend.(infer.ModelManager); ok { … }
//
// Asserting rather than widening Completer keeps the cloud backends from having to stub four
// methods they can never implement meaningfully.
type ModelManager interface {
	// Health reports whether the server is reachable.
	Health(ctx context.Context) (ServerInfo, error)

	// ModelInfo describes an already-present model.
	ModelInfo(ctx context.Context, name string) (ModelInfo, error)

	// EnsureModel fetches the model if it is missing. progress may be nil.
	EnsureModel(ctx context.Context, name string, progress func(Progress)) error
}
