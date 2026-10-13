// Package confidence derives a confidence score for a model's decision from token log
// probabilities (docs/ARCHITECTURE.md#confidence).
//
// # What the M2 spike found, and why this is not what docs/ARCHITECTURE.md#confidence first described
//
// docs/ARCHITECTURE.md#confidence specified `confidence = exp(Σ logprob of the value's tokens)`. Measured against Ollama
// 0.40.3, that is right in intent and wrong in two details, both of which matter enough to change
// the implementation.
//
// **Logprobs are reported before the grammar mask, not after.** A request constrained by a schema
// produced this at the position where the JSON object must open:
//
//	token '{"'  logprob -15.80   alternatives: '\n\n' (-0.00), '{"' (-15.80), '\n\n\n' (-16.39)
//
// The model overwhelmingly wanted to emit a newline; the grammar forced `{"` anyway, and the
// reported number is the model's own un-masked opinion. So any sum that includes structural tokens
// measures how surprised the model was by JSON syntax, not how sure it is of the answer -
// exp(-15.80) is 1.4e-7, which would report near-zero confidence for a perfectly correct answer.
//
// The consequence is that **only the value's own tokens may be scored**, which docs/ARCHITECTURE.md#confidence did say, and
// that the structural tokens around it must be excluded rather than merely not mentioned.
//
// **The token stream includes the model's reasoning.** qwen3-vl emits a thinking block; Ollama
// separates it into `message.thinking` and leaves `message.content` as the JSON, but the logprob
// array covers *everything generated*, reasoning included. In one trivial request, 56 of 61 tokens
// were reasoning. Aligning a position in the JSON to a position in the token array therefore means
// locating the JSON inside the stream first, which Align does.
//
// # Renormalising over what the grammar allowed
//
// Because the reported distribution is un-masked, the raw probability of a value token understates
// the model's confidence *given the choice it actually had*. At an enum position the grammar admits
// only the enum's members, so the honest reading is the value's probability renormalised over the
// alternatives that were legal. That is computable here precisely because the enum is known - the
// schema was generated from it (docs/ARCHITECTURE.md#model-stages) - which is the one piece of information the server did not
// have to give us.
package confidence

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/adnoctem/pertry/pkg/infer"
)

// Alternative is a value the model might have chosen instead, with its renormalised probability.
type Alternative struct {
	Value string `json:"value"`

	// Probability is renormalised over the legal alternatives, so a set of them sums to
	// roughly one rather than to an arbitrary fraction.
	Probability float64 `json:"probability"`
}

// Score is a decision's confidence and what else the model considered.
type Score struct {
	// Value is the decided value, as it appeared in the JSON.
	Value string

	// Confidence is in [0, 1]. Zero with Derived false means no score could be derived.
	Confidence float64

	// Derived reports whether a score was actually computed. A caller must not treat an
	// absent score as "zero confidence": they mean entirely different things, and storing the
	// first as the second would send every result to the review queue.
	Derived bool

	// Alternatives are the other legal values the model weighed, most likely first.
	Alternatives []Alternative

	// Raw is the un-renormalised probability of the value's tokens, kept because it is what
	// docs/ARCHITECTURE.md#confidence originally specified and because the gap between the two is the measure of how
	// much the grammar was doing.
	Raw float64

	// Tokens is how many tokens the value spanned.
	Tokens int
}

// Options configures derivation.
type Options struct {
	// Facet is the JSON property holding the decision.
	Facet string

	// Allowed is the legal value set, from the generated schema. When empty the score is not
	// renormalised, because there is nothing to renormalise over - which is the open-mode
	// case, where the decision is free text.
	Allowed []string
}

// Derive computes a confidence score for one decision field of a response.
//
// Returns a Score with Derived false, and no error, when the backend reported no logprobs: that is
// the ordinary case for a server built without them, and docs/ARCHITECTURE.md#confidence's agreement-based fallback exists
// for exactly that situation.
func Derive(resp infer.Response, opts Options) (Score, error) {
	var out Score

	content := string(resp.JSON)

	value, start, end, err := locateValue(content, opts.Facet)
	if err != nil {
		return out, err
	}

	out.Value = value

	if len(resp.Logprobs) == 0 {
		return out, nil
	}

	span, ok := Align(resp.Logprobs, content, start, end)
	if !ok {
		// The tokens could not be aligned to the JSON. Reported as "no score" rather than
		// as a low one, for the reason given on Derived.
		return out, nil
	}

	out.Tokens = len(span.Indices)

	sum := 0.0
	for _, i := range span.Indices {
		sum += resp.Logprobs[i].Logprob
	}

	out.Raw = math.Exp(sum)

	// With no legal set to renormalise over, the raw figure is all there is. It is reported,
	// with the caveat that it is un-masked, because for a free-text decision there is no
	// better number available.
	if len(opts.Allowed) == 0 {
		out.Confidence = out.Raw
		out.Derived = true

		return out, nil
	}

	first := resp.Logprobs[span.Indices[0]]

	out.Confidence, out.Alternatives = renormalise(first, value, opts.Allowed)
	out.Derived = true

	return out, nil
}

