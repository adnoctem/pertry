// Package openaibackend implements both infer capabilities against OpenAI: completions via the
// Responses API, and bounded questions via the Decisions API.
//
// The two are separate endpoints answering different kinds of question, which is why
// pkg/infer splits Completer from Decider rather than pretending one interface covers both.
// Decisions cannot emit free text at all; Responses can, but has no notion of a calibrated
// probability over a closed option set. A caller that wants both asks for both.
package openaibackend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	"github.com/adnoctem/pertry/pkg/infer"
)

// DefaultDecisionModel is the only model the Decisions API currently accepts.
const DefaultDecisionModel = "gpt-6-luna"

// Backend talks to OpenAI.
type Backend struct {
	client openai.Client
}

// New builds a Backend. Credentials come from the SDK's own resolution, which reads
// OPENAI_API_KEY.
func New(opts ...option.RequestOption) *Backend {
	return &Backend{client: openai.NewClient(opts...)}
}

// Name identifies the backend.
func (b *Backend) Name() string { return "openai" }

// HasCredentialsEnv reports whether an API key is in the environment.
func HasCredentialsEnv() bool {
	return os.Getenv("OPENAI_API_KEY") != ""
}

// Complete sends one request to the Responses API, constrained to the schema.
func (b *Backend) Complete(ctx context.Context, req infer.Request) (infer.Response, error) {
	content := responses.ResponseInputMessageContentListParam{
		{OfInputText: &responses.ResponseInputTextParam{Text: req.Prompt}},
	}

	for i, img := range req.Images {
		if err := validImage(img); err != nil {
			return infer.Response{}, fmt.Errorf("image %d: %w", i, err)
		}

		content = append(content, responses.ResponseInputContentUnionParam{
			OfInputImage: &responses.ResponseInputImageParam{
				ImageURL: openai.String(dataURL(img)),
				Detail:   responses.ResponseInputImageDetailAuto,
			},
		})
	}

	params := responses.ResponseNewParams{
		// shared.ResponsesModel is an alias for string, so the plain id passes through.
		Model: req.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responses.ResponseInputParam{
				{OfMessage: &responses.EasyInputMessageParam{
					Role:    responses.EasyInputMessageRoleUser,
					Content: responses.EasyInputMessageContentUnionParam{OfInputItemContentList: content},
				}},
			},
		},
	}

	if req.System != "" {
		params.Instructions = openai.String(req.System)
	}

	if req.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(req.MaxTokens)
	}

	if req.Schema != nil {
		// Structured outputs: the response is constrained to the schema at decode time.
		params.Text = responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
					Name:   "result",
					Schema: req.Schema,
					Strict: openai.Bool(true),
				},
			},
		}
	}

	resp, err := b.client.Responses.New(ctx, params)
	if err != nil {
		return infer.Response{}, fmt.Errorf("openai responses %s: %w", req.Model, err)
	}

	text := strings.TrimSpace(resp.OutputText())
	if text == "" {
		return infer.Response{}, fmt.Errorf("openai responses %s returned no text output", req.Model)
	}

	if !json.Valid([]byte(text)) {
		return infer.Response{}, fmt.Errorf("openai responses %s returned text that is not JSON: %.200s",
			req.Model, text)
	}

	return infer.Response{
		JSON:  json.RawMessage(text),
		Model: resp.Model,
		Usage: infer.Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}, nil
}

// Decide answers bounded questions via the Decisions API.
//
// This is the capability the other backends cannot offer honestly. The answers carry calibrated
// probabilities over the option set, which is a materially better confidence signal than a model's
// self-reported score in a JSON field, and better than reconstructing one from token logprobs.
func (b *Backend) Decide(ctx context.Context, req infer.DecisionRequest) ([]infer.Answer, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	model := req.Model
	if model == "" {
		model = DefaultDecisionModel
	}

	input, err := decisionInput(req)
	if err != nil {
		return nil, err
	}

	questions := make([]openai.DecisionNewParamsQuestionUnion, 0, len(req.Questions))

	for _, q := range req.Questions {
		converted, err := question(q)
		if err != nil {
			return nil, err
		}

		questions = append(questions, converted)
	}

	decision, err := b.client.Decisions.New(ctx, openai.DecisionNewParams{
		Model:     model,
		Input:     input,
		Questions: questions,
	})
	if err != nil {
		return nil, fmt.Errorf("openai decisions %s: %w", model, err)
	}

	return answers(decision, model)
}

// decisionInput builds the shared evidence, as a plain string when there are no images and as a
// single user message otherwise.
func decisionInput(req infer.DecisionRequest) (openai.DecisionNewParamsInputUnion, error) {
	if len(req.Images) == 0 {
		return openai.DecisionNewParamsInputUnion{OfString: openai.String(req.Evidence)}, nil
	}

	// At most 128 images across all messages in one request, per the API.
	const maxImages = 128
	if len(req.Images) > maxImages {
		return openai.DecisionNewParamsInputUnion{}, fmt.Errorf(
			"%d images exceeds the Decisions API limit of %d: %w", len(req.Images), maxImages, infer.ErrUnsupported)
	}

	parts := make([]openai.DecisionInputPartUnionParam, 0, len(req.Images)+1)

	if req.Evidence != "" {
		parts = append(parts, openai.DecisionInputPartUnionParam{
			OfInputText: &openai.DecisionInputTextParam{Text: req.Evidence},
		})
	}

	for i, img := range req.Images {
		if err := validImage(img); err != nil {
			return openai.DecisionNewParamsInputUnion{}, fmt.Errorf("image %d: %w", i, err)
		}

		parts = append(parts, openai.DecisionInputPartUnionParam{
			OfInputImage: &openai.DecisionInputImageParam{
				ImageURL: dataURL(img),
				Detail:   openai.DecisionInputImageDetailAuto,
			},
		})
	}

	return openai.DecisionNewParamsInputUnion{
		OfDecisionInputMessageArray: []openai.DecisionInputMessageParam{{
			Content: openai.DecisionInputMessageContentUnionParam{OfParts: parts},
		}},
	}, nil
}

