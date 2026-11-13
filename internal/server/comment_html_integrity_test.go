package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func TestBoostyDraftRunsPreserveParagraphWhitespaceAndMediaOrder(t *testing.T) {
	post := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":["Hello ","unstyled",[]]}`),
		json.RawMessage(`{"type":"link","content":["world","link",{"url":"https://cdn.example/?signed=PRIVATE"}]}`),
		json.RawMessage(`{"type":"text","content":["!\nNext line","unstyled",[]],"modificator":"BLOCK_END"}`),
		json.RawMessage(`{"type":"image","id":"image1","title":"Picture"}`),
		json.RawMessage(`{"type":"text","content":["Tail","unstyled",[]],"modificator":"BLOCK_END"}`),
	}}
	blocks := postContentBlocks(post)
	if len(blocks) != 3 || blocks[0].Type != "text" || blocks[0].Text != "Hello world!\nNext line" ||
		blocks[1].Type != "image" || blocks[1].Index != 0 || blocks[2].Type != "text" || blocks[2].Text != "Tail" {
		t.Fatalf("lost Draft.js runs, paragraph boundaries or media order: %+v", blocks)
	}
	assets := archiveContentBlocks(post, []ArchiveAsset{{Type: "image", Filename: "image1.jpg", Title: "Picture"}})
	var rendered bytes.Buffer
	if err := pageTemplate.Execute(&rendered, renderData{ArchiveManifest: ArchiveManifest{Title: "Draft", CapturedAt: time.Now().UTC()}, Blocks: assets}); err != nil {
		t.Fatal(err)
	}
	html := rendered.String()
	if strings.Contains(html, "PRIVATE") || !strings.Contains(html, "Hello world!") ||
		!strings.Contains(html, "Next line") || !strings.Contains(html, "white-space:pre-wrap") {
		t.Fatalf("HTML lost paragraph/newline or exposed signed metadata: %s", html)
	}
	if strings.Index(html, "Hello world!") > strings.Index(html, "image1.jpg") ||
		strings.Index(html, "image1.jpg") > strings.Index(html, "Tail") {
		t.Fatal("archive text/media order changed")
	}
}

func TestBoostyDraftRunsDoNotInsertSpacesOrExposeStyles(t *testing.T) {
	post := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":["Hello","unstyled",[]]}`),
		json.RawMessage(`{"type":"text","content":[",","bold",["style-only-SECRET"]]}`),
		json.RawMessage(`{"type":"text","content":[" world","italic",[]],"modificator":"BLOCK_END"}`),
		json.RawMessage(`{"type":"text","content":["<script>alert(1)</script>","unstyled",[]],"modificator":"BLOCK_END"}`),
	}}
	blocks := postContentBlocks(post)
	if len(blocks) != 2 || blocks[0].Text != "Hello, world" {
		t.Fatalf("Draft.js run punctuation changed: %+v", blocks)
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, renderData{Blocks: archiveContentBlocks(post, nil)}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "style-only-SECRET") || strings.Contains(out.String(), "<script>") ||
		!strings.Contains(out.String(), "&lt;script&gt;") {
		t.Fatal("HTML exposed metadata or injected active script")
	}
}

func TestCommentTextKeepsRunsBreaksAndFallback(t *testing.T) {
	comment := boosty.Comment{Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":["First ","unstyled",[]]}`),
		json.RawMessage(`{"type":"link","content":["word","link",{"url":"https://cdn.example/?secret=PRIVATE"}]}`),
		json.RawMessage(`{"type":"text","content":["!","unstyled",[]],"modificator":"BLOCK_END"}`),
		json.RawMessage(`{"type":"text","content":["Second line","unstyled",[]],"modificator":"BLOCK_END"}`),
	}}
	if got := commentText(comment); got != "First word!\nSecond line" {
		t.Fatalf("comment lost original paragraphs: %q", got)
	}
	fallback := boosty.Comment{Content: json.RawMessage(`["Fallback body","unstyled",[]]`)}
	if got := commentText(fallback); got != "Fallback body" {
		t.Fatalf("missing comment fallback not restored: %q", got)
	}
}

func TestDeletedCommentsPreserveRepliesAndPartialExplanation(t *testing.T) {
	parent := boosty.Comment{ID: "parent", IsDeleted: true}
	parent.Replies.Data = []boosty.Comment{{ID: "reply", Content: json.RawMessage(`"Live reply"`)}}
	views := []commentView{}
	appendComments(&views, []boosty.Comment{parent}, 0)
	if len(views) != 2 || views[0].Text != "Комментарий удалён" || views[1].Text != "Live reply" || views[1].Level != 1 {
		t.Fatalf("deleted comment discarded surviving reply thread: %+v", views)
	}

	manifest := ArchiveManifest{
		Title: "Example", CommentsStatus: "частично",
		CommentsNote: "Boosty вернул не все ответы на комментарии",
		CommentCount: 2, CommentExpected: 5, CapturedAt: time.Now().UTC(),
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, renderData{ArchiveManifest: manifest, Comments: views}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), manifest.CommentsNote) || !strings.Contains(out.String(), "comment-body") {
		t.Fatal("offline HTML hid reason for partial comments or lost multiline formatting")
	}
	fingerprint, err := archiveContentFingerprint(manifest, nil, views)
	if err != nil {
		t.Fatal(err)
	}
	manifest.CommentsNote = "Новая причина неполного архива"
	updated, err := archiveContentFingerprint(manifest, nil, views)
	if err != nil || fingerprint == updated {
		t.Fatal("comment partial-status reason not included in revision fingerprint")
	}
}
