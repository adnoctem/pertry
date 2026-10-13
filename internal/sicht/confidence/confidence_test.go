package confidence_test

import (
	"math"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/confidence"
	"github.com/adnoctem/pertry/pkg/infer"
)

// tok builds a token with alternatives.
func tok(text string, logprob float64, alts ...infer.Alternative) infer.TokenLogprob {
	return infer.TokenLogprob{Token: text, Logprob: logprob, Top: alts}
}

func alt(text string, logprob float64) infer.Alternative {
	return infer.Alternative{Token: text, Logprob: logprob}
}

// TestDeriveExcludesStructuralTokens is the finding that changed docs/ARCHITECTURE.md#confidence. Logprobs are reported
// before the grammar mask, so a grammar-forced structural token carries the model's own
// astonishment at JSON syntax - the spike measured -15.80 for an opening brace. Summing it in
// would report near-zero confidence for a perfectly correct answer.
func TestDeriveExcludesStructuralTokens(t *testing.T) {
	t.Parallel()

	content := `{"kind":"photo"}`

	// A realistic stream: the brace is grammar-forced and wildly improbable, the value is
	// confident.
	resp := infer.Response{
		JSON: []byte(content),
		Logprobs: []infer.TokenLogprob{
			tok(`{"`, -15.80, alt("\n\n", 0), alt(`{"`, -15.80)),
			tok(`kind`, -0.0001),
			tok(`":"`, -0.0002),
			tok(`photo`, -0.05, alt("photo", -0.05), alt("screenshot", -3.2)),
			tok(`"}`, -0.001),
		},
	}

	score, err := confidence.Derive(resp, confidence.Options{
		Facet:   "kind",
		Allowed: []string{"screenshot", "photo", "scan_document", "other"},
	})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}

	if !score.Derived {
		t.Fatal("Derived = false, want a score")
	}

	if score.Value != "photo" {
		t.Errorf("Value = %q, want %q", score.Value, "photo")
	}

	// Only the value token was scored: one token, not five.
	if score.Tokens != 1 {
		t.Errorf("Tokens = %d, want 1 - only the value's own tokens may be scored", score.Tokens)
	}

	// Had the structural tokens been included, the raw figure would be about 1e-7.
	if score.Raw < 0.9 {
		t.Errorf("Raw = %g, want about 0.95: the opening brace's -15.80 must not be in the sum",
			score.Raw)
	}

	if score.Confidence < 0.9 {
		t.Errorf("Confidence = %g, want high", score.Confidence)
	}
}

// TestRenormaliseOverLegalValues is the other half of the pre-mask consequence. The reported
// distribution includes tokens the grammar would have rejected, so the value's probability
// understates the model's confidence given the choice it actually had.
func TestRenormaliseOverLegalValues(t *testing.T) {
	t.Parallel()

	// The model's raw distribution puts most of its mass on a newline, which the grammar
	// forbids at this position. Among the legal values it prefers "photo" two to one.
	resp := infer.Response{
		JSON: []byte(`{"kind":"photo"}`),
		Logprobs: []infer.TokenLogprob{
			tok(`{"kind":"`, -0.001),
			tok(`photo`, math.Log(0.02),
				alt("\n\n", math.Log(0.90)),       // illegal: dropped
				alt("photo", math.Log(0.02)),      // legal
				alt("screenshot", math.Log(0.01)), // legal
				alt("   ", math.Log(0.07)),        // illegal: dropped
			),
			tok(`"}`, -0.001),
		},
	}

	score, err := confidence.Derive(resp, confidence.Options{
		Facet:   "kind",
		Allowed: []string{"screenshot", "photo", "scan_document", "other"},
	})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}

	// Raw is 0.02; renormalised over the two legal options it is 0.02/0.03 = 0.667.
	if math.Abs(score.Raw-0.02) > 0.005 {
		t.Errorf("Raw = %g, want about 0.02", score.Raw)
	}

	if math.Abs(score.Confidence-2.0/3.0) > 0.02 {
		t.Errorf("Confidence = %g, want about 0.667 - renormalised over the legal values only",
			score.Confidence)
	}

	if len(score.Alternatives) != 1 || score.Alternatives[0].Value != "screenshot" {
		t.Fatalf("Alternatives = %+v, want screenshot", score.Alternatives)
	}

	if math.Abs(score.Alternatives[0].Probability-1.0/3.0) > 0.02 {
		t.Errorf("alternative probability = %g, want about 0.333",
			score.Alternatives[0].Probability)
	}
}

