package server

import (
	"encoding/json"
	"strings"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

// commentInlineRuns preserves only literal bold/italic Draft.js text.
// Paragraph boundaries remain plain newlines and are displayed with
// white-space: pre-wrap. Any unrecognized comment representation falls back
// to the existing escaped plain-text renderer, never raw HTML.
func commentInlineRuns(c boosty.Comment) []inlineRun {
	if c.IsDeleted {
		return nil
	}
	var paragraphs [][]inlineRun
	var paragraph []inlineRun
	flush := func() {
		if len(paragraph) != 0 {
			paragraphs = append(paragraphs, paragraph)
			paragraph = nil
		}
	}
	for _, raw := range c.Data {
		var head struct {
			Type        string `json:"type"`
			Modificator string `json:"modificator"`
		}
		if json.Unmarshal(raw, &head) != nil {
			flush()
			continue
		}
		if head.Type != "text" && head.Type != "link" {
			flush()
		}
		paragraph = appendInlineRun(paragraph, draftInlineRun(raw))
		if head.Modificator == "BLOCK_END" || head.Type == "header" || head.Type == "list" {
			flush()
		}
	}
	flush()
	if len(paragraphs) == 0 {
		if text := commentText(c); text != "" {
			return []inlineRun{{Text: text}}
		}
		return nil
	}
	var output []inlineRun
	for index, part := range paragraphs {
		if index > 0 {
			output = append(output, inlineRun{Text: "\n"})
		}
		output = append(output, part...)
	}
	// Keep the offline renderer and existing comment content fingerprint in
	// sync even when Boosty sends a previously unknown block shape.
	var visible strings.Builder
	for _, part := range output {
		visible.WriteString(part.Text)
	}
	if visible.String() != commentText(c) {
		return []inlineRun{{Text: commentText(c)}}
	}
	return output
}
