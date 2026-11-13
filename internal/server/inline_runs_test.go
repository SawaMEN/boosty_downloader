package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func renderStyledArchive(t *testing.T, post *boosty.Post, comments []commentView) string {
	t.Helper()
	var output bytes.Buffer
	err := pageTemplate.Execute(&output, renderData{
		ArchiveManifest: ArchiveManifest{Title: "Offline", CapturedAt: time.Now().UTC()},
		Blocks:          archiveContentBlocks(post, nil),
		Comments:        comments,
	})
	if err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestOfflineDraftFormattingPreservesVisibleOrderAndEscapesMetadata(t *testing.T) {
	post := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"header","content":"Заголовок"}`),
		json.RawMessage(`{"type":"text","content":["Start ","unstyled",[]]}`),
		json.RawMessage(`{"type":"text","content":["BOLD","bold",{"url":"https://cdn.example/?token=SECRET_STYLE_URL"}]}`),
		json.RawMessage(`{"type":"text","content":[" italic","italic",["SECRET_STYLE_METADATA"]]}`),
		json.RawMessage(`{"type":"link","content":[" & link","link",{"url":"https://boosty.to/?sig=SECRET_LINK_URL"}]}`),
		json.RawMessage(`{"type":"text","content":["!","unstyled",[]],"modificator":"BLOCK_END"}`),
		json.RawMessage(`{"type":"text","content":["both","bold",[]],"modificator":"BLOCK_END"}`),
		json.RawMessage(`{"type":"list","content":"List item"}`),
	}}
	blocks := postContentBlocks(post)
	if len(blocks) != 4 || blocks[1].Text != "Start BOLD italic & link!" || len(blocks[1].Runs) != 4 {
		t.Fatalf("styled runs changed paragraph boundaries or literal text: %+v", blocks)
	}
	html := renderStyledArchive(t, post, nil)
	if !strings.Contains(html, `Start <strong>BOLD</strong><em> italic</em> &amp; link!`) {
		t.Fatalf("offline body lost safe inline formatting: %s", html)
	}
	for _, secret := range []string{"SECRET_STYLE_URL", "SECRET_STYLE_METADATA", "SECRET_LINK_URL"} {
		if strings.Contains(html, secret) {
			t.Fatalf("signed metadata leaked into offline HTML: %s", secret)
		}
	}
	if strings.Index(html, "Заголовок") > strings.Index(html, "Start") ||
		strings.Index(html, "Start") > strings.Index(html, "List item") {
		t.Fatal("styled HTML changed source block order")
	}
}

func TestOfflineDraftSupportsCombinedStylesWithoutRawHTML(t *testing.T) {
	post := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":["<svg onload=alert(1)>","bold",{"html":"SECRET_HTML"}]}`),
		json.RawMessage(`{"type":"text","content":[" & combo",["bold","italic"],{"href":"javascript:alert(1)"}],"modificator":"BLOCK_END"}`),
		json.RawMessage(`{"type":"text","content":[" no-style","link",{"url":"https://cdn.example/?token=SECRET_CDN"}],"modificator":"BLOCK_END"}`),
	}}
	html := renderStyledArchive(t, post, nil)
	if !strings.Contains(html, `<strong>&lt;svg onload=alert(1)&gt;</strong>`) ||
		!strings.Contains(html, `<strong><em> &amp; combo</em></strong>`) ||
		!strings.Contains(html, " no-style") {
		t.Fatal("rich text was not safely formatted")
	}
	for _, bad := range []string{"<svg", "SECRET_HTML", "SECRET_CDN", "javascript:alert(1)"} {
		if strings.Contains(html, bad) {
			t.Fatalf("raw HTML, unsafe link or metadata leaked: %s", bad)
		}
	}
}

func TestOfflineCommentFormattingAndDeletedFallback(t *testing.T) {
	comment := boosty.Comment{ID: "comment", Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":["Plain ","unstyled",[]]}`),
		json.RawMessage(`{"type":"text","content":["<b>unsafe</b>","bold",{"token":"SECRET_COMMENT"}],"modificator":"BLOCK_END"}`),
		json.RawMessage(`{"type":"text","content":["end","italic",[]],"modificator":"BLOCK_END"}`),
	}}
	var views []commentView
	appendComments(&views, []boosty.Comment{comment}, 0)
	if len(views) != 1 || views[0].Text != "Plain <b>unsafe</b>\nend" || len(views[0].Runs) < 3 {
		t.Fatalf("comment body was not preserved: %+v", views)
	}
	html := renderStyledArchive(t, &boosty.Post{}, views)
	if !strings.Contains(html, `Plain <strong>&lt;b&gt;unsafe&lt;/b&gt;</strong>`) ||
		!strings.Contains(html, `<em>end</em>`) || strings.Contains(html, "SECRET_COMMENT") ||
		strings.Contains(html, "<b>unsafe</b>") {
		t.Fatal("comment style lost, metadata leaked or raw HTML executed")
	}
	deleted := boosty.Comment{ID: "deleted", IsDeleted: true, Content: json.RawMessage(`"SECRET_DELETED_CONTENT"`)}
	views = nil
	appendComments(&views, []boosty.Comment{deleted}, 0)
	if len(views) != 1 || views[0].Text != "Комментарий удалён" || len(views[0].Runs) != 0 {
		t.Fatal("deleted comments must not retain old rich-text metadata")
	}
	if strings.Contains(renderStyledArchive(t, &boosty.Post{}, views), "SECRET_DELETED_CONTENT") {
		t.Fatal("deleted comment content leaked to offline HTML")
	}
}

func TestInlineStyleChangesAffectRevisionFingerprintWithoutCDNMetadata(t *testing.T) {
	manifest := ArchiveManifest{Title: "Test", Author: "author", PostID: "id"}
	base := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":["same","unstyled",{"url":"SECRET_ONE"}],"modificator":"BLOCK_END"}`),
	}}
	changed := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":["same","bold",{"url":"SECRET_TWO"}],"modificator":"BLOCK_END"}`),
	}}
	blocksA := archiveContentBlocks(base, nil)
	blocksB := archiveContentBlocks(changed, nil)
	if blocksA[0].Text != blocksB[0].Text {
		t.Fatal("test fixture has different visible text")
	}
	fingerprintA, err := archiveContentFingerprint(manifest, blocksA, nil)
	if err != nil {
		t.Fatal(err)
	}
	fingerprintB, err := archiveContentFingerprint(manifest, blocksB, nil)
	if err != nil || fingerprintA == fingerprintB {
		t.Fatal("visible text style changes must create a distinct archive revision")
	}
	otherMetadata := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":["same","bold",{"url":"SECRET_THREE"}],"modificator":"BLOCK_END"}`),
	}}
	fingerprintC, err := archiveContentFingerprint(manifest, archiveContentBlocks(otherMetadata, nil), nil)
	if err != nil || fingerprintB != fingerprintC {
		t.Fatal("signed metadata must not create false archive revisions")
	}
}

func TestCommentUnknownStyleFallsBackToPlainText(t *testing.T) {
	comment := boosty.Comment{Content: json.RawMessage(`["fallback <tag>","bold",["SECRET_UNUSED"]]`)}
	views := []commentView{}
	appendComments(&views, []boosty.Comment{comment}, 0)
	html := renderStyledArchive(t, &boosty.Post{}, views)
	if !strings.Contains(html, "fallback &lt;tag&gt;") ||
		strings.Contains(html, "SECRET_UNUSED") || strings.Contains(html, "<tag>") {
		t.Fatal("comment fallback did not escape plain text")
	}
}
