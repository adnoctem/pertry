-- Initial schema, as specified in docs/design/bookscape.md section 6.
--
-- Two conventions run through it. Nothing is ever deleted: deduplication points losers at
-- their survivor via bookmarks.canonical_id and records the merge in merge_log, and the export
-- filter decides what ships. And every derived fact carries its provenance: the classifications
-- and bookmark_tags tables record whether a row came from a rule, the model or a human, and a
-- model row additionally records which model, run and prompt produced it.

CREATE TABLE import_batches (
    id INTEGER PRIMARY KEY,
    filename TEXT NOT NULL,
    -- Re-importing the same file is detectable, which is what makes `import` idempotent.
    sha256 TEXT NOT NULL UNIQUE,
    entry_count INTEGER NOT NULL,
    imported_at INTEGER NOT NULL
);

CREATE TABLE bookmarks (
    id INTEGER PRIMARY KEY,
    batch_id INTEGER NOT NULL REFERENCES import_batches (id),
    url TEXT NOT NULL,                  -- verbatim from the export
    url_norm TEXT NOT NULL,                  -- derived comparison key
    title TEXT,
    -- The <DD> text the export carried, if any. Chrome writes none, but section 1 lists
    -- Firefox and Safari as inputs too and those do. Distinct from
    -- classifications.description, which is generated; see section 6 of the design document.
    description TEXT,
    add_date INTEGER,                           -- unix seconds, NULL when absent
    last_modified INTEGER,
    private INTEGER NOT NULL DEFAULT 0,
    canonical_id INTEGER REFERENCES bookmarks (id)   -- NULL = survivor
);

CREATE INDEX idx_bookmarks_url_norm ON bookmarks (url_norm);
CREATE INDEX idx_bookmarks_canonical ON bookmarks (canonical_id);

-- Multi-valued: a link may appear under several original folders. A link filed under both
-- Dev/Go and Reading List carries two classification signals, and discarding either loses
-- information.
CREATE TABLE source_paths (
    bookmark_id INTEGER NOT NULL REFERENCES bookmarks (id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    PRIMARY KEY (bookmark_id, path)
);

CREATE TABLE liveness (
    bookmark_id INTEGER PRIMARY KEY REFERENCES bookmarks (id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('alive', 'dead', 'uncertain', 'skipped')),
    http_status INTEGER,
    final_url TEXT,
    method TEXT,
    error TEXT,
    checked_at INTEGER NOT NULL
);

CREATE TABLE classifications (
    bookmark_id INTEGER PRIMARY KEY REFERENCES bookmarks (id) ON DELETE CASCADE,
    collection TEXT NOT NULL,
    description TEXT,
    confidence REAL,
    source TEXT NOT NULL CHECK (source IN ('rule', 'model', 'manual')),
    model TEXT,
    run_id INTEGER,
    prompt_hash TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE tags (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE bookmark_tags (
    bookmark_id INTEGER NOT NULL REFERENCES bookmarks (id) ON DELETE CASCADE,
    tag_id INTEGER NOT NULL REFERENCES tags (id),
    -- 'import' is a fourth provenance the design document's section 6 did not list: a TAGS
    -- attribute that was already on the anchor. Firefox has first-class bookmark tags and
    -- exports them, and so does a re-imported bookscape export.
    source TEXT NOT NULL CHECK (source IN ('import', 'rule', 'model', 'manual')),
    PRIMARY KEY (bookmark_id, tag_id)
);

CREATE TABLE merge_log (
    canonical_id INTEGER NOT NULL REFERENCES bookmarks (id),
    merged_id INTEGER NOT NULL REFERENCES bookmarks (id),
    reason TEXT NOT NULL,
    merged_at INTEGER NOT NULL,
    PRIMARY KEY (canonical_id, merged_id)
);
