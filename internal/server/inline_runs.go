package server

import (
	"encoding/json"
	"strings"
)

// inlineRun holds only visible text and a small allowlist of formatting.
// Never pass Boosty HTML, URLs, CSS or Draft.js metadata to html/template.
type inlineRun struct {
	Text   string `json:"text"`
	Bold   bool   `json:"bold,omitempty"`
	Italic bool   `json:"italic,omitempty"`
}

// draftInlineRun deliberately reads only the FIRST two fields of the
// Draft.js [text, style, metadata] tuple. The third field may contain signed
// URLs, private embed metadata, or arbitrary attacker-controlled markup.
func draftInlineRun(raw json.RawMessage) inlineRun {
	result := inlineRun{Text: extractText(raw)}
	if result.Text == "" {
		return result
	}
	var block struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &block) != nil {
		return result
	}
	var tuple []json.RawMessage
	if json.Unmarshal(block.Content, &tuple) != nil || len(tuple) < 2 {
		return result
	}
	var literal string
	if json.Unmarshal(tuple[0], &literal) != nil || literal != result.Text {
		return result
	}
	apply := func(style string) {
		switch strings.ToLower(strings.TrimSpace(style)) {
		case "bold":
			result.Bold = true
		case "italic":
			result.Italic = true
		}
	}
	var style string
	if json.Unmarshal(tuple[1], &style) == nil {
		apply(style)
		return result
	}
	var styles []string
	if json.Unmarshal(tuple[1], &styles) == nil {
		for _, item := range styles {
			apply(item)
		}
	}
	return result
}

func appendInlineRun(runs []inlineRun, next inlineRun) []inlineRun {
	if next.Text == "" {
		return runs
	}
	if n := len(runs); n > 0 && runs[n-1].Bold == next.Bold && runs[n-1].Italic == next.Italic {
		runs[n-1].Text += next.Text
		return runs
	}
	return append(runs, next)
}
