package server

import (
	"encoding/json"
	"strings"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

// sourceBlock is the shared ordered representation for the live page and offline HTML.
// Media indices match boosty.ExtractMedia, not Post.Data (which also contains text).
type sourceBlock struct {
	Type  string      `json:"type"`
	Text  string      `json:"text,omitempty"`
	Runs  []inlineRun `json:"runs,omitempty"`
	Index int         `json:"index,omitempty"`
}

func postContentBlocks(post *boosty.Post) []sourceBlock {
	out := make([]sourceBlock, 0, len(post.Data))
	mediaIndex := 0
	var paragraph strings.Builder
	var styled []inlineRun
	flushParagraph := func() {
		if paragraph.Len() != 0 {
			out = append(out, sourceBlock{Type: "text", Text: paragraph.String(), Runs: styled})
			paragraph.Reset()
			styled = nil
		}
	}
	for _, raw := range post.Data {
		var header struct {
			Type        string `json:"type"`
			Modificator string `json:"modificator"`
		}
		if json.Unmarshal(raw, &header) != nil {
			flushParagraph()
			continue
		}
		switch header.Type {
		case "text", "link":
			// Boosty stores a visual paragraph as adjacent Draft.js runs.
			// Never add synthetic spaces: run-boundary whitespace belongs to
			// the author's text; BLOCK_END terminates the visual paragraph.
			run := draftInlineRun(raw)
			if run.Text != "" {
				paragraph.WriteString(run.Text)
				styled = appendInlineRun(styled, run)
			}
			if header.Modificator == "BLOCK_END" {
				flushParagraph()
			}
		case "header", "list":
			flushParagraph()
			if run := draftInlineRun(raw); run.Text != "" {
				out = append(out, sourceBlock{Type: header.Type, Text: run.Text, Runs: []inlineRun{run}})
			}
		case "ok_video", "audio_file", "image", "file":
			flushParagraph()
			out = append(out, sourceBlock{Type: header.Type, Index: mediaIndex})
			mediaIndex++
		default:
			flushParagraph()
		}
	}
	flushParagraph()
	return out
}

type archiveBlock struct {
	Type    string
	Text    string
	Runs    []inlineRun
	Asset   *ArchiveAsset
	Missing bool
}

// Archive pages reference only files in the snapshot manifest, never signed CDN URLs.
// Missing or excluded media are explicitly shown as unavailable.
func archiveContentBlocks(post *boosty.Post, captured []ArchiveAsset) []archiveBlock {
	available := make(map[string]ArchiveAsset, len(captured))
	derived := make(map[string]ArchiveAsset)
	for _, a := range captured {
		available[a.Filename] = a
		if a.Type == "transcoded_video" && a.SourceFile != "" && a.SourceSHA256 != "" {
			derived[a.SourceFile] = a
		}
	}
	media := boosty.ExtractMedia(post)
	out := make([]archiveBlock, 0, len(post.Data))
	for _, b := range postContentBlocks(post) {
		block := archiveBlock{Type: b.Type, Text: b.Text, Runs: b.Runs}
		switch b.Type {
		case "ok_video", "audio_file", "image", "file":
			if b.Index >= len(media) {
				continue
			}
			name := boosty.MediaName(media[b.Index], b.Index)
			if a, ok := available[name]; ok && a.Type == b.Type {
				copied := a
				if b.Type == "ok_video" {
					if alt, ok := derived[name]; ok && alt.SourceSHA256 == a.SHA256 {
						copied = alt
					}
				}
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
