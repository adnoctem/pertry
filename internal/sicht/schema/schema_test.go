package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/schema"
)

// TestPropertyOrderPutsTheDecisionLast is docs/ARCHITECTURE.md#model-stages's ordering rule, and it is the single most
// consequential thing in this package. Generation is autoregressive, so a decision field placed
// after the observational fields is conditioned on what the model has already written about the
// image. Marshalling through a Go map would shuffle that away silently.
func TestPropertyOrderPutsTheDecisionLast(t *testing.T) {
	t.Parallel()

	stage := config.Stage{
		Name: "platform", Mode: config.ModeOpen,
		Fields: []config.Field{
			// Deliberately configured with the decision FIRST, to prove the generator
			// reorders rather than trusting the author.
			{Name: "platform", Type: config.FieldEnum, Taxonomy: "platform", Decision: true},
			{Name: "visible_text_excerpt", Type: config.FieldString, Max: 300},
			{Name: "visible_url", Type: config.FieldString, Nullable: true},
			{Name: "ui_description", Type: config.FieldString, Max: 200},
		},
	}

	built, err := schema.Build(stage, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	want := []string{"visible_text_excerpt", "visible_url", "ui_description", "platform"}

	got := built.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q (observations must precede the decision)",
				i, got[i], want[i])
		}
	}
}

