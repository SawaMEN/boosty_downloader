package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hashedZIPFixture(t *testing.T, payload []byte) (*App, *http.Cookie, string, string, ArchiveManifest) {
	t.Helper()
	app, err := New(Config{
		AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t), DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	cookie := testLogin(t, app, "admin", "strong-password-123")
	j := Job{
		ID: strings.Repeat("e", 24), OwnerID: "admin",
		Author: "creator", PostID: "hashed-media",
		Title: "Hashed media", State: "done",
		CreatedAt: time.Now().UTC(),
	}
	if err := app.store.upsert(j); err != nil {
		t.Fatal(err)
	}
	base := app.store.archiveDir(j.OwnerID, j.Author, j.PostID)
	mediaDir := filepath.Join(base, "media")
	if err := os.MkdirAll(mediaDir, 0700); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(mediaDir, "video.bin")
	if err := os.WriteFile(clip, payload, 0600); err != nil {
		t.Fatal(err)
	}
	man := ArchiveManifest{
		Title: j.Title, Author: j.Author, PostID: j.PostID,
		CapturedAt: time.Now().UTC(),
		Assets:     []ArchiveAsset{{Filename: "video.bin", Type: "file", Size: int64(len(payload)), SHA256: fmt.Sprintf("%x", sha256.Sum256(payload))}},
	}
	if err := writeJSON(filepath.Join(base, "manifest.json"), man); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "index.html"), []byte("<h1>Hash fixture</h1>"), 0600); err != nil {
		t.Fatal(err)
	}
	return app, cookie, clip, "/api/jobs/" + j.ID + "/zip", man
}

func zipMediaPayload(t *testing.T, raw []byte) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("ZIP lacks its valid central directory: %v", err)
	}
	for _, entry := range reader.File {
		if entry.Name != "media/video.bin" {
			continue
		}
		if entry.Method != zip.Store {
			t.Fatalf("hashed media must be streamed with zip.Store; got %d", entry.Method)
		}
		stream, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		closeErr := stream.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("media cannot be extracted: %v, %v", err, closeErr)
		}
		return data
	}
	t.Fatal("media missing from ZIP")
	return nil
}

func TestZIPStreamingSHA256RejectsSameSizeCorruption(t *testing.T) {
	original := bytes.Repeat([]byte{'A'}, 128<<10)
	app, cookie, clip, path, man := hashedZIPFixture(t, original)
	raw := testArchiveResponse(t, app, cookie, "GET", path, "", http.StatusOK)
	if got := zipMediaPayload(t, raw); !bytes.Equal(got, original) {
		t.Fatal("intact hashed media changed in ZIP")
	}

	// Preflight still sees the exact expected size. Streaming must never
	// publish a valid ZIP central directory if these bytes differ.
	mutated := bytes.Repeat([]byte{'B'}, len(original))
	if err := os.WriteFile(clip, mutated, 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, reqAs("GET", path, "", cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("hash mismatch response code changed: %d", rec.Code)
	}
	if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err == nil {
		t.Fatal("a valid ZIP was finalized despite a same-size SHA-256 mismatch")
	}

	if err := os.WriteFile(clip, original, 0600); err != nil {
		t.Fatal(err)
	}
	raw = testArchiveResponse(t, app, cookie, "GET", path, "", http.StatusOK)
	if got := zipMediaPayload(t, raw); !bytes.Equal(got, original) {
		t.Fatal("restored media did not export correctly")
	}

	base := filepath.Dir(filepath.Dir(clip))
	man.Assets[0].SHA256 = "not-a-valid-sha256"
	if err := writeJSON(filepath.Join(base, "manifest.json"), man); err != nil {
		t.Fatal(err)
	}
	testArchiveResponse(t, app, cookie, "GET", path, "", http.StatusConflict)

	man.Assets[0].SHA256 = fmt.Sprintf("%x", sha256.Sum256(original))
	man.Assets = append(man.Assets, ArchiveAsset{Filename: "video.bin", Size: int64(len(original)), SHA256: strings.Repeat("0", 64)})
	if err := writeJSON(filepath.Join(base, "manifest.json"), man); err != nil {
		t.Fatal(err)
	}
	testArchiveResponse(t, app, cookie, "GET", path, "", http.StatusConflict)
}

type zipMutationRecorder struct {
	*httptest.ResponseRecorder
	inject func() error
	fired  bool
	err    error
}

func (w *zipMutationRecorder) Write(chunk []byte) (int, error) {
	// A STORE entry emits literal media bytes. Only mutate once the first
	// media chunk is on the wire, after the file descriptor was opened.
	if !w.fired && bytes.Contains(chunk, bytes.Repeat([]byte{'A'}, 64)) {
		w.fired = true
		w.err = w.inject()
	}
	return w.ResponseRecorder.Write(chunk)
}

func TestZIPConcurrentInPlaceMutationCannotFinalize(t *testing.T) {
	original := bytes.Repeat([]byte{'A'}, 2<<20)
	app, cookie, clip, path, _ := hashedZIPFixture(t, original)
	rec := &zipMutationRecorder{ResponseRecorder: httptest.NewRecorder()}
	rec.inject = func() error {
		f, err := os.OpenFile(clip, os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.WriteAt(bytes.Repeat([]byte{'B'}, 16<<10), int64(len(original)-(16<<10)))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	app.Handler().ServeHTTP(rec, reqAs("GET", path, "", cookie))
	if !rec.fired || rec.err != nil {
		t.Fatalf("concurrent in-place mutation was not injected: fired=%t, error=%v", rec.fired, rec.err)
	}
	if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err == nil {
		t.Fatal("concurrently corrupted media produced a valid ZIP")
	}

	// Atomic replacement of the pathname is different: a descriptor already
	// opened by the reader retains the intact old inode and hashes cleanly.
	if err := os.WriteFile(clip, original, 0600); err != nil {
		t.Fatal(err)
	}
	atomicRec := &zipMutationRecorder{ResponseRecorder: httptest.NewRecorder()}
	atomicRec.inject = func() error {
		temp := clip + ".replacement"
		if err := os.WriteFile(temp, bytes.Repeat([]byte{'C'}, len(original)), 0600); err != nil {
			return err
		}
		return os.Rename(temp, clip)
	}
	app.Handler().ServeHTTP(atomicRec, reqAs("GET", path, "", cookie))
	if !atomicRec.fired || atomicRec.err != nil {
		t.Fatalf("atomic replacement was not injected: fired=%t, error=%v", atomicRec.fired, atomicRec.err)
	}
	if got := zipMediaPayload(t, atomicRec.Body.Bytes()); !bytes.Equal(got, original) {
		t.Fatal("atomic replacement changed a ZIP whose media descriptor was pinned")
	}
}
