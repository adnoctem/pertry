// Package schema turns a stage's field list into the JSON Schema used for constrained decoding
// (docs/ARCHITECTURE.md#model-stages).
//
// This is the only place schemas are built. That is a deliberate constraint from docs/ARCHITECTURE.md#model-stages: the schema
// determines what the model may emit, what is validated, and - through the ordering rule below -
// how good the decision is, so a second generator that disagreed with this one would be very hard
// to notice.
package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/adnoctem/pertry/internal/sicht/config"
)

// OtherValue is the escape hatch added to a closed enum.
//
// A curated taxonomy is never complete, and a model forced to choose from an incomplete list will
// pick the closest wrong answer rather than decline. "other" plus a free-text companion field lets
// it say so, and docs/ARCHITECTURE.md#model-stages uses those answers to grow the taxonomy.
const OtherValue = "other"

// otherSuffix names the free-text companion of a closed enum.
const otherSuffix = "_other"

// OtherMaxLength bounds the free-text companion. Short on purpose: it is a label, not a
// description, and a long one would invite the model to write prose where a word is wanted.
const OtherMaxLength = 40

// OpenLabelMaxLength bounds an open-mode taxonomy field, for the same reason.
const OpenLabelMaxLength = 40

// Schema is a generated JSON Schema, kept as ordered properties rather than a map.
//
// Order matters and Go maps have none. docs/ARCHITECTURE.md#model-stages's ordering rule is the whole reason: generation is
// autoregressive, so a decision field placed after the observational fields is conditioned on what
// the model has already written about the image. Marshalling through a map would silently shuffle
// that away.
type Schema struct {
	// Properties are in emission order.
	Properties []Property

	// Required lists every property name, in the same order.
	Required []string
}

// Property is one field of the generated schema.
type Property struct {
	Name string

	// Definition is the property's schema, already encoded, so that nested shapes stay
	// exactly as built.
	Definition json.RawMessage

	// Decision marks the property that becomes a facet value.
	Decision bool

	// Facet is the facet a decision property decides, which for a `<name>_other` companion
	// is the facet it belongs to rather than its own name.
	Facet string
}

// Build generates the schema for a stage.
//
// Fields are emitted in two groups: every observational field first, in configuration order, then
// every decision field. docs/ARCHITECTURE.md#model-stages requires it, and the configuration is written that way already - but
// enforcing it here means a user who reorders their fields does not quietly make the model worse.
func Build(stage config.Stage, taxonomies map[string][]string) (*Schema, error) {
	out := &Schema{}

	observational := make([]config.Field, 0, len(stage.Fields))
	decisions := make([]config.Field, 0, 2)

	for _, f := range stage.Fields {
		if f.Decision {
			decisions = append(decisions, f)
		} else {
			observational = append(observational, f)
		}
	}

	if len(decisions) == 0 {
		return nil, fmt.Errorf("stage %q has no decision field, so its schema would decide nothing",
			stage.Name)
	}

	for _, f := range append(observational, decisions...) {
		properties, err := propertiesFor(stage, f, taxonomies)
		if err != nil {
			return nil, err
		}

		out.Properties = append(out.Properties, properties...)
	}

	for _, p := range out.Properties {
		out.Required = append(out.Required, p.Name)
	}

	return out, nil
}

// propertiesFor renders one configured field, which may produce two properties.
func propertiesFor(stage config.Stage, f config.Field, taxonomies map[string][]string) ([]Property, error) {
	switch f.Type {
	case config.FieldString:
		return []Property{{
			Name:       f.Name,
			Definition: stringSchema(f.Max, f.Nullable, ""),
			Decision:   f.Decision,
			Facet:      f.Name,
		}}, nil

	case config.FieldNumber:
		return []Property{{
			Name:       f.Name,
			Definition: scalarSchema("number", f.Nullable),
			Decision:   f.Decision,
			Facet:      f.Name,
		}}, nil

	case config.FieldBool:
		return []Property{{
			Name:       f.Name,
			Definition: scalarSchema("boolean", f.Nullable),
			Decision:   f.Decision,
			Facet:      f.Name,
		}}, nil

	case config.FieldEnum:
		return enumProperties(stage, f, taxonomies)

	default:
		return nil, fmt.Errorf("stage %q field %q has unknown type %q",
			stage.Name, f.Name, f.Type)
	}
}