// question converts one question to the SDK's union.
func question(q infer.Question) (openai.DecisionNewParamsQuestionUnion, error) {
	switch q.Kind {
	case infer.KindPredicate:
		return openai.DecisionNewParamsQuestionUnion{
			OfPredicate: &openai.DecisionNewParamsQuestionPredicate{
				Name:         openai.String(q.Key),
				Instructions: q.Instructions,
			},
		}, nil

	case infer.KindChoice:
		choices := make([]openai.DecisionNewParamsQuestionChoiceChoice, 0, len(q.Choices))
		for _, c := range q.Choices {
			choices = append(choices, openai.DecisionNewParamsQuestionChoiceChoice{
				Value: openai.DecisionNewParamsQuestionChoiceChoiceValueUnion{OfString: openai.String(c)},
			})
		}

		return openai.DecisionNewParamsQuestionUnion{
			OfChoice: &openai.DecisionNewParamsQuestionChoice{
				Name:         openai.String(q.Key),
				Instructions: q.Instructions,
				Choices:      choices,
			},
		}, nil

	case infer.KindScore:
		levels := make([]openai.DecisionNewParamsQuestionScoreLevel, 0, len(q.Levels))
		for _, l := range q.Levels {
			levels = append(levels, openai.DecisionNewParamsQuestionScoreLevel{Label: l})
		}

		return openai.DecisionNewParamsQuestionUnion{
			OfScore: &openai.DecisionNewParamsQuestionScore{
				Name:         openai.String(q.Key),
				Instructions: q.Instructions,
				Levels:       levels,
			},
		}, nil

	default:
		return openai.DecisionNewParamsQuestionUnion{},
			fmt.Errorf("question kind %q: %w", q.Kind, infer.ErrUnsupported)
	}
}

// answers converts the API's answer union into infer.Answer values.
func answers(decision *openai.Decision, model string) ([]infer.Answer, error) {
	out := make([]infer.Answer, 0, len(decision.Answers))

	for _, a := range decision.Answers {
		switch v := a.AsAny().(type) {
		case openai.DecisionAnswerPredicate:
			// A predicate answers with the probability the condition holds, so the
			// value is the side of the coin that won and the probability is restated
			// relative to it.
			value, probability := "true", v.Probability
			if probability < 0.5 {
				value, probability = "false", 1-probability
			}

			out = append(out, infer.Answer{
				Key:         v.Name,
				Value:       value,
				Probability: probability,
				Alternatives: map[string]float64{
					"true":  v.Probability,
					"false": 1 - v.Probability,
				},
			})

		case openai.DecisionAnswerChoice:
			answer := infer.Answer{
				Key:          v.Name,
				Value:        choiceValue(v.Choice),
				Probability:  v.Confidence,
				Alternatives: make(map[string]float64, len(v.Probabilities)),
			}

			for _, p := range v.Probabilities {
				answer.Alternatives[probabilityValue(p)] = p.Probability
			}

			out = append(out, answer)

		case openai.DecisionAnswerScore:
			answer := infer.Answer{
				Key:          v.Name,
				Value:        fmt.Sprintf("%g", v.Score),
				Probability:  v.Confidence,
				Alternatives: make(map[string]float64, len(v.Probabilities)),
			}

			for _, p := range v.Probabilities {
				answer.Alternatives[p.Label] = p.Probability
			}

			out = append(out, answer)

		case openai.DecisionAnswerRefusal:
			// Refused questions are an error rather than a zero-probability answer: a
			// caller treating a refusal as "the condition is false" would be wrong, and
			// silently so.
			return nil, fmt.Errorf("openai decisions %s refused question %q", model, v.Name)

		default:
			return nil, fmt.Errorf("openai decisions %s returned an answer of unknown type for %q",
				model, a.Name)
		}
	}

	return out, nil
}

func choiceValue(u openai.DecisionAnswerChoiceChoiceUnion) string {
	if u.OfString != "" {
		return u.OfString
	}

	// Choice values are typed, and a boolean choice is distinct from the string with the same
	// text, so a bool has to be rendered rather than left empty.
	return strconv.FormatBool(u.OfBool)
}

func probabilityValue(p openai.DecisionAnswerChoiceProbability) string {
	if p.Value.OfString != "" {
		return p.Value.OfString
	}

	return strconv.FormatBool(p.Value.OfBool)
}

// validImage rejects an image the API cannot accept, with a message naming the field.
func validImage(img infer.Image) error {
	if img.MediaType == "" {
		return fmt.Errorf("no MediaType: %w", infer.ErrUnsupported)
	}

	if len(img.Data) == 0 {
		return fmt.Errorf("no Data: %w", infer.ErrUnsupported)
	}

	return nil
}

// dataURL encodes an image as the base64 data URL both endpoints accept.
func dataURL(img infer.Image) string {
	return "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

var (
	_ infer.Completer = (*Backend)(nil)
	_ infer.Decider   = (*Backend)(nil)
)
