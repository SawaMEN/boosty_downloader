package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SawaMEN/boosty_downloader/internal/boosty"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ArchiveAsset struct {
	Filename string `json:"filename"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Size     int64  `json:"size"`
}
type ArchiveManifest struct {
	Title           string         `json:"title"`
	Author          string         `json:"author"`
	PostID          string         `json:"postId"`
	Source          string         `json:"source"`
	CapturedAt      time.Time      `json:"capturedAt"`
	Assets          []ArchiveAsset `json:"assets"`
	CommentsStatus  string         `json:"commentsStatus"`
	CommentCount    int            `json:"commentCount"`
	CommentExpected int            `json:"commentExpected"`
	Failures        []string       `json:"failures,omitempty"`
}
type renderData struct {
	ArchiveManifest
	Body     []string
	Comments []commentView
}
type commentView struct {
	Author string
	Text   string
	Level  int
}

const postHTML = `<!doctype html><html lang="ru"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="referrer" content="no-referrer"><title>{{.Title}}</title><style>:root{color-scheme:dark}body{font:16px/1.6 system-ui,sans-serif;margin:0;background:#101319;color:#e5e9f0}main{max-width:820px;margin:auto;padding:24px}h1{line-height:1.2}header,.panel,article{background:#1c222c;border:1px solid #303949;padding:20px;border-radius:14px;margin:14px 0}a{color:#92bfff;overflow-wrap:anywhere}video,audio,img{max-width:100%;border-radius:10px}img{height:auto}article{margin-left:calc(min(var(--level),6)*16px)}small,.dim{color:#aeb9ca}pre{white-space:pre-wrap;overflow-wrap:anywhere}ul{padding-left:20px}</style></head><body><main><p><a href="/">← Медиатека</a></p><header><h1>{{.Title}}</h1><p class="dim">{{.Author}} · Обновление архива: {{.CapturedAt.Format "02.01.2006 15:04"}} UTC</p><p><a href="{{.Source}}" target="_blank" rel="noreferrer noopener">Оригинал Boosty</a></p></header><section class="panel">{{range .Body}}<p>{{.}}</p>{{end}}{{if not .Body}}<p class="dim">В исходном посте не найден текст.</p>{{end}}</section>{{range .Assets}}<section class="panel"><p>{{.Title}}</p>{{if eq .Type "ok_video"}}<video controls preload="metadata" src="media/{{.Filename}}"></video>{{else if eq .Type "audio_file"}}<audio controls src="media/{{.Filename}}"></audio>{{else if eq .Type "image"}}<img loading="lazy" src="media/{{.Filename}}" alt="{{.Title}}">{{else}}<a href="media/{{.Filename}}" download>Скачать вложение</a>{{end}}</section>{{end}}<section class="panel"><h2>Комментарии</h2><p class="dim">Статус: {{.CommentsStatus}} · Получено: {{.CommentCount}}{{if .CommentExpected}} · Указано в посте: {{.CommentExpected}}{{end}}</p>{{range .Comments}}<article style="--level:{{.Level}}"><strong>{{.Author}}</strong><p>{{.Text}}</p></article>{{else}}<p class="dim">Комментарии не включены или отсутствуют.</p>{{end}}</section>{{if .Failures}}<section class="panel"><h2>Не удалось сохранить</h2><ul>{{range .Failures}}<li>{{.}}</li>{{end}}</ul></section>{{end}}</main></body></html>`

var pageTemplate = template.Must(template.New("post").Parse(postHTML))

func extractText(raw json.RawMessage) string {
	var m struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if m.Type != "text" && m.Type != "header" && m.Type != "link" && m.Type != "list" {
		return ""
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		return text
	}
	var val any
	if json.Unmarshal(m.Content, &val) != nil {
		return ""
	}
	return flattenText(val)
}
func flattenText(x any) string {
	switch v := x.(type) {
	case string:
		return v
	case []any:
		var items []string
		for i, x := range v {
			if i == 1 {
				if name, ok := x.(string); ok && (name == "bold" || name == "italic" || name == "unstyled" || name == "link") {
					continue
				}
			}
			if text := flattenText(x); text != "" {
				items = append(items, text)
			}
		}
		return strings.Join(items, " ")
	case map[string]any:
		if t, ok := v["text"].(string); ok {
			return t
		}
		if v, ok := v["blocks"]; ok {
			return flattenText(v)
		}
	}
	return ""
}
func commentText(c boosty.Comment) string {
	var texts []string
	for _, raw := range c.Data {
		if t := extractText(raw); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}
func appendComments(out *[]commentView, comments []boosty.Comment, depth int) {
	for _, c := range comments {
		a := c.Author.Name
		if a == "" {
			a = "Пользователь"
		}
		*out = append(*out, commentView{Author: a, Text: commentText(c), Level: depth})
		appendComments(out, c.Replies.Data, depth+1)
	}
}
func redactNetError(err error) string {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err.Error()
	}
	return err.Error()
}
func writeJSON(p string, val any) error {
	b, e := json.MarshalIndent(val, "", "  ")
	if e != nil {
		return e
	}
	return writeAtomic(p, b)
}
func downloadFile(ctx context.Context, client *boosty.Client, raw, dest string) (int64, error) {
	if raw == "" {
		return 0, errors.New("медиа без URL")
	}
	tmp := dest + ".part"
	var existing int64
	if info, e := os.Stat(tmp); e == nil {
		existing = info.Size()
	}
	rangeHeader := ""
	if existing > 0 {
		rangeHeader = fmt.Sprintf("bytes=%d-", existing)
	}
	res, e := client.MediaGet(ctx, raw, rangeHeader)
	if e != nil {
		return 0, e
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("CDN HTTP %d", res.StatusCode)
	}
	if existing > 0 && res.StatusCode == http.StatusOK {
		existing = 0
	}
	if existing > 0 && res.StatusCode == http.StatusPartialContent {
		if !strings.HasPrefix(res.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", existing)) {
			return 0, errors.New("неверный Content-Range")
		}
	}
	mode := os.O_CREATE | os.O_WRONLY
	if existing > 0 {
		mode |= os.O_APPEND
	} else {
		mode |= os.O_TRUNC
	}
	f, e := os.OpenFile(tmp, mode, 0600)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	n, e := io.Copy(f, &io.LimitedReader{R: res.Body, N: 10 << 30})
	if e != nil {
		return n, e
	}
	if e = f.Sync(); e != nil {
		return n, e
	}
	if res.ContentLength >= 0 && n != res.ContentLength {
		return n, errors.New("неполный ответ CDN")
	}
	if n >= 10<<30 {
		return n, errors.New("превышен максимальный размер файла 10 GiB")
	}
	if e = os.Rename(tmp, dest); e != nil {
		return n, e
	}
	return n + existing, nil
}
func (a *App) runJob(id string) {
	job, ok := a.store.claimQueued(id)
	if !ok {
		return
	}
	job.Error = ""
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Hour)
	defer cancel()
	err := a.archive(ctx, &job)
	if a.ctx.Err() != nil {
		job.State = "queued"
		job.Error = ""
	} else if err != nil && job.CommentsPartial {
		job.State = "partial"
		job.Error = err.Error()
	} else if err != nil {
		job.State = "failed"
		job.Error = err.Error()
	} else if job.CommentsPartial {
		job.State = "partial"
	} else {
		job.State = "done"
	}
	_ = a.store.upsert(job)
}
func (a *App) archive(ctx context.Context, j *Job) error {
	c, e := a.client(j.OwnerID)
	if e != nil {
		return e
	}
	p, e := c.Post(ctx, j.Author, j.PostID)
	if e != nil {
		return e
	}
	if !p.HasAccess {
		return errors.New("нет доступа к этому посту: необходима действующая подписка")
	}
	j.Title = p.Title
	folder := a.store.archiveDir(j.OwnerID, j.Author, j.PostID)
	mediaFolder := filepath.Join(folder, "media")
	if e = os.MkdirAll(mediaFolder, 0700); e != nil {
		return e
	}
	man := ArchiveManifest{Title: p.Title, Author: j.Author, PostID: j.PostID, Source: j.URL, CapturedAt: time.Now().UTC(), CommentsStatus: "не запрашивались", CommentExpected: p.Count.Comments}
	if e = writeJSON(filepath.Join(folder, "post.json"), json.RawMessage(p.Raw)); e != nil {
		return e
	}
	var comments boosty.CommentResult
	if j.IncludeComments {
		comments, e = c.Comments(ctx, j.Author, j.PostID, p.Count.Comments)
		if e != nil {
			man.CommentsStatus = "ошибка получения"
			man.Failures = append(man.Failures, "Комментарии: "+e.Error())
			j.CommentsPartial = true
		} else {
			if e = writeJSON(filepath.Join(folder, "comments.json"), comments); e != nil {
				return e
			}
			if len(comments.Raw) > 0 {
				if e = writeJSON(filepath.Join(folder, "comments-raw.json"), json.RawMessage(comments.Raw)); e != nil {
					return e
				}
			}
			man.CommentCount = comments.Received
			j.CommentsPartial = comments.Partial
			if comments.Partial {
				man.CommentsStatus = "частично"
			} else {
				man.CommentsStatus = "сохранены полностью (по данным API)"
			}
		}
	}
	rawMedia := boosty.ExtractMedia(p)
	for _, m := range rawMedia {
		if m.Type == "file" && !j.IncludeFiles || m.Type == "ok_video" && !j.IncludeVideos || m.Type == "audio_file" && !j.IncludeAudio {
			continue
		}
		j.TotalFiles++
	}
	_ = a.store.upsert(*j)
	for i, m := range rawMedia {
		if m.Type == "file" && !j.IncludeFiles || m.Type == "ok_video" && !j.IncludeVideos || m.Type == "audio_file" && !j.IncludeAudio {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		name := boosty.MediaName(m, i)
		dest := filepath.Join(mediaFolder, name)
		if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
			man.Assets = append(man.Assets, ArchiveAsset{Filename: name, Type: m.Type, Title: m.Title, Size: info.Size()})
			j.DoneFiles++
			_ = a.store.upsert(*j)
			continue
		}
		size, err := downloadFile(ctx, c, boosty.MediaURL(p, m), dest)
		if err != nil {
			man.Failures = append(man.Failures, fmt.Sprintf("%s: %s", m.Type, redactNetError(err)))
			continue
		}
		title := m.Title
		if title == "" {
			title = m.Type
		}
		man.Assets = append(man.Assets, ArchiveAsset{Filename: name, Type: m.Type, Title: title, Size: size})
		j.DoneFiles++
		_ = a.store.upsert(*j)
	}
	var body []string
	for _, raw := range p.Data {
		if t := extractText(raw); t != "" {
			body = append(body, t)
		}
	}
	var cv []commentView
	if j.IncludeComments {
		appendComments(&cv, comments.Comments, 0)
	}
	// A completed archive has its own immutable capture date, independent of
	// the task's UpdatedAt (which changes on shares, retries and progress).
	man.CapturedAt = time.Now().UTC()
	if e = writeJSON(filepath.Join(folder, "manifest.json"), man); e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(folder, "index.html"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if e = pageTemplate.Execute(f, renderData{ArchiveManifest: man, Body: body, Comments: cv}); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	j.ArchiveUpdatedAt = man.CapturedAt
	if len(man.Failures) > 0 {
		j.CommentsPartial = true
		return fmt.Errorf("архив частичный: %d ошибок; подробности в manifest.json", len(man.Failures))
	}
	return nil
}