// enumProperties renders an enum field, whose shape depends on the stage's mode (docs/ARCHITECTURE.md#model-stages).
func enumProperties(stage config.Stage, f config.Field, taxonomies map[string][]string) ([]Property, error) {
	// A fixed value list is an enum in either mode: the author stated the options, so there
	// is nothing to discover.
	if len(f.Values) > 0 {
		return []Property{{
			Name:       f.Name,
			Definition: enumSchema(f.Values, f.Nullable),
			Decision:   f.Decision,
			Facet:      f.Name,
		}}, nil
	}

	if f.Taxonomy == "" {
		return nil, fmt.Errorf("stage %q field %q is an enum with neither values nor a taxonomy",
			stage.Name, f.Name)
	}

	// Open mode: the taxonomy does not exist yet, and discovering it is the point. A free
	// short label, which `sicht taxonomy` then aggregates for curation (docs/ARCHITECTURE.md#model-stages).
	if stage.Mode == config.ModeOpen {
		return []Property{{
			Name: f.Name,
			Definition: stringSchema(OpenLabelMaxLength, f.Nullable,
				"A short lowercase label, one or two words, with no punctuation. "+
					"Reuse an existing label rather than inventing a variant."),
			Decision: f.Decision,
			Facet:    f.Name,
		}}, nil
	}

	// Closed mode: the curated enum, plus "other" and a free-text companion so the model can
	// decline rather than pick the closest wrong answer.
	values := taxonomies[f.Taxonomy]
	if len(values) == 0 {
		return nil, fmt.Errorf("stage %q is closed and field %q names taxonomy %q, which is empty\n"+
			"hint: run the stage in open mode, curate with `sicht taxonomy %s`, then close it",
			stage.Name, f.Name, f.Taxonomy, f.Taxonomy)
	}

	withOther := slices.Clone(values)
	if !slices.Contains(withOther, OtherValue) {
		withOther = append(withOther, OtherValue)
	}

	return []Property{
		{
			Name:       f.Name,
			Definition: enumSchema(withOther, f.Nullable),
			Decision:   f.Decision,
			Facet:      f.Name,
		},
		{
			Name: f.Name + otherSuffix,
			Definition: stringSchema(OtherMaxLength, true,
				fmt.Sprintf("Set this only when %q is %q: a short lowercase label for what it "+
					"actually is. Otherwise null.", f.Name, OtherValue)),
			Decision: false,
			Facet:    f.Name,
		},
	}, nil
}

func stringSchema(maxLength int, nullable bool, description string) json.RawMessage {
	var b bytes.Buffer

	b.WriteString("{")
	b.WriteString(`"type":`)
	b.WriteString(typeOf("string", nullable))

	if maxLength > 0 {
		fmt.Fprintf(&b, `,"maxLength":%d`, maxLength)
	}

	if description != "" {
		encoded, _ := json.Marshal(description)

		fmt.Fprintf(&b, `,"description":%s`, encoded)
	}

	b.WriteString("}")

	return json.RawMessage(b.Bytes())
}

func scalarSchema(kind string, nullable bool) json.RawMessage {
	return json.RawMessage(`{"type":` + typeOf(kind, nullable) + `}`)
}

func enumSchema(values []string, nullable bool) json.RawMessage {
	var b bytes.Buffer

	b.WriteString(`{"type":`)
	b.WriteString(typeOf("string", nullable))
	b.WriteString(`,"enum":[`)

	for i, v := range values {
		if i > 0 {
			b.WriteString(",")
		}

		encoded, _ := json.Marshal(v)
		b.Write(encoded)
	}

	if nullable {
		// A nullable enum must admit null in the enum as well as in the type, or a
		// validator rejects the null the type permits.
		b.WriteString(",null")
	}

	b.WriteString("]}")

	return json.RawMessage(b.Bytes())
}

