package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func TestPostBlockOrderAndOfflineMedia(t *testing.T) {
	post := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"header","content":"Первый заголовок"}`),
		json.RawMessage(`{"type":"text","content":"Вступление"}`),
		json.RawMessage(`{"type":"image","id":"cover","title":"Обложка","url":"https://cdn.example/private?token=SECRET_CDN_TOKEN"}`),
		json.RawMessage(`{"type":"text","content":"После изображения"}`),
		json.RawMessage(`{"type":"ok_video","id":"clip","title":"Видео","playerUrls":[{"type":"full_hd","url":"https://cdn.example/video?token=SECRET_CDN_TOKEN"}]}`),
		json.RawMessage(`{"type":"file","id":"doc","title":"report.pdf"}`),
		json.RawMessage(`{"type":"list","content":"Первый пункт"}`),
	}}
	blocks := postContentBlocks(post)
	want := []string{"header", "text", "image", "text", "ok_video", "file", "list"}
	if len(blocks) != len(want) {
		t.Fatalf("unexpected number of blocks: %d", len(blocks))
	}
	for i, kind := range want {
		if blocks[i].Type != kind {
			t.Fatalf("block %d got %s, want %s", i, blocks[i].Type, kind)
		}
	}
	if blocks[2].Index != 0 || blocks[4].Index != 1 || blocks[5].Index != 2 {
		t.Fatal("media indices lost original order")
	}
	assets := []ArchiveAsset{
		{Type: "image", Filename: "cover.jpg", Title: "Обложка"},
		{Type: "ok_video", Filename: "clip.mp4", Title: "Видео"},
	}
	offline := archiveContentBlocks(post, assets)
	if !offline[5].Missing || offline[5].Asset != nil {
		t.Fatal("excluded attachment must be marked missing")
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, renderData{ArchiveManifest: ArchiveManifest{Title: "Тест", CapturedAt: time.Now()}, Blocks: offline}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	previous := -1
	for _, part := range []string{"Первый заголовок", "Вступление", "cover.jpg", "После изображения", "clip.mp4", "Медиа не сохранено в этой копии", "Первый пункт"} {
		position := strings.Index(html, part)
		if position <= previous {
			t.Fatalf("out of order: %s", part)
		}
		previous = position
	}
	if strings.Contains(html, "SECRET_CDN_TOKEN") {
		t.Fatal("signed CDN URL exposed in archive")
	}
}

func TestArchivedBlocksEscapeUntrustedText(t *testing.T) {
	post := &boosty.Post{Data: []json.RawMessage{
		json.RawMessage(`{"type":"header","content":"<script>alert(1)</script>"}`),
		json.RawMessage(`{"type":"image","id":"bad","title":"<img src=x onerror=alert(1)>"}`),
	}}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, renderData{Blocks: archiveContentBlocks(post, nil)}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<script>") || strings.Contains(out.String(), "<img src=x") {
		t.Fatal("HTML injection")
	}
}
