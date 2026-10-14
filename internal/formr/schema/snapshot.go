package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/adnoctem/pertry/internal/formr/addr"
)

// SnapshotVersion is the envelope format this build reads and writes.
const SnapshotVersion = 1

// Provenance records what produced a set of schemas. It is rendered into the report header, so
// that a reader can tell which provider releases a coverage claim is about.
type Provenance struct {
	// Engine is empty when the schemas came from a bare `providers schema -json` document.
	Engine EngineInfo
	// Providers maps a provider to the release that was selected; a provider absent here has an
	// unknown release.
	Providers map[addr.Provider]string
	// FromSnapshot is true when no engine was run.
	FromSnapshot bool
}

type envelope struct {
	FormrSnapshot int               `json:"formr_snapshot"`
	Engine        EngineInfo        `json:"engine"`
	Providers     map[string]string `json:"providers"`
	Schemas       json.RawMessage   `json:"schemas"`
}

// WriteSnapshot encodes schemas with their provenance.
func WriteSnapshot(raw []byte, prov Provenance) ([]byte, error) {
	providers := map[string]string{}
	for p, v := range prov.Providers {
		providers[p.String()] = v
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("provider schemas are not valid JSON: %w", err)
	}

	out, err := json.MarshalIndent(envelope{
		FormrSnapshot: SnapshotVersion,
		Engine:        prov.Engine,
		Providers:     providers,
		Schemas:       compact.Bytes(),
	}, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(out, '\n'), nil
}

// ReadSnapshot accepts either a formr envelope or a bare `providers schema -json` document. The
// second has no provenance, and the returned warnings say so rather than inventing any.
func ReadSnapshot(data []byte) (raw []byte, prov Provenance, warnings []string, err error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, Provenance{}, nil, fmt.Errorf("schema file is not a JSON object: %w", err)
	}

	prov = Provenance{FromSnapshot: true, Providers: map[addr.Provider]string{}}

	if _, ok := probe["formr_snapshot"]; !ok {
		if _, ok := probe["provider_schemas"]; !ok {
			return nil, Provenance{}, nil, errors.New("schema file is neither a formr snapshot nor `providers schema -json` output")
		}

		return data, prov, []string{"schema file carries no provenance: engine and provider releases are unknown"}, nil
	}

	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, Provenance{}, nil, fmt.Errorf("snapshot: %w", err)
	}
	if env.FormrSnapshot != SnapshotVersion {
		return nil, Provenance{}, nil, fmt.Errorf("snapshot format %d is not supported (this build reads %d)", env.FormrSnapshot, SnapshotVersion)
	}

	prov.Engine = env.Engine
	if env.Engine.Name == "" || env.Engine.Version == "" {
		warnings = append(warnings, "snapshot does not record the engine that produced it")
	}

	keys := make([]string, 0, len(env.Providers))
	for k := range env.Providers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p, err := addr.ParseProvider(k)
		if err != nil {
			return nil, Provenance{}, nil, fmt.Errorf("snapshot: %w", err)
		}
		prov.Providers[p] = env.Providers[k]
	}

	return env.Schemas, prov, warnings, nil
}
