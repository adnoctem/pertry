-- Initial schema, as specified in docs/design/sicht.md section 11.
--
-- Two conventions run through it. Everything is keyed by content hash, never by path, so a result
-- survives the file being moved - which is the whole point, because `apply` moves files (section
-- 4.3). And every result carries its provenance: which stage and source produced it, and for a
-- model result which model, weights, prompt and schema, so that a re-run under any different
-- version is distinguishable rather than silently conflated (section 5.4).

CREATE TABLE files (
    hash       TEXT PRIMARY KEY,   -- SHA-256 of the content
    size       INTEGER NOT NULL,
    ext        TEXT    NOT NULL,
    width      INTEGER,
    height     INTEGER,
    -- 64-bit dHash for near-duplicate grouping. Stored from M1 but not acted on until M4.
    dhash      INTEGER,
    first_seen TEXT    NOT NULL
);

-- One hash can have several locations: the same image filed twice is one file in two places.
-- Paths are relative to the workspace root and use forward slashes on every platform, so a
-- manifest written on Windows still resolves under WSL (section 4.2).
CREATE TABLE locations (
    path    TEXT PRIMARY KEY,
    hash    TEXT NOT NULL REFERENCES files(hash) ON DELETE CASCADE,
    mtime   TEXT NOT NULL,
    seen_at TEXT NOT NULL
);

CREATE INDEX idx_locations_hash ON locations(hash);

CREATE TABLE metadata (
    hash           TEXT PRIMARY KEY REFERENCES files(hash) ON DELETE CASCADE,
    exif_json      TEXT,
    sidecar_json   TEXT,
    extracted_with TEXT
);

CREATE TABLE results (
    hash  TEXT NOT NULL REFERENCES files(hash) ON DELETE CASCADE,
    facet TEXT NOT NULL,
    value TEXT NOT NULL,

    confidence REAL,

    -- 'human', 'rule:<index>' or 'model:<name>'. Part of the primary key so that a human
    -- decision and a model's opinion about the same facet coexist, and the decisions view
    -- below picks between them rather than one overwriting the other.
    source TEXT NOT NULL,

    stage TEXT,

    -- Provenance for a model result. If any of these change, `run` reports results as stale
    -- and only re-processes them with --rerun (section 5.4).
    model        TEXT,
    model_digest TEXT,
    prompt_hash  TEXT,
    schema_hash  TEXT,

    -- Non-decision fields of the stage, kept for display and search.
    payload_json TEXT,

    -- Top alternatives at the decision's first token, for the review UI to offer.
    alternatives_json TEXT,

    created_at TEXT NOT NULL,

    PRIMARY KEY (hash, facet, source)
);

CREATE INDEX idx_results_facet ON results(facet, value);

-- The winning result per (hash, facet): human beats rule beats model, and within a source class
-- the most recent wins.
--
-- A view rather than a column, because precedence is a reading of the rows and not a fact about
-- them: materialising it would mean every writer having to maintain it, and a human decision
-- arriving after a model one would have to rewrite the model's row to take effect. Section 5.7
-- requires that a human decision always overrides, and this is where that is enforced.
CREATE VIEW decisions AS
SELECT hash, facet, value, confidence, source, stage, model, model_digest,
       prompt_hash, schema_hash, payload_json, alternatives_json, created_at
FROM (
    SELECT *,
           row_number() OVER (
               PARTITION BY hash, facet
               ORDER BY CASE
                            WHEN source = 'human' THEN 0
                            WHEN source LIKE 'rule:%' THEN 1
                            ELSE 2
                        END,
                        created_at DESC
           ) AS rank
    FROM results
)
WHERE rank = 1;

CREATE TABLE batches (
    batch_id      TEXT PRIMARY KEY,
    plan          TEXT NOT NULL,
    status        TEXT NOT NULL,
    manifest_path TEXT,
    created_at    TEXT NOT NULL
);

CREATE TABLE runs (
    run_id      TEXT PRIMARY KEY,
    stage       TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    stats_json  TEXT
);
