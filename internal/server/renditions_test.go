package server

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetainVerifiedRenditionsOnlyForMatchingSource(t *testing.T) {
	mediaDir := t.TempDir()
	encode := []byte("reduced video bytes")
	sum := sha256.Sum256(encode)
	digest := hex.EncodeToString(sum[:])
	name := "clip.mp4.h264.mp4"
	if err := os.WriteFile(filepath.Join(mediaDir, name), encode, 0600); err != nil {
		t.Fatal(err)
	}
	sourceSHA := strings.Repeat("a", 64)
	previous := ArchiveAsset{
		Filename: name, Type: "transcoded_video", Size: int64(len(encode)),
		SHA256: digest, SourceFile: "clip.mp4", SourceSHA256: sourceSHA,
		Title: "Old caption",
	}
	prior := map[string]ArchiveAsset{name: previous}
	man := ArchiveManifest{Assets: []ArchiveAsset{{
		Filename: "clip.mp4", Type: "ok_video", Size: 100,
		SHA256: sourceSHA, Title: "New caption",
	}}}
	retainVerifiedRenditions(&man, prior, mediaDir)
	if len(man.Assets) != 2 || man.Assets[1].Filename != name || man.Assets[1].Title != "New caption" {
		t.Fatalf("verified rendition was not retained: %+v", man.Assets)
	}
	retainVerifiedRenditions(&man, prior, mediaDir)
	if len(man.Assets) != 2 {
		t.Fatal("rendition appended twice")
	}
	changed := ArchiveManifest{Assets: []ArchiveAsset{{
		Filename: "clip.mp4", Type: "ok_video", SHA256: strings.Repeat("b", 64),
	}}}
	retainVerifiedRenditions(&changed, prior, mediaDir)
	if len(changed.Assets) != 1 {
		t.Fatal("rendition from previous source was reused")
	}
	if err := os.WriteFile(filepath.Join(mediaDir, name), []byte("corrupted file bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupted := ArchiveManifest{Assets: []ArchiveAsset{{
		Filename: "clip.mp4", Type: "ok_video", SHA256: sourceSHA,
	}}}
	retainVerifiedRenditions(&corrupted, prior, mediaDir)
	if len(corrupted.Assets) != 1 {
		t.Fatal("corrupted derived video was retained")
	}
	malicious := previous
	malicious.Filename = "../other.mp4"
	retainVerifiedRenditions(&corrupted, map[string]ArchiveAsset{malicious.Filename: malicious}, mediaDir)
	if len(corrupted.Assets) != 1 {
		t.Fatal("unsafe rendition path was retained")
	}
}
