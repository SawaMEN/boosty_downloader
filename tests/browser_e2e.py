#!/usr/bin/env python3
"""Offline browser E2E against the real HTTPS app, with only Boosty API fixtures.

The server uses an isolated temporary data directory. No credentials, post
contents or network requests are sent to a real Boosty account or VPS.
"""

import base64
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import ssl
import subprocess
import tempfile
import time
import unittest
import zipfile
from urllib.error import URLError
from urllib.request import urlopen

from playwright.sync_api import expect, sync_playwright

ADMIN_USER = "e2e-admin"
ADMIN_PASSWORD = "test-browser-password-123"

ARCHIVE_JOB_ID = "d1d2d3d4d5d6d7d8d9dadbdc"
ARCHIVE_REVISION_ID = "a1a2a3a4a5a6a7a8a9aaabac"
ARCHIVE_MEMBER = "browser-member"
ARCHIVE_MEMBER_PASSWORD = "browser-member-password-123"


def seed_browser_archive(state):
    """Seed a finished, private archive and an older revision before startup.

    These files are local test data, not mocked responses from the archive API.
    The browser exercises real permissions, storage, ZIP generation and restore.
    """
    state.mkdir(mode=0o700, parents=True, exist_ok=True)

    def write(path, content):
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        path.write_bytes(content if isinstance(content, bytes) else content.encode("utf-8"))
        path.chmod(0o600)

    created = "2026-11-13T10:00:00Z"
    updated = "2026-11-14T10:00:00Z"
    job = {
        "id": ARCHIVE_JOB_ID, "ownerId": "admin",
        "author": "browserfixture", "postId": "archive-test",
        "url": "https://boosty.to/browserfixture/posts/archive-test",
        "title": "Latest browser fixture", "state": "done",
        "createdAt": created, "updatedAt": updated,
        "archiveUpdatedAt": updated, "doneFiles": 1, "totalFiles": 1,
    }
    write(state / "jobs.json", json.dumps({ARCHIVE_JOB_ID: job}).encode())
    base = state / "library" / "admin" / "browserfixture" / "archive-test"
    older = state / "revisions" / "admin" / "browserfixture" / "archive-test" / ARCHIVE_REVISION_ID

    def create_copy(path, title, marker, captured):
        media = ("MEDIA_" + marker).encode()
        manifest = {
            "title": title, "author": "browserfixture", "postId": "archive-test",
            "source": job["url"], "capturedAt": captured,
            "assets": [{
                "filename": "attachment.txt", "type": "file",
                "title": "Attachment", "size": len(media),
            }],
            "contentSha256": "PRIVATE_BROWSER_FINGERPRINT",
            "subscription": {
                "level": "PRIVATE_BROWSER_TIER", "paid": True,
                "observedAt": captured, "source": "boosty_subscriptions",
            },
            "commentsStatus": "не запрашивались",
        }
        write(path / "manifest.json", json.dumps(manifest).encode())
        write(path / "index.html", (
            '<!doctype html><html><body><h1>' + marker +
            '</h1><a href="media/attachment.txt">Attachment</a></body></html>'
        ))
        write(path / "media" / "attachment.txt", media)

    create_copy(base, "Latest browser fixture", "LATEST_BROWSER_ARCHIVE", updated)
    create_copy(older, "Older browser fixture", "OLDER_BROWSER_ARCHIVE", created)
    # Raw API payloads and incomplete media must never enter downloadable ZIPs.
    write(base / "post.json", '{"private":"RAW_BROWSER_CDN_SECRET"}')
    write(base / "media" / "unfinished.part", "PARTIAL_BROWSER_FILE")



