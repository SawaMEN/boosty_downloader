#!/usr/bin/env python3
"""Offline non-destructive SQLite inventory of Boosty Downloader metadata.

Go continues using JSON. Stop all writers before using this export.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
import sys
import tempfile
from datetime import datetime, timezone

PART = re.compile(r"[a-zA-Z0-9_.-]{1,128}\Z")
SHA = re.compile(r"[0-9a-f]{64}\Z")
REV = re.compile(r"[0-9a-f]{24}\Z")
STATES = frozenset({"queued", "running", "done", "partial", "failed"})


class MigrationError(Exception):
    pass


def part(value, kind):
    if not isinstance(value, str) or value in (".", "..") or not PART.fullmatch(value):
        raise MigrationError(f"Unsafe {kind}: {value!r}")
    return value


def regular(path):
    try:
        info = path.lstat()
    except OSError as exc:
        raise MigrationError(f"Cannot inspect {path}: {exc}") from exc
    if not stat.S_ISREG(info.st_mode):
        raise MigrationError(f"Not a regular file: {path}")
    return info


def directory(path):
    try:
        info = path.lstat()
    except OSError as exc:
        raise MigrationError(f"Cannot inspect {path}: {exc}") from exc
    if not stat.S_ISDIR(info.st_mode):
        raise MigrationError(f"Not a real directory: {path}")


def safe_file(root, rel):
    path = root
    for segment in rel.parts[:-1]:
        part(segment, "directory component")
        path = path / segment
        directory(path)
    final = path / part(rel.name, "filename")
    return final, regular(final)


def digest_file(path):
    hasher = hashlib.sha256()
    size = 0
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1 << 20), b""):
            size += len(chunk)
            hasher.update(chunk)
    return hasher.hexdigest(), size


def checked_json(root, path):
    file, _ = safe_file(root, path.relative_to(root))
    with file.open("rb") as source:
        return json.load(source)


def text(value):
    return value if isinstance(value, str) else ""


def checked_snapshot(db, root, job, rel, revision_id, is_current, stats, inode_set):
    base = root / rel
    man = checked_json(root, base / "manifest.json")
    if not isinstance(man, dict) or man.get("author") != job["author"] or man.get("postId") != job["postId"]:
        raise MigrationError(f"Archive identity mismatch: {base}")
    safe_file(root, rel / "index.html")
    assets = man.get("assets", [])
    if not isinstance(assets, list) or len(assets) > 5000:
        raise MigrationError(f"Invalid media list: {base}")
    captured = text(man.get("capturedAt")) or text(job.get("archiveUpdatedAt"))
    if not captured:
        raise MigrationError(f"Missing capture date: {base}")
    observation = None
    sub = man.get("subscription")
    if isinstance(sub, dict) and sub.get("observedAt") and sub.get("source") in ("boosty_subscriptions", "unknown"):
        cur = db.execute("""INSERT INTO subscription_observations
            (owner_id,author_slug,level_name,level_source_id,paid,paused,observed_at,source)
            VALUES (?,?,?,?,?,?,?,?)""", (job["ownerId"], job["author"], text(sub.get("level")),
            text(sub.get("levelId")), int(bool(sub.get("paid"))), int(bool(sub.get("paused"))),
            sub["observedAt"], sub["source"]))
        observation = cur.lastrowid
    fingerprint = man.get("contentSha256")
    if not isinstance(fingerprint, str) or not SHA.fullmatch(fingerprint):
        fingerprint = None
    db.execute("""INSERT INTO archive_revisions
        (revision_id,archive_id,captured_at,title,source_fingerprint_sha256,
         snapshot_relpath,subscription_observation_id,asset_count,is_current)
        VALUES (?,?,?,?,?,?,?,?,?)""", (revision_id, job["id"], captured, text(man.get("title")),
        fingerprint, rel.as_posix(), observation, len(assets), int(is_current)))
    used = set()
    for asset in assets:
        if not isinstance(asset, dict):
            raise MigrationError(f"Invalid asset metadata: {base}")
        name = part(asset.get("filename"), "asset filename")
        if name in used:
            raise MigrationError(f"Duplicate asset: {base / 'media' / name}")
        used.add(name)
        asset_rel = rel / "media" / name
        media_path, before = safe_file(root, asset_rel)
        expected_hash = asset.get("sha256")
        if expected_hash is not None and expected_hash != "" and (not isinstance(expected_hash, str) or not SHA.fullmatch(expected_hash)):
            raise MigrationError(f"Invalid SHA-256 metadata: {media_path}")
        digest, size = digest_file(media_path)
        after = regular(media_path)
        if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) or before.st_size != size:
            raise MigrationError(f"Media changed during migration: {media_path}")
        expected_size = asset.get("size")
        if isinstance(expected_size, bool) or (expected_size is not None and (not isinstance(expected_size, int) or expected_size < 0)):
            raise MigrationError(f"Invalid asset size: {media_path}")
        if expected_size not in (None, 0) and size != expected_size:
            raise MigrationError(f"Size mismatch: {media_path}")
        if expected_hash and expected_hash != digest:
            raise MigrationError(f"SHA-256 mismatch: {media_path}")
        exists = db.execute("SELECT byte_length FROM media_blobs WHERE sha256=?", (digest,)).fetchone()
        if exists and exists[0] != size:
            raise MigrationError(f"Digest/size collision: {media_path}")
        if not exists:
            # NULL CAS path means that files may exist without hardlink dedup.
            cas_rel = Path("blobs") / "sha256" / digest[:2] / digest
            cas = root / cas_rel
            cas_name = None
            if cas.exists() or cas.is_symlink():
                cas_file, cas_info = safe_file(root, cas_rel)
                if cas_info.st_size != size or digest_file(cas_file) != (digest, size):
                    raise MigrationError(f"Corrupt CAS object: {cas_file}")
                cas_name = cas_rel.as_posix()
            db.execute("INSERT INTO media_blobs (sha256,byte_length,stored_at,relative_path) VALUES (?,?,?,?)",
                       (digest, size, datetime.now(timezone.utc).isoformat(), cas_name))
        db.execute("INSERT INTO revision_assets (revision_id,asset_relpath,role,sha256) VALUES (?,?,?,?)",
                   (revision_id, (Path("media") / name).as_posix(), text(asset.get("type")), digest))
        stats["logical_media_bytes"] += size
        inode_key = (after.st_dev, after.st_ino)
        if inode_key not in inode_set:
            inode_set.add(inode_key)
            stats["unique_media_inode_bytes"] += size
        stats["assets"] += 1
    stats["revisions"] += 1


def import_archive(db, root, job, stats, inode_set):
    if not isinstance(job, dict):
        raise MigrationError("Malformed job")
    job_id = part(job.get("id"), "job ID")
    owner = part(job.get("ownerId"), "owner ID")
    author = part(job.get("author"), "author")
    post_id = part(job.get("postId"), "post ID")
    state = job.get("state")
    if state not in STATES:
        raise MigrationError(f"Unknown job state {state!r}: {job_id}")
    db.execute("INSERT OR IGNORE INTO source_posts (author_slug,remote_post_id,first_seen_at) VALUES (?,?,?)",
               (author, post_id, text(job.get("createdAt"))))
    post_key = db.execute("SELECT post_key FROM source_posts WHERE author_slug=? AND remote_post_id=?",
                          (author, post_id)).fetchone()[0]
    db.execute("""INSERT INTO owner_archives
        (archive_id,owner_id,post_key,title,job_state,created_at,updated_at,archived_at,
         deleted_at,auto_sync,shared_for_all,payload_json)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?)""", (job_id, owner, post_key, text(job.get("title")), state,
        text(job.get("createdAt")), text(job.get("updatedAt")), text(job.get("archiveUpdatedAt")) or None,
        text(job.get("deletedAt")) or None, int(bool(job.get("autoSync"))), int(bool(job.get("sharedForAll"))),
        json.dumps(job, ensure_ascii=False, sort_keys=True, separators=(",", ":"))))
    grants = job.get("sharedWith") or []
    if not isinstance(grants, list):
        raise MigrationError(f"Invalid grants: {job_id}")
    for grantee in grants:
        grantee = part(grantee, "grantee ID")
        db.execute("INSERT INTO archive_grants (archive_id,grantee_owner_id,created_at) VALUES (?,?,?)",
                   (job_id, grantee, text(job.get("updatedAt"))))
        stats["grants"] += 1
    live = Path("library") / owner / author / post_id
    if (root / live / "manifest.json").exists() or (root / live / "manifest.json").is_symlink():
        checked_snapshot(db, root, job, live, "current:" + job_id, True, stats, inode_set)
    elif state in ("done", "partial"):
        raise MigrationError(f"Completed job lacks a manifest: {job_id}")
    history = Path("revisions") / owner / author / post_id
    if (root / history).exists() or (root / history).is_symlink():
        for n in range(1, len(history.parts) + 1):
            directory(root.joinpath(*history.parts[:n]))
        for entry in sorted((root / history).iterdir()):
            if entry.name.startswith(".incomplete-"):
                continue
            if not REV.fullmatch(entry.name):
                raise MigrationError(f"Unexpected revision entry: {entry}")
            directory(entry)
            checked_snapshot(db, root, job, history / entry.name, entry.name, False, stats, inode_set)
    stats["archives"] += 1


def import_jobs(db, root, stats):
    path = root / "imports.json"
    if not path.exists() and not path.is_symlink():
        return None
    source = checked_json(root, path)
    if not isinstance(source, dict):
        raise MigrationError("imports.json must be a JSON object")
    start_sha, _ = digest_file(path)
    for key, item in sorted(source.items()):
        if not isinstance(item, dict) or item.get("id") != key:
            raise MigrationError(f"Import key/ID mismatch: {key!r}")
        db.execute("""INSERT INTO author_imports
            (import_id,owner_id,author_slug,job_state,auto_sync,updated_at,payload_json)
            VALUES (?,?,?,?,?,?,?)""",
            (part(key, "import ID"), part(item.get("ownerId"), "import owner"),
             part(item.get("author"), "import author"), text(item.get("state")),
             int(bool(item.get("autoSync"))), text(item.get("updatedAt")),
             json.dumps(item, ensure_ascii=False, sort_keys=True, separators=(",", ":"))))
        stats["imports"] += 1
    return start_sha


def migrate(root, schema, output=None):
    directory(root)
    regular(schema)
    schema_sql = schema.read_text(encoding="utf-8")
    schema_hash = hashlib.sha256(schema_sql.encode("utf-8")).hexdigest()
    jobs_path = root / "jobs.json"
    jobs = checked_json(root, jobs_path)
    if not isinstance(jobs, dict):
        raise MigrationError("jobs.json must be a JSON object")
    start_sha, _ = digest_file(jobs_path)
    if output is not None:
        output = output.absolute()
        if output.exists() or output.is_symlink():
            raise MigrationError(f"Refusing to overwrite: {output}")
        directory(output.parent)
        fd, tmp_name = tempfile.mkstemp(prefix=".boosty-sqlite-", suffix=".db", dir=output.parent)
        os.close(fd)
        tmp = Path(tmp_name)
    else:
        tmp = None
    db = None
    try:
        db = sqlite3.connect(str(tmp) if tmp else ":memory:")
        db.execute("PRAGMA foreign_keys=ON")
        db.executescript(schema_sql)
        db.execute("BEGIN IMMEDIATE")
        db.execute("INSERT INTO schema_migrations VALUES (?,?,?)",
                   (1, datetime.now(timezone.utc).isoformat(), schema_hash))
        stats = {"archives": 0, "revisions": 0, "assets": 0, "grants": 0,
                 "logical_media_bytes": 0, "unique_media_inode_bytes": 0, "imports": 0}
        inodes = set()
        for key in sorted(jobs):
            job = jobs[key]
            if not isinstance(job, dict) or job.get("id") != key:
                raise MigrationError(f"Job key/ID mismatch: {key!r}")
            import_archive(db, root, job, stats, inodes)
        imports_sha = import_jobs(db, root, stats)
        if imports_sha and digest_file(root / "imports.json")[0] != imports_sha:
            raise MigrationError("imports.json changed during migration; stop writers and retry")
        if digest_file(jobs_path)[0] != start_sha:
            raise MigrationError("jobs.json changed during migration; stop writers and retry")
        if db.execute("PRAGMA foreign_key_check").fetchone() is not None:
            raise MigrationError("SQLite foreign key verification failed")
        if db.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
            raise MigrationError("SQLite integrity check failed")
        if db.execute("SELECT COUNT(*) FROM owner_archives").fetchone()[0] != len(jobs):
            raise MigrationError("Archive count mismatch")
        db.commit()
        if tmp:
            db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            db.close()
            db = None
            os.chmod(tmp, 0o600)
            os.link(tmp, output)  # atomic create; never overwrite an existing DB
        return stats
    except (sqlite3.Error, OSError, ValueError, TypeError, KeyError) as exc:
        raise MigrationError(str(exc)) from exc
    finally:
        if db is not None:
            db.close()
        if tmp is not None:
            tmp.unlink(missing_ok=True)
            Path(str(tmp) + "-wal").unlink(missing_ok=True)
            Path(str(tmp) + "-shm").unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-dir", required=True, type=Path)
    parser.add_argument("--schema", type=Path, default=Path(__file__).resolve().parents[1] / "deploy/sqlite/schema_v1.sql")
    parser.add_argument("--output", type=Path, help="New DB file; must not exist; omission performs an in-memory check")
    args = parser.parse_args()
    try:
        result = migrate(args.data_dir.absolute(), args.schema.absolute(), args.output)
    except (MigrationError, OSError) as exc:
        print(f"SQLite migration blocked: {exc}", file=sys.stderr)
        return 1
    print(json.dumps({"mode": "export" if args.output else "check-only", **result}, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
