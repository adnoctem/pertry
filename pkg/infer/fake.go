package infer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Fake is an in-memory Completer and Decider for tests.
//
// docs/ARCHITECTURE.md#inference-backend-and-model-choice requires one ("Provide a `fake` backend for tests that returns
// deterministic results from a fixture map"), and bookscape's model pass needs the same thing for
// a blunter reason: a test that could reach a paid API by accident is a trap. Fake records what it
// was asked so a test can assert on batching and prompt construction, which is most of what there
// is to get wrong above the transport.
type Fake struct {
	// CompleteFunc answers Complete. When nil, Complete returns Fixture or an error.
	CompleteFunc func(req Request) (Response, error)

	// DecideFunc answers Decide. When nil, Decide returns Answers or an error.
	DecideFunc func(req DecisionRequest) ([]Answer, error)

	// Fixture is returned verbatim as the response JSON when CompleteFunc is nil.
	Fixture json.RawMessage

	// Answers are returned verbatim when DecideFunc is nil.
	Answers []Answer

	mu        sync.Mutex
	requests  []Request
	decisions []DecisionRequest
}

// Name identifies the backend.
func (f *Fake) Name() string { return "fake" }

// Complete records the request and answers from CompleteFunc or Fixture.
func (f *Fake) Complete(_ context.Context, req Request) (Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if f.CompleteFunc != nil {
		return f.CompleteFunc(req)
	}

	if f.Fixture == nil {
		return Response{}, fmt.Errorf("fake backend has neither CompleteFunc nor Fixture: %w", ErrUnsupported)
	}

	return Response{JSON: f.Fixture, Model: req.Model}, nil
}

// Decide records the request and answers from DecideFunc or Answers.
func (f *Fake) Decide(_ context.Context, req DecisionRequest) ([]Answer, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	f.decisions = append(f.decisions, req)
	f.mu.Unlock()

	if f.DecideFunc != nil {
		return f.DecideFunc(req)
	}

	return f.Answers, nil
}

// Requests returns the completion requests received so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]Request(nil), f.requests...)
}

// Decisions returns the decision requests received so far.
func (f *Fake) Decisions() []DecisionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]DecisionRequest(nil), f.decisions...)
}

// Assert at compile time that Fake satisfies both capabilities.
var (
	_ Completer = (*Fake)(nil)
	_ Decider   = (*Fake)(nil)
)