def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class BrowserE2E(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        binary = os.environ.get("BOOSTY_E2E_BIN")
        if not binary or not Path(binary).is_file():
            raise RuntimeError("BOOSTY_E2E_BIN must point to a built boosty-downloader")
        cls.tmp = tempfile.TemporaryDirectory(prefix="boosty-browser-e2e-")
        root = Path(cls.tmp.name)
        seed_browser_archive(root / "state")
        cls.browser = None
        cls.server = None
        try:
            cert, key = root / "cert.pem", root / "key.pem"
            subprocess.run(
                ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                 "-keyout", str(key), "-out", str(cert), "-days", "1",
                 "-subj", "/CN=localhost"],
                check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                timeout=20,
            )
            cls.port = free_port()
            cls.url = "https://127.0.0.1:" + str(cls.port)
            env = os.environ.copy()
            env.update({
                "BD_LISTEN": "127.0.0.1:" + str(cls.port),
                "BD_DATA_DIR": str(root / "state"),
                "BD_ADMIN_USER": ADMIN_USER,
                "BD_ADMIN_PASSWORD": ADMIN_PASSWORD,
                "BD_SECRET_KEY": base64.b64encode(secrets.token_bytes(32)).decode(),
                "BD_TLS_CERT": str(cert),
                "BD_TLS_KEY": str(key),
            })
            cls.logs = (root / "server.log").open("wb")
            cls.server = subprocess.Popen(
                [binary], env=env, stdout=cls.logs, stderr=subprocess.STDOUT,
            )
            context = ssl._create_unverified_context()
            for _ in range(100):
                if cls.server.poll() is not None:
                    raise RuntimeError("server exited early: " + (root / "server.log").read_text(errors="replace"))
                try:
                    with urlopen(cls.url + "/healthz", context=context, timeout=1) as response:
                        if response.status == 200:
                            break
                except (OSError, URLError):
                    time.sleep(0.1)
            else:
                raise RuntimeError("server did not serve /healthz")
            cls.playwright = sync_playwright().start()
            browser_args = ["--no-sandbox"]
            local_binary = os.environ.get("BOOSTY_E2E_CHROMIUM")
            kwargs = {"headless": True, "args": browser_args}
            if local_binary:
                kwargs["executable_path"] = local_binary
            cls.browser = cls.playwright.chromium.launch(**kwargs)
        except Exception:
            cls.tearDownClass()
            raise

    @classmethod
    def tearDownClass(cls):
        if getattr(cls, "browser", None):
            cls.browser.close()
        if getattr(cls, "playwright", None):
            cls.playwright.stop()
        if getattr(cls, "server", None):
            cls.server.terminate()
            try:
                cls.server.wait(timeout=5)
            except subprocess.TimeoutExpired:
                cls.server.kill()
                cls.server.wait(timeout=5)
        if getattr(cls, "logs", None):
            cls.logs.close()
        if getattr(cls, "tmp", None):
            cls.tmp.cleanup()

    def setUp(self):
        self.context = self.browser.new_context(ignore_https_errors=True)
        self.page = self.context.new_page()
        self.page.set_default_timeout(12000)
        self.context.on("page", lambda new_page: new_page.close())

    def tearDown(self):
        self.context.close()

    def login(self):
        page = self.page
        page.goto(self.url, wait_until="domcontentloaded")
        expect(page.locator("#loginScreen")).to_be_visible()
        page.locator("#loginUser").fill(ADMIN_USER)
        page.locator("#loginPass").fill(ADMIN_PASSWORD)
        page.locator("#loginForm button[type=submit]").click()
        expect(page.locator("#appScreen")).to_be_visible()
        expect(page.locator("#loginScreen")).to_be_hidden()
        expect(page.locator("#userName")).to_have_text(ADMIN_USER)

    def test_login_navigation_logout_revokes_session(self):
        page = self.page
        page.goto(self.url)
        response = self.context.request.get(self.url + "/api/jobs")
        self.assertEqual(response.status, 401, "unauthenticated API must not return jobs")
        self.login()
        page.locator('#navigation [data-page="library"]').click()
        expect(page.locator("#page-library")).to_be_visible()
        expect(page.locator("#archiveSearchStatus")).to_contain_text("Найдено:")
        page.locator('#navigation [data-page="authors"]').click()
        expect(page.locator("#page-authors")).to_be_visible()
        page.locator("#logout").click()
        expect(page.locator("#loginScreen")).to_be_visible()
        self.assertEqual(self.context.request.get(self.url + "/api/jobs").status, 401,
                         "logout must revoke the server session, not just hide the UI")
        page.reload()
        expect(page.locator("#loginScreen")).to_be_visible()

    def test_post_comments_and_catalog_escape_untrusted_text(self):
        page = self.page
        payload = '<img src=x onerror="window.__browser_xss=1">'
        source = "https://boosty.to/creator/posts/offline-fixture"

        def mock_post(route):
            route.fulfill(json={
                "title": payload, "author": "<svg onload=alert(1)>",
                "hasAccess": True, "source": source,
                "media": [], "comments": 2,
                "blocks": [
                    {"type": "header", "text": "Заголовок " + payload},
                    {"type": "text", "text": "Text before <script>bad()</script>"},
                    {"type": "unsupported", "text": "SECRET_UNSUPPORTED_RAW_BLOCK"},
                    {"type": "text", "text": "Text after"},
                ],
            })

        def mock_comments(route):
            route.fulfill(json={
                "received": 2, "partial": True,
                "reason": "Boosty returned fewer replies",
                "comments": [{
                    "author": "Comment author " + payload,
                    "text": "Safe literal <script>blocked()</script>",
                    "createdAt": 0,
                    "replies": [{"author": "Child", "text": "Child reply", "replies": []}],
                }],
            })

        page.route(re.compile(r"/api/post[?]"), mock_post)
        page.route(re.compile(r"/api/comments[?]"), mock_comments)
        self.login()
        page.locator("#postUrl").fill(source)
        page.locator("#openForm button[type=submit]").click()
        expect(page.locator("#postViewer")).to_be_visible()
        expect(page.locator("#postTitle")).to_have_text(payload)
        expect(page.locator("#postBlocks")).to_contain_text("Text after")
        expect(page.locator("#postBlocks")).to_contain_text("Неподдерживаемый блок Boosty")
        self.assertNotIn("SECRET_UNSUPPORTED_RAW_BLOCK", page.locator("#postBlocks").inner_text())
        self.assertEqual(page.locator("#postBlocks img, #postBlocks script").count(), 0,
                         "malicious post text must never create elements")

        page.locator("#loadComments").click()
        expect(page.locator("#commentsArea")).to_contain_text("Boosty returned fewer replies")
        expect(page.locator("#commentsArea")).to_contain_text("Child reply")
        self.assertEqual(page.locator("#commentsArea .comment-item").count(), 2)
        self.assertEqual(page.locator("#commentsArea img, #commentsArea script").count(), 0)

        def mock_catalog(route):
            route.fulfill(json={
                "page": 1, "pages": 1, "total": 1,
                "items": [{
                    "id": "aaaaaaaaaaaaaaaaaaaaaaaa",
                    "title": "Archived " + payload,
                    "url": source, "author": "creator", "postId": "offline-fixture",
                    "state": "done", "doneFiles": 0, "totalFiles": 0,
                    "createdAt": "2026-11-14T00:00:00Z",
                }],
            })

        page.route(re.compile(r"/api/catalog([?]|$)"), mock_catalog)
        page.locator('#navigation [data-page="library"]').click()
        expect(page.locator("#libraryJobs .job-title")).to_contain_text("Archived " + payload)
        self.assertEqual(page.locator("#libraryJobs img, #libraryJobs script").count(), 0)
        self.assertIsNone(page.evaluate("window.__browser_xss || null"))

    def test_mobile_navigation_on_real_ui(self):
        self.context.close()
        self.context = self.browser.new_context(
            ignore_https_errors=True, viewport={"width": 390, "height": 844},
            is_mobile=True, has_touch=True,
        )
        self.page = self.context.new_page()
        self.page.set_default_timeout(12000)
        self.login()
        page = self.page
        page.locator("#mobileMenu").click()
        expect(page.locator("#sidebar")).to_have_class(re.compile(r"(^| )open( |$)"))
        page.locator('#navigation [data-page="authors"]').click()
        expect(page.locator("#page-authors")).to_be_visible()
        expect(page.locator("#sidebar")).not_to_have_class(re.compile(r"(^| )open( |$)"))


    def test_z_archive_download_sharing_revision_and_trash(self):
        """Real browser + HTTPS server, using only local filesystem fixtures."""
        archive = "/api/jobs/" + ARCHIVE_JOB_ID
        self.assertEqual(self.context.request.get(self.url + archive + "/zip").status, 401)
        self.login()
        page = self.page
        page.locator('#navigation [data-page="library"]').click()
        owner_card = page.locator("#libraryJobs .job-item").filter(has_text="Latest browser fixture")
        expect(owner_card).to_have_count(1)

        with page.expect_download() as owner_download:
            owner_card.get_by_role("link", name="Скачать ZIP").click()
        owner_zip = Path(self.tmp.name) / "owner-archive.zip"
        owner_download.value.save_as(str(owner_zip))
        with zipfile.ZipFile(owner_zip) as zf:
            self.assertEqual(set(zf.namelist()), {
                "index.html", "manifest.json", "media/attachment.txt",
            })
            self.assertIn(b"LATEST_BROWSER_ARCHIVE", zf.read("index.html"))
            self.assertEqual(zf.read("media/attachment.txt"), b"MEDIA_LATEST_BROWSER_ARCHIVE")
            self.assertIn("PRIVATE_BROWSER_TIER", zf.read("manifest.json").decode())
            self.assertNotIn(b"RAW_BROWSER_CDN_SECRET", owner_zip.read_bytes())
            self.assertNotIn(b"PARTIAL_BROWSER_FILE", owner_zip.read_bytes())

        member_context = self.browser.new_context(
            ignore_https_errors=True, accept_downloads=True,
        )
        try:
            # Enable registration through the authenticated admin API.
            enabled = self.context.request.post(
                self.url + "/api/admin/registration",
                headers={"Content-Type": "application/json", "X-BD-Request": "1"},
                data=json.dumps({"registrationOpen": True}),
            )
            self.assertEqual(enabled.status, 200, enabled.text())
            member_page = member_context.new_page()
            member_page.set_default_timeout(12000)
            member_page.goto(self.url)
            expect(member_page.locator("#switchAuth")).to_be_visible()
            member_page.locator("#switchAuth").click()
            member_page.locator("#loginUser").fill(ARCHIVE_MEMBER)
            member_page.locator("#loginPass").fill(ARCHIVE_MEMBER_PASSWORD)
            member_page.locator("#loginForm button[type=submit]").click()
            expect(member_page.locator("#appScreen")).to_be_visible()

            # No implicit access to other users' manifests, files, ZIP or revisions.
            for suffix in ("/manifest", "/zip", "/revisions"):
                self.assertEqual(member_context.request.get(self.url + archive + suffix).status, 404)
            self.assertEqual(member_context.request.get(
                self.url + "/library/" + ARCHIVE_JOB_ID + "/index.html"
            ).status, 404)

            owner_card.get_by_role("button", name="Поделиться").click()
            expect(page.locator("#shareDialog")).to_be_visible()
            page.locator("#shareUsername").fill(ARCHIVE_MEMBER)
            page.locator('#shareForm button[type=submit]').click()
            expect(page.locator("#shareParticipants")).to_contain_text(ARCHIVE_MEMBER)

            member_page.locator('#navigation [data-page="shared"]').click()
            shared_card = member_page.locator("#sharedJobs .job-item").filter(
                has_text="Latest browser fixture"
            )
            expect(shared_card).to_have_count(1)
            expect(shared_card).to_contain_text(ADMIN_USER)
            self.assertEqual(member_context.request.get(
                self.url + "/library/" + ARCHIVE_JOB_ID + "/index.html"
            ).status, 200)
            manifest_response = member_context.request.get(self.url + archive + "/manifest")
            self.assertEqual(manifest_response.status, 200)
            self.assertNotIn("PRIVATE_BROWSER_TIER", manifest_response.text())
            self.assertNotIn("PRIVATE_BROWSER_FINGERPRINT", manifest_response.text())
            self.assertEqual(member_context.request.get(
                self.url + archive + "/revisions"
            ).status, 404)

            with member_page.expect_download() as member_download:
                shared_card.get_by_role("link", name="Скачать ZIP").click()
            shared_zip = Path(self.tmp.name) / "shared-archive.zip"
            member_download.value.save_as(str(shared_zip))
            with zipfile.ZipFile(shared_zip) as zf:
                self.assertEqual(set(zf.namelist()), {
                    "index.html", "manifest.json", "media/attachment.txt",
                })
                private_data = zf.read("manifest.json").decode()
                self.assertNotIn("PRIVATE_BROWSER_TIER", private_data)
                self.assertNotIn("PRIVATE_BROWSER_FINGERPRINT", private_data)
                self.assertIn(b"LATEST_BROWSER_ARCHIVE", zf.read("index.html"))

            # Revocation applies immediately to catalog, files and downloads.
            page.once("dialog", lambda dialog: dialog.accept())
            page.locator("#shareParticipants").get_by_role(
                "button", name="Отозвать"
            ).click()
            expect(page.locator("#shareParticipants")).not_to_contain_text(ARCHIVE_MEMBER)
            member_page.locator('#navigation [data-page="shared"]').click()
            expect(member_page.locator("#sharedJobs")).not_to_contain_text(
                "Latest browser fixture"
            )
            for suffix in ("/manifest", "/zip"):
                self.assertEqual(member_context.request.get(self.url + archive + suffix).status, 404)

            # Restoring a historical version updates real bytes and catalog data.
            page.locator("#closeShare").click()
            owner_card.get_by_role("button", name="История версий").click()
            old_revision = page.locator("#revisionsList .participant-row").filter(
                has_text="Older browser fixture"
            )
            expect(old_revision).to_have_count(1)
            page.once("dialog", lambda dialog: dialog.accept())
            old_revision.get_by_role("button", name="Восстановить").click()
            expect(page.locator("#revisionsList")).to_contain_text("Latest browser fixture")
            page.locator("#closeRevisions").click()
            restored_card = page.locator("#libraryJobs .job-item").filter(
                has_text="Older browser fixture"
            )
            expect(restored_card).to_have_count(1)
            self.assertIn("OLDER_BROWSER_ARCHIVE", self.context.request.get(
                self.url + "/library/" + ARCHIVE_JOB_ID + "/index.html"
            ).text())
            self.assertEqual(member_context.request.get(
                self.url + archive + "/manifest"
            ).status, 404)

            # Trash/restore does not recreate previously revoked grants.
            page.once("dialog", lambda dialog: dialog.accept())
            restored_card.get_by_role("button", name="Удалить", exact=True).click()
            expect(page.locator("#trashJobs")).to_contain_text("Older browser fixture")
            self.assertEqual(self.context.request.get(self.url + archive + "/zip").status, 404)
            page.locator("#trashJobs .job-item").filter(
                has_text="Older browser fixture"
            ).get_by_role("button", name="Восстановить").click()
            expect(page.locator("#libraryJobs")).to_contain_text("Older browser fixture")
            self.assertEqual(self.context.request.get(self.url + archive + "/zip").status, 200)
            self.assertEqual(member_context.request.get(self.url + archive + "/zip").status, 404)
        finally:
            member_context.close()


if __name__ == "__main__":
    unittest.main(verbosity=2)
