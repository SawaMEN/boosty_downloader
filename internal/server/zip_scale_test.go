package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestPortableZIPFilesBoundedLargeManifest(t *testing.T) {
	manifest := ArchiveManifest{Assets: make([]ArchiveAsset, 4998)}
	for i := range manifest.Assets {
		manifest.Assets[i].Filename = fmt.Sprintf("asset-%04d.mp4", i)
	}
	files, err := portableZipFiles(manifest)
	if err != nil {
		t.Fatalf("4998 assets must fit with two portable files: %v", err)
	}
	if len(files) != 5000 || !sort.StringsAreSorted(files) {
		t.Fatalf("incorrect large archive inventory: %d entries", len(files))
	}

	manifest.Assets = append(manifest.Assets, ArchiveAsset{Filename: "new.mp4"})
	if _, err := portableZipFiles(manifest); err == nil {
		t.Fatal("5001 unique portable ZIP entries were accepted")
	}

	// Repeated media names must not multiply ZIP entries or bypass the
	// absolute limit on the number of manifest records.
	manifest.Assets = make([]ArchiveAsset, 5000)
	for i := range manifest.Assets {
		manifest.Assets[i].Filename = "duplicate.mp4"
	}
	files, err = portableZipFiles(manifest)
	if err != nil || len(files) != 3 {
		t.Fatalf("duplicate manifest entry handling: %d files, %v", len(files), err)
	}
	manifest.Assets = append(manifest.Assets, ArchiveAsset{Filename: "duplicate.mp4"})
	if _, err := portableZipFiles(manifest); err == nil {
		t.Fatal("unbounded manifest entries accepted despite duplicate ZIP names")
	}
}

// Covers thousands of sequential O_NOFOLLOW descriptor opens with a real
// authenticated HTTP handler. A per-file descriptor leak or ZIP method
// regression must fail rather than silently passing a trivial one-file test.
func TestZIPThousandMediaEntries(t *testing.T) {
	const n = 1000
	app, err := New(Config{
		AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t), DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	owner := testLogin(t, app, "admin", "strong-password-123")
	j := Job{
		ID: strings.Repeat("c", 24), OwnerID: "admin",
		Author: "creator", PostID: "many-small-media",
		Title: "Media stress fixture", State: "done",
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
	manifest := ArchiveManifest{
		Title: j.Title, Author: j.Author, PostID: j.PostID,
		CapturedAt: time.Now().UTC(),
		Assets: make([]ArchiveAsset, n),
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("chunk-%04d.bin", i)
		data := []byte(fmt.Sprintf("payload-%04d", i))
		manifest.Assets[i] = ArchiveAsset{
			Filename: name, Type: "file", Size: int64(len(data)),
		}
		if err := os.WriteFile(filepath.Join(mediaDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSON(filepath.Join(base, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "index.html"), []byte("<h1>Many media</h1>"), 0600); err != nil {
		t.Fatal(err)
	}

	raw := testArchiveResponse(t, app, owner, "GET",
		"/api/jobs/"+j.ID+"/zip", "", http.StatusOK)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != n+2 {
		t.Fatalf("expected %d ZIP files, got %d", n+2, len(zr.File))
	}
	for _, item := range zr.File {
		if !strings.HasPrefix(item.Name, "media/") {
			continue
		}
		if item.Method != zip.Store {
			t.Fatalf("%s: expected method STORE, got %d", item.Name, item.Method)
		}
		r, err := item.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(r)
		closeErr := r.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("invalid %s: %v, %v", item.Name, readErr, closeErr)
		}
		if len(body) != len("payload-0000") {
			t.Fatalf("incomplete archived file %s", item.Name)
		}
	}
}
