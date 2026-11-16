package server

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestArchiveVerificationPinsManifestAndMediaDescriptors(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "library", "admin", "creator", "verified-post")
	media := filepath.Join(base, "media")
	if err := os.MkdirAll(media, 0700); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(media, "clip.bin")
	data := []byte("saved-media-bytes")
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := os.WriteFile(clip, data, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := ArchiveManifest{
		Title: "Verified post", Author: "creator", PostID: "verified-post",
		CapturedAt: time.Now().UTC(),
		Assets:     []ArchiveAsset{{Filename: "clip.bin", Type: "file", Size: int64(len(data)), SHA256: hash}},
	}
	manifestPath := filepath.Join(base, "manifest.json")
	writeManifest := func() {
		t.Helper()
		if err := writeJSON(manifestPath, manifest); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		got, err := verifyArchive(base)
		if err != nil {
			t.Fatalf("verification failed unexpectedly: %v", err)
		}
		if len(got.Items) != 1 || got.Items[0].State != want {
			t.Fatalf("state = %+v, want %s", got, want)
		}
		if got.Valid != (want == "verified") {
			t.Fatalf("valid flag inconsistent with %s: %+v", want, got)
		}
		switch want {
		case "verified":
			if got.Verified != 1 || got.Damaged != 0 || got.Missing != 0 || got.Unchecked != 0 {
				t.Fatalf("wrong verified counters: %+v", got)
			}
		case "damaged":
			if got.Damaged != 1 {
				t.Fatalf("wrong damaged counter: %+v", got)
			}
		case "missing":
			if got.Missing != 1 {
				t.Fatalf("wrong missing counter: %+v", got)
			}
		case "unchecked":
			if got.Unchecked != 1 {
				t.Fatalf("wrong unchecked counter: %+v", got)
			}
		}
	}
	writeManifest()
	check("verified")

	// A same-size byte mutation is always detected on the pinned handle.
	if err := os.WriteFile(clip, []byte("other-media-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	check("damaged")
	if err := os.Remove(clip); err != nil {
		t.Fatal(err)
	}
	check("missing")

	// A symlink to another file must never be followed for SHA-256 hashing.
	private := filepath.Join(root, "private-token")
	if err := os.WriteFile(private, []byte("do-not-hash-this-private-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, clip); err != nil {
		t.Fatal(err)
	}
	check("damaged")
	if err := os.Remove(clip); err != nil {
		t.Fatal(err)
	}

	// An attacker-controlled FIFO must not hang the verification endpoint.
	if err := syscall.Mkfifo(clip, 0600); err != nil {
		t.Fatal(err)
	}
	check("damaged")
	if err := os.Remove(clip); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(clip, data, 0600); err != nil {
		t.Fatal(err)
	}
	manifest.Assets[0].SHA256 = ""
	writeManifest()
	check("unchecked")
	manifest.Assets[0].SHA256 = "bad-hex-digest"
	writeManifest()
	check("damaged")
	manifest.Assets[0].SHA256 = hash
	writeManifest()
	check("verified")

	// Reject symlinked parent directories as well as final media files.
	originalMediaDir := media + "-real"
	if err := os.Rename(media, originalMediaDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(originalMediaDir, media); err != nil {
		t.Fatal(err)
	}
	check("damaged")

	// A forged symlinked manifest must be rejected even when its target is
	// valid JSON that would otherwise pass the archive verifier.
	forged := filepath.Join(root, "private-manifest.json")
	if err := writeJSON(forged, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(forged, manifestPath); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyArchive(base); err == nil {
		t.Fatal("verification followed a symlinked archive manifest")
	}
}
