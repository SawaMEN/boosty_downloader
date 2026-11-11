package server

import (
	"path/filepath"
	"sort"
)

// retainVerifiedRenditions keeps extra video copies across an unchanged
// source-media refresh. A rendition is not valid for a new source SHA-256.
// Never trust a previous manifest without validating the file bytes.
func retainVerifiedRenditions(man *ArchiveManifest, prior map[string]ArchiveAsset, mediaDir string) {
	current := make(map[string]ArchiveAsset)
	exists := make(map[string]bool)
	for _, asset := range man.Assets {
		exists[asset.Filename] = true
		if asset.Type == "ok_video" && validContentFingerprint(asset.SHA256) {
			current[asset.Filename] = asset
		}
	}
	names := make([]string, 0, len(prior))
	for name := range prior {
		names = append(names, name)
	}
	sort.Strings(names) // Stable manifest order makes normalized hashes reproducible.
	for _, name := range names {
		rendition := prior[name]
		if rendition.Type != "transcoded_video" || !safeArchivePart(rendition.Filename) ||
			!safeArchivePart(rendition.SourceFile) || !validContentFingerprint(rendition.SourceSHA256) ||
			!validContentFingerprint(rendition.SHA256) || exists[rendition.Filename] {
			continue
		}
		source, ok := current[rendition.SourceFile]
		if !ok || source.SHA256 != rendition.SourceSHA256 {
			continue
		}
		valid, err := verifyAsset(filepath.Join(mediaDir, rendition.Filename), rendition)
		if err != nil || !valid {
			continue
		}
		rendition.Title = source.Title
		man.Assets = append(man.Assets, rendition)
		exists[rendition.Filename] = true
	}
}
