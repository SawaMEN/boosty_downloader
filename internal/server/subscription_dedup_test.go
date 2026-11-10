package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func TestOwnerScopedTiersAndSharedPhysicalMedia(t *testing.T) {
	app, err := New(Config{DataDir: t.TempDir(), AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	admin := testLogin(t, app, "admin", "strong-password-123")
	other := testMember(t, app, "tiermember")
	member, ok := app.accounts.userByName("tiermember")
	if !ok {
		t.Fatal("missing member")
	}
	now := time.Now().UTC()
	lo := captureSubscription(boosty.SubscriptionAuthor{Blog: "creator", Level: "Tier A", Paid: true}, now.Add(-7*24*time.Hour))
	hi := captureSubscription(boosty.SubscriptionAuthor{Blog: "creator", Level: "Tier B", Paid: true}, now)
	jobs := []Job{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaa", OwnerID: "admin", Author: "creator", PostID: "shared-post", Title: "one post", State: "done", Subscription: lo},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbb", OwnerID: member.ID, Author: "creator", PostID: "shared-post", Title: "one post", State: "done", Subscription: hi},
	}
	body := []byte("the same verified media bytes")
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	for _, j := range jobs {
		if err := app.store.upsert(j); err != nil {
			t.Fatal(err)
		}
		folder := app.store.archiveDir(j.OwnerID, j.Author, j.PostID)
		if err := os.MkdirAll(filepath.Join(folder, "media"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, "media", "video.mp4"), body, 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(folder, "manifest.json"), ArchiveManifest{
			Author: j.Author, PostID: j.PostID, Title: j.Title, Subscription: j.Subscription,
			Assets: []ArchiveAsset{{Filename: "video.mp4", Type: "ok_video", Size: int64(len(body)), SHA256: digest}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("public-safe page"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := app.store.deduplicateAsset(filepath.Join(folder, "media", "video.mp4"), digest, int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	a := filepath.Join(app.store.archiveDir(jobs[0].OwnerID, jobs[0].Author, jobs[0].PostID), "media", "video.mp4")
	b := filepath.Join(app.store.archiveDir(jobs[1].OwnerID, jobs[1].Author, jobs[1].PostID), "media", "video.mp4")
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(ai, bi) {
		t.Fatal("identical media occupies two physical inodes")
	}
	// Two separate owners must keep two metadata/ACL records, even if the
	// canonical post ID and the media SHA-256 are identical.
	if got := len(app.store.all()); got != 2 {
		t.Fatalf("expected two independent owner records, got %d", got)
	}
	testArchiveResponse(t, app, other, "GET", "/api/jobs/"+jobs[0].ID+"/manifest", "", 404)
	testArchiveResponse(t, app, other, "GET", "/library/"+jobs[0].ID+"/media/video.mp4", "", 404)
	mine := testArchiveResponse(t, app, other, "GET", "/api/catalog?scope=mine", "", 200)
	if strings.Contains(string(mine), "Tier A") || !strings.Contains(string(mine), "Tier B") {
		t.Fatalf("wrong subscription leaked into private catalogue: %s", mine)
	}
	if _, err := app.store.setEveryone(jobs[0].ID, "admin", true); err != nil {
		t.Fatal(err)
	}
	shared := testArchiveResponse(t, app, other, "GET", "/api/catalog?scope=shared", "", 200)
	if bytes.Contains(shared, []byte("Tier A")) {
		t.Fatal("owner tier exposed via shared catalogue")
	}
	manifest := testArchiveResponse(t, app, other, "GET", "/api/jobs/"+jobs[0].ID+"/manifest", "", 200)
	if bytes.Contains(manifest, []byte("Tier A")) {
		t.Fatal("owner subscription exposed via shared manifest")
	}
	zipped := testArchiveResponse(t, app, other, "GET", "/api/jobs/"+jobs[0].ID+"/zip", "", 200)
	reader, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range reader.File {
		if entry.Name == "manifest.json" {
			r, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 4096)
			n, _ := r.Read(buf)
			r.Close()
			if bytes.Contains(buf[:n], []byte("Tier A")) {
				t.Fatal("owner subscription exposed via shared ZIP")
			}
		}
	}
	ownManifest := testArchiveResponse(t, app, admin, "GET", "/api/jobs/"+jobs[0].ID+"/manifest", "", 200)
	var decoded ArchiveManifest
	if err := json.Unmarshal(ownManifest, &decoded); err != nil || decoded.Subscription == nil || decoded.Subscription.Level != "Tier A" {
		t.Fatalf("owner lost tier provenance %v %+v", err, decoded.Subscription)
	}
	if n, err := app.store.removeOwned("admin", jobs[0].ID, ""); err != nil || n != 1 {
		t.Fatalf("trash %v %d", err, n)
	}
	if n, err := app.store.purgeOwned("admin", jobs[0].ID, ""); err != nil || n != 1 {
		t.Fatalf("purge %v %d", err, n)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatalf("second owner's media disappeared after first owner purged: %v", err)
	}
}
