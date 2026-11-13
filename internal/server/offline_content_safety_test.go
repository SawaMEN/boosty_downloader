package server

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func nestedCommentFixture(depth int) []boosty.Comment {
	var children []boosty.Comment
	for i := depth - 1; i >= 0; i-- {
		node := boosty.Comment{ID: strconv.Itoa(i), Content: json.RawMessage(`"Visible comment"`)}
		node.Replies.Data = children
		if len(children) != 0 {
			node.ReplyCount = 1
		}
		children = []boosty.Comment{node}
	}
	return children
}

func TestOfflineNestedCommentsMatchBoundedAPICount(t *testing.T) {
	for _, depth := range []int{20, boosty.MaxCommentTreeDepth + 8} {
		comments := nestedCommentFixture(depth)
		var views []commentView
		appendComments(&views, comments, 0)
		expected := depth
		if expected > boosty.MaxCommentTreeDepth {
			expected = boosty.MaxCommentTreeDepth
		}
		if len(views) != expected {
			t.Fatalf("depth %d emitted %d comments instead of %d", depth, len(views), expected)
		}
		for i, view := range views {
			if view.Level != i || view.Text != "Visible comment" {
				t.Fatalf("comment depth/visible content lost at %d: %+v", i, view)
			}
		}
		if depth == 20 {
			var html bytes.Buffer
			man := ArchiveManifest{Title: "Offline", CapturedAt: time.Now().UTC()}
			if err := pageTemplate.Execute(&html, renderData{ArchiveManifest: man, Comments: views}); err != nil {
				t.Fatal(err)
			}
			if strings.Count(html.String(), "<article style=") != expected {
				t.Fatalf("offline HTML lost nested comments: %d", strings.Count(html.String(), "<article style="))
			}
		}
	}
}

func TestLiveCommentPreviewPreservesDeepRepliesWithoutLeakingDeletedText(t *testing.T) {
	deep := nestedCommentFixture(20)
	deep[0].IsDeleted = true
	deep[0].Content = json.RawMessage(`"PRIVATE_DELETED_BODY"`)
	deep = append(deep, boosty.Comment{ID: "second-root", Content: json.RawMessage(`"Second root"`)})
	out := viewComments(deep)
	if len(out) != 2 || out[0].Text != "Комментарий удалён" || out[1].Text != "Second root" {
		t.Fatalf("root comment structure leaked or changed: %+v", out)
	}
	cursor := &out[0]
	for i := 1; i < 20; i++ {
		if len(cursor.Replies) != 1 || cursor.Replies[0].ID != strconv.Itoa(i) {
			t.Fatalf("reply %d silently dropped from live preview", i)
		}
		cursor = &cursor.Replies[0]
	}
	if cursor.Text != "Visible comment" || len(cursor.Replies) != 0 {
		t.Fatal("deepest comment not rendered properly")
	}
}

func TestUnknownPostBlocksHaveVisibleWarningsAndStableFingerprints(t *testing.T) {
	post := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"text","content":"Before","modificator":"BLOCK_END"}`),
		json.RawMessage(`{"type":"poll","content":{"url":"https://cdn.example/?token=PRIVATE_UNKNOWN","label":"<script>alert(1)</script>"}}`),
		json.RawMessage(`{"type":"image","id":"cover","title":"Cover"}`),
		json.RawMessage(`{"type":"text","content":"After","modificator":"BLOCK_END"}`),
	}}
	blocks := postContentBlocks(post)
	if len(blocks) != 4 || blocks[1].Type != "unsupported" || blocks[1].Digest == "" ||
		blocks[2].Index != 0 || blocks[3].Text != "After" {
		t.Fatalf("unknown block altered ordered post/media or vanished: %+v", blocks)
	}
	sourceJSON, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sourceJSON, []byte("PRIVATE_UNKNOWN")) || bytes.Contains(sourceJSON, []byte("poll")) {
		t.Fatal("live block API disclosed unsupported raw data")
	}
	archiveBlocks := archiveContentBlocks(post, nil)
	man := ArchiveManifest{Title: "Offline", CapturedAt: time.Now().UTC()}
	recordUnsupportedPostBlocks(&man, archiveBlocks)
	if len(man.Failures) != 1 || !strings.Contains(man.Failures[0], "1") {
		t.Fatalf("unsupported block didn't make archive partial: %+v", man.Failures)
	}
	var html bytes.Buffer
	if err := pageTemplate.Execute(&html, renderData{ArchiveManifest: man, Blocks: archiveBlocks}); err != nil {
		t.Fatal(err)
	}
	out := html.String()
	if !strings.Contains(out, "Неподдерживаемый блок публикации") || strings.Contains(out, "PRIVATE_UNKNOWN") ||
		strings.Contains(out, "<script>alert(1)</script>") ||
		strings.Index(out, "Before") > strings.Index(out, "Неподдерживаемый блок публикации") ||
		strings.Index(out, "Неподдерживаемый блок публикации") > strings.Index(out, "After") {
		t.Fatalf("unknown block warning missing, out of order, or leaked raw data: %s", out)
	}
	first, err := archiveContentFingerprint(man, archiveBlocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	post.Data[1] = json.RawMessage(`{"type":"poll","content":{"url":"https://cdn.example/?token=CHANGED_PRIVATE_UNKNOWN"}}`)
	changed, err := archiveContentFingerprint(man, archiveContentBlocks(post, nil), nil)
	if err != nil || first == changed {
		t.Fatal("change of unsupported raw content must preserve old archive revision")
	}
}

func TestMalformedUnknownPostDataIsNotSilentlyDiscarded(t *testing.T) {
	post := &boosty.Post{Data: []json.RawMessage{json.RawMessage(`{"type":`)}}
	blocks := postContentBlocks(post)
	if len(blocks) != 1 || blocks[0].Type != "unsupported" {
		t.Fatal("invalid Boosty block silently treated as an empty complete post")
	}
	man := ArchiveManifest{}
	recordUnsupportedPostBlocks(&man, archiveContentBlocks(post, nil))
	if len(man.Failures) != 1 {
		t.Fatalf("malformed block did not flag partial archive: %v", man.Failures)
	}
}