// TestAlignSkipsTheThinkingBlock is the second spike finding: the logprob array covers everything
// generated, and qwen3-vl spends most of its tokens reasoning. In one trivial request 56 of 61
// tokens were reasoning.
func TestAlignSkipsTheThinkingBlock(t *testing.T) {
	t.Parallel()

	// The reasoning quotes the answer it is about to give, which the spike observed - so the
	// first occurrence of the JSON in the stream is the wrong one.
	resp := infer.Response{
		JSON: []byte(`{"kind":"photo"}`),
		Logprobs: []infer.TokenLogprob{
			tok(`We`, -0.4),
			tok(` should answer `, -0.1),
			tok(`{"kind":"photo"}`, -0.2), // quoted inside the reasoning
			tok(` because it has a horizon.`, -0.1),
			tok(`</think>`, -0.2),
			tok(`{"`, -15.8),
			tok(`kind`, 0),
			tok(`":"`, 0),
			tok(`photo`, -0.03, alt("photo", -0.03), alt("screenshot", -4.0)),
			tok(`"}`, 0),
		},
	}

	score, err := confidence.Derive(resp, confidence.Options{
		Facet:   "kind",
		Allowed: []string{"screenshot", "photo"},
	})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}

	if !score.Derived {
		t.Fatal("Derived = false, want a score")
	}

	// The real value token, not the one quoted in the reasoning: its logprob is -0.03, so
	// the raw probability is about 0.97. Had the reasoning's copy been used, it would be
	// about 0.82.
	if score.Raw < 0.95 {
		t.Errorf("Raw = %g, want the final occurrence (about 0.97), not the quoted one", score.Raw)
	}

	if score.Tokens != 1 {
		t.Errorf("Tokens = %d, want 1", score.Tokens)
	}
}

// TestNoLogprobsIsNotZeroConfidence is a distinction that decides whether every result lands in
// the review queue. "No score available" and "the model was unsure" are entirely different, and
// docs/ARCHITECTURE.md#confidence's fallback exists for the first.
func TestNoLogprobsIsNotZeroConfidence(t *testing.T) {
	t.Parallel()

	score, err := confidence.Derive(infer.Response{
		JSON: []byte(`{"kind":"photo"}`),
	}, confidence.Options{Facet: "kind", Allowed: []string{"photo"}})
	if err != nil {
		t.Fatalf("Derive() error = %v, want nil when the backend reported no logprobs", err)
	}

	if score.Derived {
		t.Error("Derived = true with no logprobs, want false")
	}

	if score.Value != "photo" {
		t.Errorf("Value = %q, want the value to be read even without a score", score.Value)
	}
}

// TestMultiTokenValue covers a value a tokeniser split, which is the normal case for anything
// longer than a short word.
func TestMultiTokenValue(t *testing.T) {
	t.Parallel()

	resp := infer.Response{
		JSON: []byte(`{"kind":"scan_document"}`),
		Logprobs: []infer.TokenLogprob{
			tok(`{"kind":"`, -0.001),
			tok(`scan`, math.Log(0.9), alt("scan", math.Log(0.9)), alt("photo", math.Log(0.1))),
			tok(`_`, math.Log(0.99)),
			tok(`document`, math.Log(0.98)),
			tok(`"}`, -0.001),
		},
	}

	score, err := confidence.Derive(resp, confidence.Options{
		Facet:   "kind",
		Allowed: []string{"screenshot", "photo", "scan_document"},
	})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}

	if score.Tokens != 3 {
		t.Errorf("Tokens = %d, want 3 - the value spans three tokens", score.Tokens)
	}

	// Raw is the product over the value's tokens: 0.9 * 0.99 * 0.98.
	if math.Abs(score.Raw-0.873) > 0.01 {
		t.Errorf("Raw = %g, want about 0.873", score.Raw)
	}
}

