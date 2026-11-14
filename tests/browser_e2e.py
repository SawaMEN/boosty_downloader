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
from urllib.error import URLError
from urllib.request import urlopen

from playwright.sync_api import expect, sync_playwright

ADMIN_USER = "e2e-admin"
ADMIN_PASSWORD = "test-browser-password-123"


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


if __name__ == "__main__":
    unittest.main(verbosity=2)
