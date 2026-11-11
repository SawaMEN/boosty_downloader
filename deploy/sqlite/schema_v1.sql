-- Offline migration inventory schema. NOT enabled by the current Go server.
-- JSON remains authoritative until a separately tested Go SQLite adapter ships.
-- Media bytes remain on disk in SHA-256 content-addressed blobs.
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL,
    checksum_sha256 TEXT NOT NULL
);

-- One global identity for a source post, regardless of number of subscribers.
-- This row contains NO raw Boosty content and grants NO access.
CREATE TABLE IF NOT EXISTS source_posts (
    post_key INTEGER PRIMARY KEY,
    author_slug TEXT NOT NULL COLLATE NOCASE,
    remote_post_id TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    UNIQUE(author_slug, remote_post_id)
);

-- One owner's independent archived copy of a logical post, with one set of
-- permissions, deletion status and sync policy. Multiple owners reference
-- the same source_posts.post_key but NEVER each other's private copy.
CREATE TABLE IF NOT EXISTS owner_archives (
    archive_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    post_key INTEGER NOT NULL REFERENCES source_posts(post_key) ON DELETE RESTRICT,
    title TEXT NOT NULL DEFAULT '',
    job_state TEXT NOT NULL CHECK(job_state IN ('queued','running','done','partial','failed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    archived_at TEXT,
    deleted_at TEXT,
    auto_sync INTEGER NOT NULL DEFAULT 0 CHECK(auto_sync IN (0,1)),
    shared_for_all INTEGER NOT NULL DEFAULT 0 CHECK(shared_for_all IN (0,1)),
    -- Lossless private job payload for a future audited Go adapter.
    payload_json TEXT NOT NULL CHECK(json_valid(payload_json)),
    UNIQUE(owner_id, post_key)
);
CREATE INDEX IF NOT EXISTS idx_owner_archives_list ON owner_archives(owner_id, deleted_at, archived_at DESC);
CREATE INDEX IF NOT EXISTS idx_owner_archives_queue ON owner_archives(job_state, updated_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_owner_archives_trash ON owner_archives(deleted_at) WHERE deleted_at IS NOT NULL;

-- This describes the owner's Boosty tier when checked, NOT a post's
-- required tier. Never infer individual content rights from this table.
CREATE TABLE IF NOT EXISTS subscription_observations (
    observation_id INTEGER PRIMARY KEY,
    owner_id TEXT NOT NULL,
    author_slug TEXT NOT NULL COLLATE NOCASE,
    level_name TEXT,
    level_source_id TEXT,
    paid INTEGER NOT NULL CHECK(paid IN (0,1)),
    paused INTEGER NOT NULL CHECK(paused IN (0,1)),
    observed_at TEXT NOT NULL,
    source TEXT NOT NULL CHECK(source IN ('boosty_subscriptions','unknown'))
);
CREATE INDEX IF NOT EXISTS idx_tier_owner_author ON subscription_observations(owner_id, author_slug, observed_at DESC);

-- Each capture belongs to ONE owner archive. Older versions are not
-- publicly shareable even if the current owner archive is shared.
CREATE TABLE IF NOT EXISTS archive_revisions (
    revision_id TEXT PRIMARY KEY,
    archive_id TEXT NOT NULL REFERENCES owner_archives(archive_id) ON DELETE CASCADE,
    captured_at TEXT NOT NULL,
    title TEXT NOT NULL,
    source_fingerprint_sha256 TEXT,
    snapshot_relpath TEXT NOT NULL,
    subscription_observation_id INTEGER REFERENCES subscription_observations(observation_id) ON DELETE SET NULL,
    asset_count INTEGER NOT NULL DEFAULT 0 CHECK(asset_count >= 0),
    is_current INTEGER NOT NULL DEFAULT 0 CHECK(is_current IN (0,1)),
    UNIQUE(archive_id, snapshot_relpath)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_revision_current
    ON archive_revisions(archive_id) WHERE is_current = 1;
CREATE INDEX IF NOT EXISTS idx_revision_history
    ON archive_revisions(archive_id, captured_at DESC);

-- One logical content identity per verified SHA-256. A verified CAS path is
-- optional: libraries may have deduplication disabled or legacy media copies.
-- NULL relative_path MUST NOT be interpreted as an already deduplicated inode.
CREATE TABLE IF NOT EXISTS media_blobs (
    sha256 TEXT PRIMARY KEY CHECK(length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    byte_length INTEGER NOT NULL CHECK(byte_length >= 0),
    stored_at TEXT NOT NULL,
    relative_path TEXT UNIQUE
);

-- Cross-user archive revisions may reference exactly the same blob.
-- Deleting one owner never cascades into media_blobs: orphan objects are
-- reclaimed ONLY after no revision_assets reference remains and hardlink
-- count / filesystem reconciliation confirms safety.
CREATE TABLE IF NOT EXISTS revision_assets (
    revision_id TEXT NOT NULL REFERENCES archive_revisions(revision_id) ON DELETE CASCADE,
    asset_relpath TEXT NOT NULL,
    role TEXT NOT NULL,
    sha256 TEXT NOT NULL REFERENCES media_blobs(sha256) ON DELETE RESTRICT,
    PRIMARY KEY (revision_id, asset_relpath)
);
CREATE INDEX IF NOT EXISTS idx_blob_references ON revision_assets(sha256);

-- Grants are tied to one owner's object, NEVER to a global post or blob.
-- The Go app must check current authentication AND live per-object ACL
-- before any manifest/ZIP/media or stream response.
CREATE TABLE IF NOT EXISTS archive_grants (
    archive_id TEXT NOT NULL REFERENCES owner_archives(archive_id) ON DELETE CASCADE,
    grantee_owner_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (archive_id, grantee_owner_id)
);

CREATE TABLE IF NOT EXISTS archive_events (
    event_id INTEGER PRIMARY KEY,
    owner_id TEXT NOT NULL,
    archive_id TEXT,
    event_type TEXT NOT NULL,
    created_at TEXT NOT NULL,
    detail_json TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_owner_created ON archive_events(owner_id, created_at DESC);

-- Author-import jobs are not included in source_posts: discovery progress is
-- private account state, preserved losslessly until Go uses SQLite natively.
CREATE TABLE IF NOT EXISTS author_imports (
    import_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    author_slug TEXT NOT NULL COLLATE NOCASE,
    job_state TEXT NOT NULL CHECK(job_state IN ('queued','running','done','failed','canceled')),
    auto_sync INTEGER NOT NULL DEFAULT 0 CHECK(auto_sync IN (0,1)),
    updated_at TEXT NOT NULL,
    payload_json TEXT NOT NULL CHECK(json_valid(payload_json))
);
CREATE INDEX IF NOT EXISTS idx_author_imports_owner ON author_imports(owner_id, updated_at DESC);

-- Secrets/tokens and account password hashes remain in their existing
-- encrypted vault/account store until independently migrated and audited.