// typeOf renders a JSON Schema type, as an array when the field may be null.
func typeOf(kind string, nullable bool) string {
	if nullable {
		return `["` + kind + `","null"]`
	}

	return `"` + kind + `"`
}

// MarshalJSON renders the schema with its properties in order.
//
// Hand-assembled rather than built from a map, because the key order is the thing being preserved.
// additionalProperties is false so that a model cannot invent a field, which both tightens the
// constrained decoding and makes validation meaningful.
func (s *Schema) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer

	b.WriteString(`{"type":"object","properties":{`)

	for i, p := range s.Properties {
		if i > 0 {
			b.WriteString(",")
		}

		name, err := json.Marshal(p.Name)
		if err != nil {
			return nil, fmt.Errorf("encode property name %q: %w", p.Name, err)
		}

		b.Write(name)
		b.WriteString(":")
		b.Write(p.Definition)
	}

	b.WriteString(`},"required":[`)

	for i, name := range s.Required {
		if i > 0 {
			b.WriteString(",")
		}

		encoded, err := json.Marshal(name)
		if err != nil {
			return nil, fmt.Errorf("encode required name %q: %w", name, err)
		}

		b.Write(encoded)
	}

	b.WriteString(`],"additionalProperties":false}`)

	return b.Bytes(), nil
}

// JSON returns the encoded schema.
func (s *Schema) JSON() (json.RawMessage, error) {
	raw, err := s.MarshalJSON()
	if err != nil {
		return nil, err
	}

	return json.RawMessage(raw), nil
}

// Hash returns a stable identifier for the schema, recorded with every result so that a result
// produced under a different schema is distinguishable rather than silently conflated (docs/ARCHITECTURE.md#model-stages).
func (s *Schema) Hash() (string, error) {
	raw, err := s.MarshalJSON()
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(raw)

	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DecisionFields returns the properties that become facet values, in emission order.
func (s *Schema) DecisionFields() []Property {
	var out []Property

	for _, p := range s.Properties {
		if p.Decision {
			out = append(out, p)
		}
	}

	return out
}

// Names returns every property name, in emission order.
func (s *Schema) Names() []string { return slices.Clone(s.Required) }

// OtherFieldFor returns the name of a decision field's free-text companion.
func OtherFieldFor(facet string) string { return facet + otherSuffix }

// HasEscapeHatch reports whether Build gave a field the "other" value and its free-text companion.
//
// Only a taxonomy field of a closed stage has one. An inline enum may well list "other" among its
// own values, and there it is an ordinary answer: the default `kind` stage did exactly that, and
// treating it as the escape hatch sent every "other" to review as an invented value, whatever its
// confidence, with no companion to resolve it into.
func HasEscapeHatch(stage config.Stage, field string) bool {
	if stage.Mode == config.ModeOpen {
		return false
	}

	for _, f := range stage.Fields {
		if f.Name == field {
			return f.Taxonomy != ""
		}
	}

	return false
}

// ResolveValue returns the value to store for a decision field, following "other" into its
// companion field.
//
// A result of "other" is not a facet value anybody wants to file by; the companion holds what the
// model actually saw. Returning it, marked as uncurated, is what lets docs/ARCHITECTURE.md#model-stages aggregate real labels
// instead of a pile of "other".
func ResolveValue(facet string, payload map[string]any) (value string, uncurated bool) {
	raw, _ := payload[facet].(string)

	if raw != OtherValue {
		return raw, false
	}

	companion, ok := payload[OtherFieldFor(facet)].(string)
	if !ok {
		return OtherValue, true
	}

	companion = strings.ToLower(strings.TrimSpace(companion))
	if companion == "" {
		return OtherValue, true
	}

	return companion, true
}
