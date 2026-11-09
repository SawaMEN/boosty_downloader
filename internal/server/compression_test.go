package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveJSONZstdRoundTripAndDisable(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("system zstd is not installed")
	}
	path := filepath.Join(t.TempDir(), "post.json")
	original := map[string]any{"author": "creator", "content": strings.Repeat("Большой пост про роботов пылесосов. ", 1500)}
	if err := writeArchiveJSON(path, original, 3); err != nil {
		t.Fatal(err)
	}
	compressed, err := os.ReadFile(path + ".zst")
	if err != nil {
		t.Fatalf("zstd metadata missing: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("uncompressed duplicate remained on disk")
	}
	decoded, err := exec.Command("zstd", "-q", "-dc", path+".zst").Output()
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(decoded, &result); err != nil || result["content"] != original["content"] {
		t.Fatalf("round trip differs: %v", err)
	}
	if len(compressed) >= len(decoded) {
		t.Fatal("compression did not save space")
	}
	if err := writeArchiveJSON(path, original, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".zst"); !os.IsNotExist(err) {
		t.Fatal("stale compressed file remained after disabling zstd")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveJSONWithoutCompressor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "comments.json")
	t.Setenv("PATH", t.TempDir())
	if err := writeArchiveJSON(path, strings.Repeat("content", 2000), 3); err != nil {
		t.Fatalf("zstd absence must be backward compatible: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestDeduplicationPreservesEachArchiveAndGarbageCollects(t *testing.T) {
	s := &Storage{root: t.TempDir()}
	media := bytes.Repeat([]byte("CONTENT_"), 4096)
	sum := sha256.Sum256(media)
	digest := hex.EncodeToString(sum[:])
	first := filepath.Join(s.root, "users", "one", "media", "video.mp4")
	second := filepath.Join(s.root, "users", "two", "media", "video.mp4")
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, media, 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.deduplicateAsset(path, digest, int64(len(media))); err != nil {
			t.Fatal(err)
		}
	}
	firstStat, _ := os.Stat(first)
	secondStat, _ := os.Stat(second)
	if !os.SameFile(firstStat, secondStat) {
		t.Fatal("identical media stored as separate physical objects")
	}
	object := filepath.Join(s.root, "blobs", "sha256", digest[:2], digest)
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := s.collectUnusedBlobs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(object); err != nil {
		t.Fatalf("object was removed while another archive uses it: %v", err)
	}
	if b, err := os.ReadFile(second); err != nil || !bytes.Equal(b, media) {
		t.Fatal("second owner's archive lost original bytes")
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if err := s.collectUnusedBlobs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(object); !os.IsNotExist(err) {
		t.Fatal("unreferenced shared object was not deleted")
	}
}

func TestDeduplicationRejectsInvalidCache(t *testing.T) {
	s := &Storage{root: t.TempDir()}
	data := []byte("original video content")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	first := filepath.Join(s.root, "one.mp4")
	second := filepath.Join(s.root, "two.mp4")
	if err := os.WriteFile(first, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.deduplicateAsset(first, digest, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, bytes.Repeat([]byte{'!'}, len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	// The incoming checksum must be calculated by the verified downloader.
	// A malformed digest is not allowed to enter the object index.
	if err := s.deduplicateAsset(second, "../../evil", int64(len(data))); err == nil {
		t.Fatal("unsafe hash accepted")
	}
	// Corrupting an immutable blob must not cause that data to be linked into
	// another archive merely because the byte count matches.
	if err := os.Chmod(first, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, bytes.Repeat([]byte{'?'}, len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.deduplicateAsset(second, digest, int64(len(data))); err == nil {
		t.Fatal("corrupt cached object was accepted")
	}
}
