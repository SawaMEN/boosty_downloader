package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func videoPublicationFixture(t *testing.T) (*App, Job, ArchiveManifest, ArchiveAsset, string) {
	t.Helper()
	app, err := New(Config{
		DataDir: t.TempDir(), AdminUser: "admin",
		AdminPassword: "strong-password-123", SecretKey: testKey(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	timestamp := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	job := Job{
		ID: strings.Repeat("d", 24), OwnerID: "admin",
		Author: "creator", PostID: "post-a", State: "done", ArchiveUpdatedAt: timestamp,
	}
	if err := app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	dir := app.store.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(filepath.Join(dir, "media"), 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "media", "movie.mp4")
	if err := os.WriteFile(source, []byte("original source media"), 0600); err != nil {
		t.Fatal(err)
	}
	size, digest, err := fileDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	original := ArchiveAsset{
		Filename: "movie.mp4", Type: "ok_video", Title: "Movie", Size: size, SHA256: digest,
	}
	man := ArchiveManifest{
		Author: job.Author, PostID: job.PostID, CapturedAt: timestamp,
		Assets: []ArchiveAsset{original},
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), man); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "post.json"), []byte("{\"data\":[]}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("original HTML"), 0600); err != nil {
		t.Fatal(err)
	}
	return app, job, man, original, dir
}

func newVideoPublicationStage(t *testing.T, root string) (string, int64, string) {
	t.Helper()
	folder := t.TempDir()
	path := filepath.Join(folder, "rendition.mp4")
	if err := os.WriteFile(path, []byte("encoded media bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	size, digest, err := fileDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, size, digest
}

func TestVideoRenditionPublishConsistentFiles(t *testing.T) {
	app, job, man, original, dir := videoPublicationFixture(t)
	staged, size, digest := newVideoPublicationStage(t, dir)
	target := original.Filename + ".h264.mp4"
	published, err := app.publishVideoRendition(job, man.CapturedAt, original, target, staged, size, digest)
	if err != nil || !published {
		t.Fatalf("rendition not published: %v %v", published, err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged output not consumed: %v", err)
	}
	_, updated, err := readArchiveManifestNoFollow(dir)
	if err != nil || len(updated.Assets) != 2 || updated.Assets[1].Filename != target ||
		updated.Assets[1].SourceSHA256 != original.SHA256 || updated.ContentSHA256 != "" {
		t.Fatalf("rendition metadata mismatch: %+v %v", updated, err)
	}
	asset := updated.Assets[1]
	if ok, err := verifyAsset(filepath.Join(dir, "media", target), asset); err != nil || !ok {
		t.Fatalf("published video not verified: %v %v", ok, err)
	}
	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil || string(html) == "original HTML" {
		t.Fatalf("HTML was not rebuilt: %v", err)
	}
}

func TestVideoRenditionRejectsChangedArchive(t *testing.T) {
	app, job, man, original, dir := videoPublicationFixture(t)
	staged, size, digest := newVideoPublicationStage(t, dir)
	target := original.Filename + ".h264.mp4"
	updatedJob := job
	updatedJob.ArchiveUpdatedAt = updatedJob.ArchiveUpdatedAt.Add(time.Hour)
	if err := app.store.upsert(updatedJob); err != nil {
		t.Fatal(err)
	}
	published, err := app.publishVideoRendition(job, man.CapturedAt, original, target, staged, size, digest)
	if err != nil || published {
		t.Fatalf("stale encode was published: %v %v", published, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "media", target)); !os.IsNotExist(err) {
		t.Fatalf("stale file changed live media: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "index.html")); err != nil || string(raw) != "original HTML" {
		t.Fatalf("stale encode modified HTML: %v", err)
	}
}

func TestVideoRenditionHonorsArchiveWriterGate(t *testing.T) {
	app, job, man, original, dir := videoPublicationFixture(t)
	staged, size, digest := newVideoPublicationStage(t, dir)
	unlock := app.archiveGates.write(job)
	defer unlock()
	published, err := app.publishVideoRendition(job, man.CapturedAt, original, original.Filename+".h264.mp4", staged, size, digest)
	if err != nil || published {
		t.Fatalf("rendition bypassed active archive writer: %v %v", published, err)
	}
}

func TestVideoRenditionRefusesModifiedSourceBytes(t *testing.T) {
	app, job, man, original, dir := videoPublicationFixture(t)
	staged, size, digest := newVideoPublicationStage(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "media", original.Filename), []byte("tampered media content"), 0600); err != nil {
		t.Fatal(err)
	}
	published, err := app.publishVideoRendition(job, man.CapturedAt, original, original.Filename+".h264.mp4", staged, size, digest)
	if err != nil || published {
		t.Fatalf("rendition attached to changed source: %v %v", published, err)
	}
}
