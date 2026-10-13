package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// ValidationError describes why a response did not match its schema.
type ValidationError struct {
	// Field is the property at fault, empty for a document-level problem.
	Field string

	// Problem is what was wrong, phrased for a log line.
	Problem string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Problem
	}

	return fmt.Sprintf("%s: %s", e.Field, e.Problem)
}

// Validate checks a decoded response against the generated schema.
//
// docs/ARCHITECTURE.md#inference-backend-and-model-choice requires that every response be validated even though decoding was constrained, because
// "constrained" is a property of the server's implementation and not a guarantee the caller can
// verify. This validates against the generated Schema rather than by interpreting JSON Schema in
// general: the schema was built by Build from a stage definition, so the shapes that can occur are
// exactly the ones Build emits. That is narrower than a JSON Schema validator and strictly more
// accurate for the job, and it cannot drift from the generator, because it reads the same
// structure the generator produced rather than a re-parse of its output.
func Validate(s *Schema, payload map[string]any) error {
	if s == nil {
		return errors.New("no schema to validate against")
	}

	var problems []error

	known := make(map[string]struct{}, len(s.Properties))

	for _, p := range s.Properties {
		known[p.Name] = struct{}{}

		value, present := payload[p.Name]
		if !present {
			problems = append(problems, &ValidationError{
				Field: p.Name, Problem: "required property is missing",
			})

			continue
		}

		if err := validateProperty(p, value); err != nil {
			problems = append(problems, err)
		}
	}

	// additionalProperties is false in the generated schema, so anything else is a violation.
	// Reported rather than ignored: a model inventing a field is a sign the prompt and the
	// schema disagree, which is worth seeing.
	for name := range payload {
		if _, ok := known[name]; !ok {
			problems = append(problems, &ValidationError{
				Field: name, Problem: "property is not in the schema",
			})
		}
	}

	return errors.Join(problems...)
}

// definition is the subset of JSON Schema that Build emits.
type definition struct {
	// Type is a string or an array of strings, so it is decoded loosely.
	Type      json.RawMessage `json:"type"`
	MaxLength *int            `json:"maxLength"`
	Enum      []any           `json:"enum"`
}

func validateProperty(p Property, value any) error {
	var def definition

	if err := json.Unmarshal(p.Definition, &def); err != nil {
		return &ValidationError{Field: p.Name, Problem: fmt.Sprintf("schema is unreadable: %v", err)}
	}

	types, nullable, err := decodeTypes(def.Type)
	if err != nil {
		return &ValidationError{Field: p.Name, Problem: err.Error()}
	}

	if value == nil {
		if nullable {
			return nil
		}

		return &ValidationError{Field: p.Name, Problem: "is null, and the schema does not allow null"}
	}

	primary := ""
	for _, t := range types {
		if t != "null" {
			primary = t

			break
		}
	}

	switch primary {
	case "string":
		text, ok := value.(string)
		if !ok {
			return &ValidationError{
				Field: p.Name, Problem: fmt.Sprintf("is %T, want a string", value),
			}
		}

		// Counted in runes, because maxLength in JSON Schema is defined over characters and
		// a model asked for 200 characters of German should not be cut off at 140.
		if def.MaxLength != nil && utf8.RuneCountInString(text) > *def.MaxLength {
			return &ValidationError{
				Field: p.Name,
				Problem: fmt.Sprintf("is %d characters, and the schema allows %d",
					utf8.RuneCountInString(text), *def.MaxLength),
			}
		}

		if len(def.Enum) > 0 && !inEnum(def.Enum, text) {
			return &ValidationError{
				Field:   p.Name,
				Problem: fmt.Sprintf("is %q, which is not one of %s", text, enumList(def.Enum)),
			}
		}

	case "number":
		if _, ok := value.(float64); !ok {
			return &ValidationError{
				Field: p.Name, Problem: fmt.Sprintf("is %T, want a number", value),
			}
		}

	case "boolean":
		if _, ok := value.(bool); !ok {
			return &ValidationError{
				Field: p.Name, Problem: fmt.Sprintf("is %T, want a boolean", value),
			}
		}

	default:
		return &ValidationError{
			Field: p.Name, Problem: fmt.Sprintf("schema declares unsupported type %q", primary),
		}
	}

	return nil
}

// decodeTypes reads a JSON Schema `type`, which may be a string or an array of them.
func decodeTypes(raw json.RawMessage) (types []string, nullable bool, err error) {
	if len(raw) == 0 {
		return nil, false, errors.New("schema has no type")
	}

	var single string

	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, single == "null", nil
	}

	var many []string

	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, false, fmt.Errorf("schema type is neither a string nor an array: %w", err)
	}

	return many, slices.Contains(many, "null"), nil
}

func inEnum(enum []any, value string) bool {
	for _, e := range enum {
		if text, ok := e.(string); ok && text == value {
			return true
		}
	}

	return false
}

func enumList(enum []any) string {
	parts := make([]string, 0, len(enum))

	for _, e := range enum {
		if text, ok := e.(string); ok {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, ", ")
}

// ValidateJSON decodes a response and validates it in one step.
func ValidateJSON(s *Schema, raw []byte) (map[string]any, error) {
	var payload map[string]any

	// UseNumber is deliberately not set: a facet value is a string and a number field is a
	// float64, and json.Number would complicate every consumer for no benefit here.
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("response is not a JSON object: %w", err)
	}

	if err := Validate(s, payload); err != nil {
		return payload, err
	}

	return payload, nil
}