// TestOpenModeHasNoLegalSet is the free-text decision: there is nothing to renormalise over, so
// the raw figure is reported with that understood.
func TestOpenModeHasNoLegalSet(t *testing.T) {
	t.Parallel()

	resp := infer.Response{
		JSON: []byte(`{"platform":"reddit"}`),
		Logprobs: []infer.TokenLogprob{
			tok(`{"platform":"`, -0.001),
			tok(`reddit`, math.Log(0.8)),
			tok(`"}`, -0.001),
		},
	}

	score, err := confidence.Derive(resp, confidence.Options{Facet: "platform"})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}

	if !score.Derived {
		t.Fatal("Derived = false, want the raw figure for a free-text decision")
	}

	if math.Abs(score.Confidence-0.8) > 0.01 {
		t.Errorf("Confidence = %g, want the raw 0.8", score.Confidence)
	}

	if len(score.Alternatives) != 0 {
		t.Errorf("Alternatives = %+v, want none without a legal set", score.Alternatives)
	}
}

func TestLocateValueWithAwkwardContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
		want string
	}{
		{
			name: "a brace inside an earlier string",
			json: `{"visible_ui":"a {weird} caption","kind":"photo"}`,
			want: "photo",
		},
		{
			name: "an escaped quote inside an earlier string",
			json: `{"visible_ui":"he said \"hi\"","kind":"screenshot"}`,
			want: "screenshot",
		},
		{
			name: "a nested object before the facet",
			json: `{"meta":{"a":1,"b":[1,2]},"kind":"other"}`,
			want: "other",
		},
		{
			name: "the facet first",
			json: `{"kind":"photo","visible_ui":"none"}`,
			want: "photo",
		},
		{
			name: "a null-valued field before it",
			json: `{"visible_url":null,"kind":"photo"}`,
			want: "photo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A single token covering the whole document, so alignment is trivial and the
			// test is about locating the value.
			resp := infer.Response{
				JSON:     []byte(tt.json),
				Logprobs: []infer.TokenLogprob{tok(tt.json, math.Log(0.5))},
			}

			score, err := confidence.Derive(resp, confidence.Options{Facet: "kind"})
			if err != nil {
				t.Fatalf("Derive() error = %v", err)
			}

			if score.Value != tt.want {
				t.Errorf("Value = %q, want %q", score.Value, tt.want)
			}
		})
	}
}

func TestDeriveErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		json  string
		facet string
	}{
		"missing facet": {`{"kind":"photo"}`, "platform"},
		"not a string":  {`{"kind":42}`, "kind"},
		"invalid JSON":  {`{"kind":`, "kind"},
		"not an object": {`["photo"]`, "kind"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := confidence.Derive(infer.Response{JSON: []byte(tt.json)},
				confidence.Options{Facet: tt.facet})
			if err == nil {
				t.Errorf("Derive() error = nil for %s", name)
			}
		})
	}
}

// TestAlignFailsCleanlyWhenTokensDoNotMatch: a stream that cannot be reconciled with the content
// yields no score rather than a wrong one.
func TestAlignFailsCleanlyWhenTokensDoNotMatch(t *testing.T) {
	t.Parallel()

	score, err := confidence.Derive(infer.Response{
		JSON:     []byte(`{"kind":"photo"}`),
		Logprobs: []infer.TokenLogprob{tok("something else entirely", -0.5)},
	}, confidence.Options{Facet: "kind", Allowed: []string{"photo"}})
	if err != nil {
		t.Fatalf("Derive() error = %v, want nil", err)
	}

	if score.Derived {
		t.Error("Derived = true for an unalignable stream, want false")
	}
}