// TestEncodedOrderMatchesPropertyOrder checks the JSON itself, since a correct internal order that
// marshals in a different one would be worthless.
func TestEncodedOrderMatchesPropertyOrder(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(kindStage(), nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	raw, err := built.JSON()
	if err != nil {
		t.Fatalf("JSON(): %v", err)
	}

	encoded := string(raw)

	// The observational field's key must appear before the decision field's.
	visible := strings.Index(encoded, `"visible_ui"`)
	kind := strings.Index(encoded, `"kind"`)

	if visible < 0 || kind < 0 {
		t.Fatalf("both keys should be present: %s", encoded)
	}

	if visible > kind {
		t.Errorf("the decision key precedes the observation in the encoded schema: %s", encoded)
	}

	// And `required` is in the same order, which docs/ARCHITECTURE.md#model-stages asks for explicitly.
	requiredAt := strings.Index(encoded, `"required"`)
	if requiredAt < 0 {
		t.Fatalf("the schema has no required list: %s", encoded)
	}

	tail := encoded[requiredAt:]

	if strings.Index(tail, `"visible_ui"`) > strings.Index(tail, `"kind"`) {
		t.Errorf("required is not in property order: %s", tail)
	}
}

// TestEncodedOrderIsStable guards against a map creeping into the implementation: Go randomises
// map iteration, so a regression would show up as an intermittent order.
func TestEncodedOrderIsStable(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(kindStage(), nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	first, err := built.JSON()
	if err != nil {
		t.Fatalf("JSON(): %v", err)
	}

	for range 50 {
		again, err := built.JSON()
		if err != nil {
			t.Fatalf("JSON(): %v", err)
		}

		if string(again) != string(first) {
			t.Fatalf("the encoded schema varies between calls:\n%s\n%s", first, again)
		}
	}
}

func TestSchemaIsValidJSON(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	for _, stage := range cfg.Stages {
		built, err := schema.Build(stage, cfg.Taxonomies)
		if err != nil {
			t.Fatalf("Build(%s): %v", stage.Name, err)
		}

		raw, err := built.JSON()
		if err != nil {
			t.Fatalf("JSON(%s): %v", stage.Name, err)
		}

		var generic map[string]any

		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatalf("stage %s produced invalid JSON: %v\n%s", stage.Name, err, raw)
		}

		if generic["type"] != "object" {
			t.Errorf("stage %s: type = %v, want object", stage.Name, generic["type"])
		}

		// A model must not be able to invent a field: it tightens constrained decoding and
		// makes validation mean something.
		if generic["additionalProperties"] != false {
			t.Errorf("stage %s: additionalProperties = %v, want false",
				stage.Name, generic["additionalProperties"])
		}
	}
}

func TestStringFieldMaxLength(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(config.Stage{
		Name: "s", Mode: config.ModeOpen,
		Fields: []config.Field{
			{Name: "note", Type: config.FieldString, Max: 200},
			{Name: "d", Type: config.FieldString, Decision: true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	raw, _ := built.JSON()
	if !strings.Contains(string(raw), `"maxLength":200`) {
		t.Errorf("maxLength is missing: %s", raw)
	}
}

func TestNullableField(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(config.Stage{
		Name: "s", Mode: config.ModeOpen,
		Fields: []config.Field{
			{Name: "visible_url", Type: config.FieldString, Nullable: true},
			{Name: "d", Type: config.FieldString, Decision: true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	raw, _ := built.JSON()
	if !strings.Contains(string(raw), `["string","null"]`) {
		t.Errorf("a nullable field is not typed as nullable: %s", raw)
	}
}

func TestEnumWithFixedValues(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(kindStage(), nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	raw, _ := built.JSON()

	for _, want := range []string{"screenshot", "photo", "scan_document", "other"} {
		if !strings.Contains(string(raw), `"`+want+`"`) {
			t.Errorf("enum value %q is missing: %s", want, raw)
		}
	}

	// A fixed value list is the author's own statement of the options, so no companion field
	// is invented for it.
	if strings.Contains(string(raw), "kind_other") {
		t.Errorf("a fixed enum gained an _other companion: %s", raw)
	}
}

// TestOpenModeTaxonomyIsAShortLabel is docs/ARCHITECTURE.md#model-stages: in open mode the taxonomy does not exist yet, and
// discovering it is the point.
func TestOpenModeTaxonomyIsAShortLabel(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(config.Stage{
		Name: "platform", Mode: config.ModeOpen,
		Fields: []config.Field{
			{Name: "platform", Type: config.FieldEnum, Taxonomy: "platform", Decision: true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	raw, _ := built.JSON()
	encoded := string(raw)

	if strings.Contains(encoded, `"enum"`) {
		t.Errorf("an open-mode taxonomy field became an enum: %s", encoded)
	}

	if !strings.Contains(encoded, `"maxLength":40`) {
		t.Errorf("the open label is not bounded: %s", encoded)
	}

	// The instruction matters: without it the model writes a sentence where a label is
	// wanted, and the taxonomy becomes impossible to curate.
	if !strings.Contains(encoded, "lowercase label") {
		t.Errorf("the open label carries no instruction: %s", encoded)
	}
}

// TestClosedModeTaxonomyGetsOtherAndACompanion is the escape hatch. A curated taxonomy is never
// complete, and a model forced to choose from an incomplete list picks the closest wrong answer
// instead of declining.
func TestClosedModeTaxonomyGetsOtherAndACompanion(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(config.Stage{
		Name: "platform", Mode: config.ModeClosed,
		Fields: []config.Field{
			{Name: "ui", Type: config.FieldString, Max: 100},
			{Name: "platform", Type: config.FieldEnum, Taxonomy: "platform", Decision: true},
		},
	}, map[string][]string{"platform": {"instagram", "reddit", "youtube"}})
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	names := built.Names()

	want := []string{"ui", "platform", "platform_other"}
	if len(names) != len(want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}

	for i := range want {
		if names[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], want[i])
		}
	}

	raw, _ := built.JSON()

	if !strings.Contains(string(raw), `"other"`) {
		t.Errorf("the closed enum has no escape hatch: %s", raw)
	}

	// The companion is not itself a decision; it informs the one above it.
	for _, p := range built.DecisionFields() {
		if p.Name == "platform_other" {
			t.Error("the _other companion is marked as a decision field")
		}
	}
}

func TestClosedModeRejectsAnEmptyTaxonomy(t *testing.T) {
	t.Parallel()

	_, err := schema.Build(config.Stage{
		Name: "platform", Mode: config.ModeClosed,
		Fields: []config.Field{
			{Name: "platform", Type: config.FieldEnum, Taxonomy: "platform", Decision: true},
		},
	}, nil)
	if err == nil {
		t.Fatal("Build() error = nil for a closed stage over an empty taxonomy")
	}

	// The message has to name the way out, since the mistake is an ordering one.
	if !strings.Contains(err.Error(), "sicht taxonomy") {
		t.Errorf("error = %q, want it to point at the taxonomy workflow", err)
	}
}

func TestBuildRejectsAStageWithNoDecision(t *testing.T) {
	t.Parallel()

	_, err := schema.Build(config.Stage{
		Name: "s", Mode: config.ModeOpen,
		Fields: []config.Field{{Name: "note", Type: config.FieldString}},
	}, nil)
	if err == nil {
		t.Error("Build() error = nil for a stage that decides nothing")
	}
}

// TestHashChangesWithTheSchema is what makes docs/ARCHITECTURE.md#model-stages's staleness detection work: a result produced
// under a different schema must be distinguishable rather than silently conflated.
func TestHashChangesWithTheSchema(t *testing.T) {
	t.Parallel()

	first, err := schema.Build(kindStage(), nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	firstHash, err := first.Hash()
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}

	// The same stage hashes the same, every time.
	again, _ := schema.Build(kindStage(), nil)

	againHash, err := again.Hash()
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}

	if againHash != firstHash {
		t.Errorf("the same stage hashed differently: %q vs %q", firstHash, againHash)
	}

	// A changed enum changes the hash.
	changed := kindStage()
	changed.Fields[1].Values = append(changed.Fields[1].Values, "diagram")

	changedBuilt, _ := schema.Build(changed, nil)

	changedHash, err := changedBuilt.Hash()
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}

	if changedHash == firstHash {
		t.Error("adding an enum value did not change the schema hash")
	}

	if !strings.HasPrefix(firstHash, "sha256:") {
		t.Errorf("Hash() = %q, want it to name its algorithm", firstHash)
	}
}

// TestResolveValueFollowsOther is what keeps a taxonomy from filling up with "other". The
// companion holds what the model actually saw, and that is the label worth aggregating.
func TestResolveValueFollowsOther(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		payload   map[string]any
		want      string
		uncurated bool
	}{
		{
			name:    "a curated value",
			payload: map[string]any{"platform": "reddit"},
			want:    "reddit",
		},
		{
			name: "other with a companion",
			payload: map[string]any{
				"platform": "other", "platform_other": "Mastodon",
			},
			want: "mastodon", uncurated: true,
		},
		{
			name:    "other with no companion",
			payload: map[string]any{"platform": "other"},
			want:    "other", uncurated: true,
		},
		{
			name: "other with an empty companion",
			payload: map[string]any{
				"platform": "other", "platform_other": "   ",
			},
			want: "other", uncurated: true,
		},
		{
			name: "other with a null companion",
			payload: map[string]any{
				"platform": "other", "platform_other": nil,
			},
			want: "other", uncurated: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, uncurated := schema.ResolveValue("platform", tt.payload)

			if got != tt.want {
				t.Errorf("value = %q, want %q", got, tt.want)
			}

			if uncurated != tt.uncurated {
				t.Errorf("uncurated = %v, want %v", uncurated, tt.uncurated)
			}
		})
	}
}

func TestNumberAndBoolFields(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(config.Stage{
		Name: "s", Mode: config.ModeOpen,
		Fields: []config.Field{
			{Name: "count", Type: config.FieldNumber},
			{Name: "legible", Type: config.FieldBool},
			{Name: "d", Type: config.FieldString, Decision: true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	raw, _ := built.JSON()

	for _, want := range []string{`"type":"number"`, `"type":"boolean"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s is missing: %s", want, raw)
		}
	}
}

func kindStage() config.Stage {
	return config.Stage{
		Name: "kind", Mode: config.ModeClosed,
		Fields: []config.Field{
			{Name: "visible_ui", Type: config.FieldString, Max: 200},
			{
				Name: "kind", Type: config.FieldEnum, Decision: true,
				Values: []string{"screenshot", "photo", "scan_document", "other"},
			},
		},
	}
}

// TestValidateAcceptsAGoodResponse is the happy path for docs/ARCHITECTURE.md#inference-backend-and-model-choice's requirement that every response be
// validated even though decoding was constrained - "constrained" being a property of the server's
// implementation rather than something the caller can verify.
func TestValidateAcceptsAGoodResponse(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(kindStage(), nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	payload, err := schema.ValidateJSON(built, []byte(`{"visible_ui":"a status bar","kind":"screenshot"}`))
	if err != nil {
		t.Fatalf("ValidateJSON() error = %v, want nil", err)
	}

	if payload["kind"] != "screenshot" {
		t.Errorf("kind = %v, want screenshot", payload["kind"])
	}
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(kindStage(), nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	tests := map[string]struct {
		json string
		want string
	}{
		"a missing required property": {
			`{"visible_ui":"a status bar"}`, "missing",
		},
		"a value outside the enum": {
			`{"visible_ui":"x","kind":"meme"}`, "not one of",
		},
		"a string where an enum is wanted": {
			`{"visible_ui":"x","kind":42}`, "want a string",
		},
		"a null in a field that forbids it": {
			`{"visible_ui":null,"kind":"photo"}`, "does not allow null",
		},
		"an invented property": {
			`{"visible_ui":"x","kind":"photo","mood":"cheerful"}`, "not in the schema",
		},
		"a string over maxLength": {
			`{"visible_ui":"` + strings.Repeat("x", 201) + `","kind":"photo"}`, "schema allows 200",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := schema.ValidateJSON(built, []byte(tt.json))
			if err == nil {
				t.Fatalf("ValidateJSON() error = nil, want one mentioning %q", tt.want)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestValidateCountsCharactersNotBytes: maxLength in JSON Schema is defined over characters, and a
// model asked for 200 characters of German should not be cut off at 140.
func TestValidateCountsCharactersNotBytes(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(config.Stage{
		Name: "s", Mode: config.ModeOpen,
		Fields: []config.Field{
			{Name: "note", Type: config.FieldString, Max: 10},
			{Name: "d", Type: config.FieldString, Decision: true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	// Ten two-byte runes: twenty bytes, ten characters, so it fits.
	payload := `{"note":"ümlautüüüü","d":"x"}`

	if _, err := schema.ValidateJSON(built, []byte(payload)); err != nil {
		t.Errorf("ValidateJSON() error = %v, want ten runes to be accepted", err)
	}
}

func TestValidateAcceptsANullableField(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(config.Stage{
		Name: "s", Mode: config.ModeOpen,
		Fields: []config.Field{
			{Name: "visible_url", Type: config.FieldString, Nullable: true},
			{Name: "d", Type: config.FieldString, Decision: true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	if _, err := schema.ValidateJSON(built, []byte(`{"visible_url":null,"d":"x"}`)); err != nil {
		t.Errorf("ValidateJSON() error = %v, want null to be accepted", err)
	}
}

// TestValidateReportsEveryProblem rather than only the first, because a response that is wrong in
// three ways is worth logging once with all three.
func TestValidateReportsEveryProblem(t *testing.T) {
	t.Parallel()

	built, err := schema.Build(kindStage(), nil)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	_, err = schema.ValidateJSON(built, []byte(`{"kind":"meme","invented":true}`))
	if err == nil {
		t.Fatal("ValidateJSON() error = nil")
	}

	message := err.Error()

	for _, want := range []string{"visible_ui", "kind", "invented"} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to mention %q", message, want)
		}
	}
}

func TestValidateNotAnObject(t *testing.T) {
	t.Parallel()

	built, _ := schema.Build(kindStage(), nil)

	if _, err := schema.ValidateJSON(built, []byte(`["screenshot"]`)); err == nil {
		t.Error("ValidateJSON() error = nil for an array, want an error")
	}
}