// renormalise computes the value's probability over the alternatives the grammar allowed.
//
// The reported distribution is un-masked, so it includes tokens the grammar would have rejected.
// Those are dropped and the rest renormalised, which is what the server would have reported had
// the logprobs been post-mask.
func renormalise(token infer.TokenLogprob, value string, allowed []string) (float64, []Alternative) {
	// Each candidate is matched by whether the emitted token could begin it. A tokeniser
	// splits "screenshot" into pieces that are nobody's business, so the comparison is on a
	// prefix basis after stripping JSON string punctuation.
	type candidate struct {
		value string
		prob  float64
	}

	var (
		candidates []candidate
		total      float64
	)

	seen := map[string]bool{}

	consider := func(tok string, logprob float64) {
		cleaned := cleanToken(tok)
		if cleaned == "" {
			return
		}

		for _, a := range allowed {
			if seen[a] {
				continue
			}

			if !strings.HasPrefix(strings.ToLower(a), strings.ToLower(cleaned)) {
				continue
			}

			p := math.Exp(logprob)

			candidates = append(candidates, candidate{value: a, prob: p})

			total += p
			seen[a] = true

			break
		}
	}

	consider(token.Token, token.Logprob)

	for _, alt := range token.Top {
		consider(alt.Token, alt.Logprob)
	}

	if total <= 0 {
		return 0, nil
	}

	var (
		score        float64
		alternatives []Alternative
	)

	for _, c := range candidates {
		normalised := c.prob / total

		if c.value == value {
			score = normalised

			continue
		}

		alternatives = append(alternatives, Alternative{Value: c.value, Probability: normalised})
	}

	// Most likely first, so a review UI can offer the best alternative without sorting.
	for i := 1; i < len(alternatives); i++ {
		for j := i; j > 0 && alternatives[j].Probability > alternatives[j-1].Probability; j-- {
			alternatives[j], alternatives[j-1] = alternatives[j-1], alternatives[j]
		}
	}

	return score, alternatives
}

// cleanToken strips the JSON punctuation a tokeniser tends to glue onto a value's first piece, so
// that `"screen` and `screen` compare alike.
func cleanToken(tok string) string {
	return strings.TrimSpace(strings.Trim(tok, "\"' \t\n\r{}[],:"))
}

// Span is a run of tokens covering a region of the generated text.
type Span struct {
	// Indices are positions in the logprob array, in order.
	Indices []int
}

// Align maps a character range of the JSON content onto the tokens that produced it.
//
// Needed because the logprob array covers everything the model generated, reasoning included,
// while the content is only the JSON. The JSON is located as the last occurrence in the
// reconstructed stream: a thinking block frequently quotes the answer it is about to give - the
// spike saw exactly that - so the first occurrence is the wrong one.
func Align(tokens []infer.TokenLogprob, content string, start, end int) (Span, bool) {
	if start < 0 || end <= start || end > len(content) {
		return Span{}, false
	}

	// Reconstruct the generated text, remembering where each token begins.
	var (
		builder strings.Builder
		offsets = make([]int, 0, len(tokens))
	)

	for _, t := range tokens {
		offsets = append(offsets, builder.Len())
		builder.WriteString(t.Token)
	}

	full := builder.String()

	base := strings.LastIndex(full, content)
	if base < 0 {
		// The content is not reproducible from the tokens. Ollama normalises whitespace in
		// `content`, so try the trimmed form before giving up.
		trimmed := strings.TrimSpace(content)

		base = strings.LastIndex(full, trimmed)
		if base < 0 {
			return Span{}, false
		}
	}

	wantStart := base + start
	wantEnd := base + end

	var span Span

	for i, offset := range offsets {
		tokenEnd := offset + len(tokens[i].Token)

		// Any token overlapping the range contributed to the value.
		if tokenEnd > wantStart && offset < wantEnd {
			span.Indices = append(span.Indices, i)
		}
	}

	if len(span.Indices) == 0 {
		return Span{}, false
	}

	return span, true
}

// locateValue finds a string property's value in a JSON document and returns it with the character
// range of its contents, excluding the surrounding quotes.
//
// Uses a decoder with offsets rather than a regular expression, so that a value containing a brace
// or an escaped quote cannot throw the search off.
func locateValue(content, facet string) (value string, start, end int, err error) {
	decoder := json.NewDecoder(strings.NewReader(content))

	// Expect the opening brace.
	if _, err := decoder.Token(); err != nil {
		return "", 0, 0, fmt.Errorf("read JSON: %w", err)
	}

	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", 0, 0, fmt.Errorf("read JSON key: %w", err)
		}

		key, _ := keyToken.(string)

		// InputOffset after reading the key is just before the value; after reading the
		// value it is just after it. The quotes are stripped by taking one character in
		// from each end, which is exact for a string value.
		before := decoder.InputOffset()

		valueToken, err := decoder.Token()
		if err != nil {
			return "", 0, 0, fmt.Errorf("read JSON value for %q: %w", key, err)
		}

		after := decoder.InputOffset()

		if key != facet {
			// Skip over a nested object or array wholesale.
			if delim, ok := valueToken.(json.Delim); ok && (delim == '{' || delim == '[') {
				if err := skipNested(decoder); err != nil {
					return "", 0, 0, err
				}
			}

			continue
		}

		text, ok := valueToken.(string)
		if !ok {
			return "", 0, 0, fmt.Errorf("property %q is %T, not a string", facet, valueToken)
		}

		// Find the value's quotes within the consumed range, which is robust to the
		// whitespace the decoder skipped.
		region := content[before:after]

		open := strings.Index(region, `"`)
		closeAt := strings.LastIndex(region, `"`)

		if open < 0 || closeAt <= open {
			return "", 0, 0, fmt.Errorf("could not locate the quotes around %q", facet)
		}

		return text, int(before) + open + 1, int(before) + closeAt, nil
	}

	return "", 0, 0, fmt.Errorf("property %q is not in the response", facet)
}

// skipNested consumes the remainder of an object or array the decoder has just opened.
func skipNested(decoder *json.Decoder) error {
	depth := 1

	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("skip nested value: %w", err)
		}

		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}

	return nil
}
