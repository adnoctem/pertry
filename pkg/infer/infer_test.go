package infer_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/pkg/infer"
)

func TestQuestionValidate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		question infer.Question
		wantErr  string
	}{
		"predicate": {
			question: infer.Question{Key: "k", Kind: infer.KindPredicate},
		},
		"choice": {
			question: infer.Question{Key: "k", Kind: infer.KindChoice, Choices: []string{"a", "b"}},
		},
		"score": {
			question: infer.Question{Key: "k", Kind: infer.KindScore, Levels: []string{"low", "high"}},
		},
		"no key": {
			question: infer.Question{Kind: infer.KindPredicate},
			wantErr:  "no key",
		},
		"unknown kind": {
			question: infer.Question{Key: "k", Kind: "vibes"},
			wantErr:  "unknown kind",
		},
		// A one-option choice is not a choice. Catching it here beats a provider rejecting
		// it after the request has been built and billed.
		"choice with one option": {
			question: infer.Question{Key: "k", Kind: infer.KindChoice, Choices: []string{"only"}},
			wantErr:  "at least 2",
		},
		"score with one level": {
			question: infer.Question{Key: "k", Kind: infer.KindScore, Levels: []string{"only"}},
			wantErr:  "at least 2",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tc.question.Validate()

			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() error = %v, want nil", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("Validate() error = nil, want one mentioning %q", tc.wantErr)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestDecisionRequestValidate(t *testing.T) {
	t.Parallel()

	ok := infer.Question{Key: "a", Kind: infer.KindPredicate}

	t.Run("no questions", func(t *testing.T) {
		t.Parallel()

		if err := (infer.DecisionRequest{}).Validate(); err == nil {
			t.Error("Validate() error = nil, want one for a request with no questions")
		}
	})

	// Keys are how an answer is matched back to its question, so a duplicate makes the
	// response ambiguous rather than merely untidy.
	t.Run("duplicate key", func(t *testing.T) {
		t.Parallel()

		req := infer.DecisionRequest{Questions: []infer.Question{ok, ok}}

		err := req.Validate()
		if err == nil {
			t.Fatal("Validate() error = nil, want one for a duplicate key")
		}

		if !strings.Contains(err.Error(), "twice") {
			t.Errorf("Validate() error = %q, want it to say the key appears twice", err)
		}
	})

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		req := infer.DecisionRequest{Questions: []infer.Question{
			ok,
			{Key: "b", Kind: infer.KindChoice, Choices: []string{"x", "y"}},
		}}

		if err := req.Validate(); err != nil {
			t.Errorf("Validate() error = %v, want nil", err)
		}
	})
}

func TestModelInfoHasCapability(t *testing.T) {
	t.Parallel()

	m := infer.ModelInfo{Capabilities: []string{"completion", "vision"}}

	if !m.HasCapability("vision") {
		t.Error(`HasCapability("vision") = false, want true`)
	}

	if m.HasCapability("tools") {
		t.Error(`HasCapability("tools") = true, want false`)
	}
}

func TestFakeComplete(t *testing.T) {
	t.Parallel()

	t.Run("returns the fixture and records the request", func(t *testing.T) {
		t.Parallel()

		fake := &infer.Fake{Fixture: json.RawMessage(`{"ok":true}`)}

		resp, err := fake.Complete(context.Background(), infer.Request{Model: "m", Prompt: "p"})
		if err != nil {
			t.Fatalf("Complete() error = %v, want nil", err)
		}

		if diff := cmp.Diff(`{"ok":true}`, string(resp.JSON)); diff != "" {
			t.Errorf("JSON mismatch (-want +got):\n%s", diff)
		}

		requests := fake.Requests()
		if len(requests) != 1 || requests[0].Prompt != "p" {
			t.Errorf("Requests() = %+v, want the one request that was sent", requests)
		}
	})

	t.Run("CompleteFunc wins over Fixture", func(t *testing.T) {
		t.Parallel()

		fake := &infer.Fake{
			Fixture:      json.RawMessage(`{"from":"fixture"}`),
			CompleteFunc: func(infer.Request) (infer.Response, error) { return infer.Response{}, errors.New("boom") },
		}

		if _, err := fake.Complete(context.Background(), infer.Request{}); err == nil {
			t.Error("Complete() error = nil, want the error from CompleteFunc")
		}
	})

	// A Fake with neither configured is a programming mistake in the test, not a model
	// failure, so it says so rather than returning empty JSON that fails later and elsewhere.
	t.Run("neither configured is an error", func(t *testing.T) {
		t.Parallel()

		_, err := (&infer.Fake{}).Complete(context.Background(), infer.Request{})
		if !errors.Is(err, infer.ErrUnsupported) {
			t.Errorf("Complete() error = %v, want it to wrap ErrUnsupported", err)
		}
	})
}

func TestFakeDecide(t *testing.T) {
	t.Parallel()

	question := infer.Question{Key: "kind", Kind: infer.KindChoice, Choices: []string{"screenshot", "photo"}}

	t.Run("returns the configured answers", func(t *testing.T) {
		t.Parallel()

		fake := &infer.Fake{Answers: []infer.Answer{{Key: "kind", Value: "screenshot", Probability: 0.9}}}

		answers, err := fake.Decide(context.Background(), infer.DecisionRequest{
			Evidence:  "a screenshot",
			Questions: []infer.Question{question},
		})
		if err != nil {
			t.Fatalf("Decide() error = %v, want nil", err)
		}

		if len(answers) != 1 || answers[0].Value != "screenshot" {
			t.Errorf("Decide() = %+v, want the configured answer", answers)
		}

		if len(fake.Decisions()) != 1 {
			t.Errorf("Decisions() = %d, want 1", len(fake.Decisions()))
		}
	})

	// Validation happens before the fixture is returned, so a test cannot accidentally
	// assert against a request a real provider would have rejected.
	t.Run("validates the request", func(t *testing.T) {
		t.Parallel()

		if _, err := (&infer.Fake{}).Decide(context.Background(), infer.DecisionRequest{}); err == nil {
			t.Error("Decide() error = nil, want a validation error")
		}
	})
}

// TestFakeSatisfiesBothCapabilities is the compile-time contract made explicit: pkg/infer splits
// Completer from Decider because the providers differ in kind, and the fake has to stand in for
// either.
func TestFakeSatisfiesBothCapabilities(t *testing.T) {
	t.Parallel()

	var (
		completer infer.Completer = &infer.Fake{}
		decider   infer.Decider   = &infer.Fake{}
	)

	if completer.Name() != "fake" || decider.Name() != "fake" {
		t.Errorf("Name() = %q and %q, want \"fake\" for both", completer.Name(), decider.Name())
	}
}
