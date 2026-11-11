package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveContentFingerprintIgnoresCaptureTimeButTracksChanges(t *testing.T) {
	man := ArchiveManifest{
		Title: "Post", Author: "creator", PostID: "post-1",
		Source:         "https://boosty.to/creator/posts/post-1",
		CapturedAt:     time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		CommentsStatus: "сохранены полностью (по данным API)",
		CommentCount:   1, CommentExpected: 1,
		Assets: []ArchiveAsset{{Filename: "video.mp4", Type: "ok_video", SHA256: strings.Repeat("a", 64), Size: 200}},
		Subscription: &CapturedSubscription{
			Level: "Tier A", LevelID: "123", Paid: true,
			ObservedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			Source:     "boosty_subscriptions",
		},
	}
	blocks := []archiveBlock{{Type: "text", Text: "Post body"}, {Type: "ok_video", Asset: &man.Assets[0]}}
	comments := []commentView{{Author: "Subscriber", Text: "Thanks", Date: "01.11.2026 10:00"}}
	first, err := archiveContentFingerprint(man, blocks, comments)
	if err != nil || !validContentFingerprint(first) {
		t.Fatalf("bad fingerprint: %s %v", first, err)
	}
	man.CapturedAt = man.CapturedAt.Add(7 * 24 * time.Hour)
	man.Subscription.ObservedAt = man.CapturedAt
	second, err := archiveContentFingerprint(man, blocks, comments)
	if err != nil || first != second {
		t.Fatalf("capture timestamp created a false new version: %s %s %v", first, second, err)
	}
	man.Subscription.Level = "Tier B"
	changedTier, err := archiveContentFingerprint(man, blocks, comments)
	if err != nil || changedTier == first {
		t.Fatal("subscription level changes must preserve the prior attribution")
	}
	man.Subscription.Level = "Tier A"
	comments[0].Text = "Edited comment"
	changedComments, err := archiveContentFingerprint(man, blocks, comments)
	if err != nil || changedComments == first {
		t.Fatal("comment edit omitted from the fingerprint")
	}
	comments[0].Text = "Thanks"
	man.Assets[0].SHA256 = strings.Repeat("b", 64)
	changedMedia, err := archiveContentFingerprint(man, blocks, comments)
	if err != nil || changedMedia == first {
		t.Fatal("changed video bytes omitted from the fingerprint")
	}
	man.Assets[0].SHA256 = strings.Repeat("a", 64)
	blocks[0].Text = "Edited post"
	changedPost, err := archiveContentFingerprint(man, blocks, comments)
	if err != nil || changedPost == first {
		t.Fatal("edited post text omitted from the fingerprint")
	}
}

func TestDiscardIdenticalArchiveRevisionsSafely(t *testing.T) {
	cases := []struct {
		name       string
		oldHash    string
		newHash    string
		newFailure bool
		wantRemove bool
	}{
		{"identical complete content", strings.Repeat("a", 64), strings.Repeat("a", 64), false, true},
		{"different content", strings.Repeat("a", 64), strings.Repeat("b", 64), false, false},
		{"legacy version without hash", "", strings.Repeat("a", 64), false, false},
		{"failed refresh", strings.Repeat("a", 64), strings.Repeat("a", 64), true, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store, err := newStorage(t.TempDir(), testKey(t))
			if err != nil {
				t.Fatal(err)
			}
			job := Job{ID: "111111111111111111111111", OwnerID: "admin", Author: "creator", PostID: "post1", State: "done"}
			live := store.archiveDir(job.OwnerID, job.Author, job.PostID)
			if err := os.MkdirAll(filepath.Join(live, "media"), 0700); err != nil {
				t.Fatal(err)
			}
			mediaPath := filepath.Join(live, "media", "clip.mp4")
			if err := os.WriteFile(mediaPath, []byte("precious media"), 0600); err != nil {
				t.Fatal(err)
			}
			old := ArchiveManifest{
				Author: job.Author, PostID: job.PostID, Title: "Post",
				ContentSHA256: tt.oldHash,
				Assets:        []ArchiveAsset{{Filename: "clip.mp4", Type: "ok_video", Size: 14}},
			}
			if err := writeJSON(filepath.Join(live, "manifest.json"), old); err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(filepath.Join(live, "index.html"), []byte("old HTML")); err != nil {
				t.Fatal(err)
			}
			revisionID, err := store.saveArchiveRevision(job)
			if err != nil || !validRevisionID(revisionID) {
				t.Fatalf("snapshot: %q %v", revisionID, err)
			}
			current := old
			current.ContentSHA256 = tt.newHash
			current.CapturedAt = time.Now().UTC()
			if tt.newFailure {
				current.Failures = []string{"temporarily incomplete"}
			}
			if err := writeJSON(filepath.Join(live, "manifest.json"), current); err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(filepath.Join(live, "index.html"), []byte("new HTML")); err != nil {
				t.Fatal(err)
			}
			removed, err := store.discardIdenticalRevision(job, revisionID)
			if err != nil || removed != tt.wantRemove {
				t.Fatalf("removed=%v want=%v, error=%v", removed, tt.wantRemove, err)
			}
			history, err := store.listArchiveRevisions(job)
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 1
			if tt.wantRemove {
				wantCount = 0
			}
			if len(history) != wantCount {
				t.Fatalf("history entries: %d want %d", len(history), wantCount)
			}
			if data, err := os.ReadFile(mediaPath); err != nil || string(data) != "precious media" {
				t.Fatalf("media damaged by revision cleanup: %s %v", data, err)
			}
		})
	}
}
