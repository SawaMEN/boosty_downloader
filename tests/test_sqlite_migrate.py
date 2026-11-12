"""Offline SQLite migration tests: owners, ACL, CAS, corruption, and paths."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
from sqlite_migrate import MigrationError, migrate  # noqa: E402

SCHEMA = ROOT / "deploy/sqlite/schema_v1.sql"


class SQLiteMigrationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.data = Path(self.temp.name) / "data"
        self.data.mkdir()
        self.database = Path(self.temp.name) / "metadata.db"

    def write(self, name, content):
        path = self.data / name
        path.parent.mkdir(parents=True, exist_ok=True)
        if isinstance(content, (dict, list)):
            content = json.dumps(content).encode()
        elif isinstance(content, str):
            content = content.encode()
        path.write_bytes(content)
        return path

    def fixture(self):
        body = b"video body\x00video body\x00"
        digest = hashlib.sha256(body).hexdigest()
        cas = self.write(f"blobs/sha256/{digest[:2]}/{digest}", body)
        jobs = {}
        for n, owner in enumerate(("first", "second")):
            job = {
                "id": str(n + 1) * 24, "ownerId": owner,
                "author": "creator", "postId": "post-1", "state": "done",
                "title": "An archive", "createdAt": "2026-11-01T10:00:00Z",
                "updatedAt": "2026-11-01T11:00:00Z",
                "archiveUpdatedAt": "2026-11-01T10:30:00Z",
                "autoSync": True,
                "sharedWith": ["second"] if n == 0 else [],
            }
            jobs[job["id"]] = job
            base = f"library/{owner}/creator/post-1"
            media = self.data / base / "media" / "video.mp4"
            media.parent.mkdir(parents=True, exist_ok=True)
            os.link(cas, media)
            manifest = {
                "author": "creator", "postId": "post-1",
                "title": "Post title", "capturedAt": "2026-11-01T10:30:00Z",
                "assets": [{
                    "filename": "video.mp4", "size": len(body),
                    "sha256": digest, "type": "ok_video",
                }],
            }
            self.write(f"{base}/manifest.json", manifest)
            self.write(f"{base}/index.html", "<h1>Offline</h1>")
        revision = f"revisions/first/creator/post-1/{'c' * 24}"
        self.write(f"{revision}/manifest.json", manifest)
        self.write(f"{revision}/index.html", "<h1>Older</h1>")
        (self.data / revision / "media").mkdir()
        os.link(cas, self.data / revision / "media/video.mp4")
        self.write("jobs.json", jobs)
        self.write("imports.json", {
            "a" * 24: {
                "id": "a" * 24, "ownerId": "first", "author": "creator",
                "state": "done", "updatedAt": "2026-11-01T10:00:00Z",
            }
        })
        return jobs, body, digest

    def test_two_owners_one_post_no_grant_leak(self):
        _, body, digest = self.fixture()
        totals = migrate(self.data, SCHEMA, self.database)
        self.assertEqual(
            tuple(totals[k] for k in ("archives", "revisions", "assets", "grants", "imports")),
            (2, 3, 3, 1, 1),
        )
        with sqlite3.connect(self.database) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM source_posts").fetchone()[0], 1)
            self.assertEqual(db.execute("SELECT count(*) FROM owner_archives").fetchone()[0], 2)
            self.assertEqual(db.execute("SELECT count(*) FROM media_blobs").fetchone()[0], 1)
            self.assertEqual(
                db.execute("SELECT relative_path FROM media_blobs").fetchone()[0],
                f"blobs/sha256/{digest[:2]}/{digest}",
            )
            self.assertEqual(
                db.execute("SELECT archive_id FROM archive_grants").fetchone()[0], "1" * 24
            )
            self.assertEqual(
                db.execute("SELECT owner_id FROM owner_archives WHERE archive_id=?", ("2" * 24,)).fetchone()[0],
                "second",
            )
            self.assertEqual(
                json.loads(db.execute("SELECT payload_json FROM author_imports").fetchone()[0])["ownerId"],
                "first",
            )
            self.assertEqual(db.execute("PRAGMA integrity_check").fetchone()[0], "ok")
            self.assertEqual(db.execute("PRAGMA foreign_key_check").fetchall(), [])
        self.assertEqual(
            (self.data / "library/second/creator/post-1/media/video.mp4").read_bytes(), body
        )
        with self.assertRaisesRegex(MigrationError, "Refusing to overwrite"):
            migrate(self.data, SCHEMA, self.database)

    def test_corrupt_media_blocks_export(self):
        self.fixture()
        self.write("library/first/creator/post-1/media/video.mp4", b"corrupted bytes")
        with self.assertRaisesRegex(MigrationError, "(SHA-256 mismatch|Size mismatch)"):
            migrate(self.data, SCHEMA, self.database)
        self.assertFalse(self.database.exists())

    def test_path_traversal_and_symlink_are_rejected(self):
        jobs, _, _ = self.fixture()
        jobs["1" * 24]["author"] = ".."
        self.write("jobs.json", jobs)
        with self.assertRaisesRegex(MigrationError, "Unsafe"):
            migrate(self.data, SCHEMA, self.database)
        jobs["1" * 24]["author"] = "creator"
        self.write("jobs.json", jobs)
        media = self.data / "library/second/creator/post-1/media/video.mp4"
        media.unlink()
        media.symlink_to("/etc/passwd")
        with self.assertRaisesRegex(MigrationError, "Not a regular file"):
            migrate(self.data, SCHEMA, self.database)

    def test_busy_data_directory_rejects_export_and_check_only(self):
        self.fixture()
        descriptor = os.open(self.data, os.O_RDONLY | os.O_DIRECTORY)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            for output in (None, self.database):
                with self.assertRaisesRegex(MigrationError, "locked"):
                    migrate(self.data, SCHEMA, output)
                self.assertFalse(self.database.exists())
        finally:
            os.close(descriptor)
        result = migrate(self.data, SCHEMA, self.database)
        self.assertEqual(result["archives"], 2)
        self.assertTrue(self.database.exists())

    def test_no_cas_and_check_only(self):
        _, body, digest = self.fixture()
        (self.data / f"blobs/sha256/{digest[:2]}/{digest}").unlink()
        result = migrate(self.data, SCHEMA)
        self.assertEqual(result["assets"], 3)
        self.assertFalse(self.database.exists())
        self.database.write_text("existing data")
        with self.assertRaisesRegex(MigrationError, "Refusing"):
            migrate(self.data, SCHEMA, self.database)
        self.assertEqual(self.database.read_text(), "existing data")


if __name__ == "__main__":
    unittest.main()
