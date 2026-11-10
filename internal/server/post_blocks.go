package server

import (
	"encoding/json"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

// sourceBlock is the shared ordered representation for the live page and offline HTML.
// Media indices match boosty.ExtractMedia, not Post.Data (which also contains text).
type sourceBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Index int    `json:"index,omitempty"`
}

func postContentBlocks(post *boosty.Post) []sourceBlock {
	out := make([]sourceBlock, 0, len(post.Data))
	mediaIndex := 0
	for _, raw := range post.Data {
		var header struct { Type string `json:"type"` }
		if json.Unmarshal(raw, &header) != nil { continue }
		switch header.Type {
		case "text", "header", "list", "link":
			if txt := extractText(raw); txt != "" { out = append(out, sourceBlock{Type: header.Type, Text: txt}) }
		case "ok_video", "audio_file", "image", "file":
			out = append(out, sourceBlock{Type: header.Type, Index: mediaIndex})
			mediaIndex++
		}
	}
	return out
}

type archiveBlock struct {
	Type    string
	Text    string
	Asset   *ArchiveAsset
	Missing bool
}

// Archive pages reference only files in the snapshot manifest, never signed CDN URLs.
// Missing or excluded media are explicitly shown as unavailable.
func archiveContentBlocks(post *boosty.Post, captured []ArchiveAsset) []archiveBlock {
	available := make(map[string]ArchiveAsset, len(captured))
	for _, a := range captured { available[a.Filename] = a }
	media := boosty.ExtractMedia(post)
	out := make([]archiveBlock, 0, len(post.Data))
	for _, b := range postContentBlocks(post) {
		block := archiveBlock{Type: b.Type, Text: b.Text}
		switch b.Type {
		case "ok_video", "audio_file", "image", "file":
			if b.Index >= len(media) { continue }
			name := boosty.MediaName(media[b.Index], b.Index)
			if a, ok := available[name]; ok && a.Type == b.Type {
				copied := a
				block.Asset = &copied
			} else {
				block.Missing = true
				block.Text = media[b.Index].Title
			}
		}
		out = append(out, block)
	}
	return out
}
