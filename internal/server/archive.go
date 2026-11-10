package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SawaMEN/boosty_downloader/internal/boosty"
	"html/template"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ArchiveAsset struct {
	Filename     string `json:"filename"`
	Type         string `json:"type"`
	Title        string `json:"title"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256,omitempty"`
	SourceFile   string `json:"sourceFile,omitempty"`
	SourceSHA256 string `json:"sourceSha256,omitempty"`
}
type ArchiveManifest struct {
	Title            string                   `json:"title"`
	Author           string                   `json:"author"`
	PostID           string                   `json:"postId"`
	Source           string                   `json:"source"`
	CapturedAt       time.Time                `json:"capturedAt"`
	Assets           []ArchiveAsset           `json:"assets"`
	CommentsStatus   string                   `json:"commentsStatus"`
	CommentCount     int                      `json:"commentCount"`
	CommentExpected  int                      `json:"commentExpected"`
	Failures         []string                 `json:"failures,omitempty"`
	Subscription     *CapturedSubscription    `json:"subscription,omitempty"`
	RequiredTier     *boosty.PostRequirement  `json:"requiredTier,omitempty"`
}
type renderData struct {
	ArchiveManifest
	Blocks   []archiveBlock
	Comments []commentView
}
type commentView struct {
	Author string
	Text   string
	Level  int
	Date   string
}

const postHTML = `<!doctype html>
<html lang="ru"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="referrer" content="no-referrer">
<title>{{.Title}}</title><style>:root{color-scheme:dark}body{font:16px/1.6 system-ui,sans-serif;margin:0;background:#101319;color:#e5e9f0}main{max-width:820px;margin:auto;padding:24px}h1{line-height:1.2}header,.panel,article{background:#1c222c;border:1px solid #303949;padding:20px;border-radius:14px;margin:14px 0}a{color:#92bfff;overflow-wrap:anywhere}video,audio,img{max-width:100%;border-radius:10px}img{height:auto}article{margin-left:calc(min(var(--level),6)*16px)}small,.dim{color:#aeb9ca}ul{padding-left:20px}.body-block{margin:16px 0}.unavailable{border:1px dashed #5e6570;padding:12px;border-radius:8px}</style></head>
<body><main><p><a href="/">← Медиатека</a></p>
<header><h1>{{.Title}}</h1><p class="dim">{{.Author}} · Обновление архива: {{.CapturedAt.Format "02.01.2006 15:04"}} UTC</p><p><a href="{{.Source}}" target="_blank" rel="noreferrer noopener">Оригинал Boosty</a></p></header>
<section class="panel" aria-label="Содержание публикации">{{range .Blocks}}
{{if eq .Type "header"}}<h2 class="body-block">{{.Text}}</h2>
{{else if eq .Type "list"}}<ul class="body-block"><li>{{.Text}}</li></ul>
{{else if eq .Type "text"}}<p class="body-block">{{.Text}}</p>
{{else if eq .Type "link"}}<p class="body-block">{{.Text}}</p>
{{else if .Asset}}<div class="body-block"><p>{{.Asset.Title}}</p>
{{if eq .Type "ok_video"}}<video controls playsinline preload="metadata" src="media/{{.Asset.Filename}}"></video>
{{else if eq .Type "audio_file"}}<audio controls preload="none" src="media/{{.Asset.Filename}}"></audio>
{{else if eq .Type "image"}}<img loading="lazy" src="media/{{.Asset.Filename}}" alt="{{.Asset.Title}}">
{{else if eq .Type "file"}}<a href="media/{{.Asset.Filename}}" download>Скачать: {{.Asset.Title}}</a>{{end}}
</div>{{else if .Missing}}<p class="dim unavailable body-block">Медиа не сохранено в этой копии{{if .Text}}: {{.Text}}{{end}}</p>{{end}}
{{else}}<p class="dim">В исходном посте нет распознанных блоков.</p>{{end}}</section>
<section class="panel"><h2>Комментарии</h2><p class="dim">Статус: {{.CommentsStatus}} · Получено: {{.CommentCount}}{{if .CommentExpected}} · Указано в посте: {{.CommentExpected}}{{end}}</p>{{range .Comments}}<article style="--level:{{.Level}}"><strong>{{.Author}}</strong>{{if .Date}}<small class="dim"> · {{.Date}} UTC</small>{{end}}<p>{{.Text}}</p></article>{{else}}<p class="dim">Комментарии не включены или отсутствуют.</p>{{end}}</section>
{{if .Failures}}<section class="panel"><h2>Не удалось сохранить</h2><ul>{{range .Failures}}<li>{{.}}</li>{{end}}</ul></section>{{end}}
</main></body></html>`

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
		date := ""
		if c.CreatedAt > 0 {
			date = time.Unix(c.CreatedAt, 0).UTC().Format("02.01.2006 15:04")
		}
		*out = append(*out, commentView{Author: a, Text: commentText(c), Level: depth, Date: date})
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
func (a *App) runJob(id string) {
	job, ok := a.store.claimQueued(id)
	if !ok {
		return
	}
	// A permission can be revoked while a job is waiting in the queue.
	for _, feature := range []struct {
		requested bool
		key       string
	}{
		{true, "createArchives"}, {job.IncludeComments, "saveComments"}, {job.IncludeFiles, "saveFiles"},
		{job.IncludeVideos, "saveVideos"}, {job.IncludeAudio, "saveAudio"},
	} {
		if feature.requested && !a.accounts.allowed(job.OwnerID, feature.key) {
			job.State = "failed"
			job.Error = "загрузка запрещена администратором: " + feature.key
			_ = a.store.upsert(job)
			return
		}
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
	policy := a.accounts.policy()
	ctx = withStorageBudget(ctx, a.store.root, j.OwnerID, policy.MinFreeGiB, policy.MaxUserGiB)
	if err := checkStorageBudget(ctx, 0); err != nil {
		return err
	}
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
	// Only Boosty's own subscription listing can supply the owner's tier.
	// An accessible post may also be free or purchased independently, so we
	// deliberately do not infer its required tier from this snapshot.
	if j.Subscription == nil && c.HasToken() {
		checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		listing, checkErr := a.lookupSubscriptions(checkCtx, j.OwnerID)
		cancel()
		if checkErr == nil {
			for _, sub := range listing.Authors {
				if strings.EqualFold(sub.Blog, j.Author) {
					j.Subscription = captureSubscription(sub, time.Now().UTC())
					break
				}
			}
		}
	}
	j.Title = p.Title
	j.RequiredTier = boosty.ParsePostRequirement(p.Raw)
	folder := a.store.archiveDir(j.OwnerID, j.Author, j.PostID)
	// Capture last completed copy before modifying raw JSON, comments or
	// media, so refresh failures never destroy the previous version.
	if _, e = a.store.saveArchiveRevision(*j); e != nil {
		return fmt.Errorf("не удалось сохранить прошлую версию: %w", e)
	}
	mediaFolder := filepath.Join(folder, "media")
	if e = os.MkdirAll(mediaFolder, 0700); e != nil {
		return e
	}
	compressionPolicy := a.accounts.policy()
	man := ArchiveManifest{Title: p.Title, Author: j.Author, PostID: j.PostID, Source: j.URL, CapturedAt: time.Now().UTC(), CommentsStatus: "не запрашивались", CommentExpected: p.Count.Comments, Subscription: j.Subscription, RequiredTier: j.RequiredTier}
	if e = writeArchiveJSON(filepath.Join(folder, "post.json"), json.RawMessage(p.Raw), compressionPolicy.CompressionLevel); e != nil {
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
			if e = writeArchiveJSON(filepath.Join(folder, "comments.json"), comments, compressionPolicy.CompressionLevel); e != nil {
				return e
			}
			if len(comments.Raw) > 0 {
				if e = writeArchiveJSON(filepath.Join(folder, "comments-raw.json"), json.RawMessage(comments.Raw), compressionPolicy.CompressionLevel); e != nil {
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
	// Read the previous manifest solely as a verification baseline. Existing
	// media without a digest cannot be trusted, even if it has a nonzero size.
	prior := map[string]ArchiveAsset{}
	if b, err := os.ReadFile(filepath.Join(folder, "manifest.json")); err == nil {
		var old ArchiveManifest
		if json.Unmarshal(b, &old) == nil {
			for _, asset := range old.Assets {
				prior[asset.Filename] = asset
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
		if previous, known := prior[name]; known && previous.Type == m.Type {
			valid, err := verifyAsset(dest, previous)
			if err == nil && valid && (m.Size <= 0 || m.Size == previous.Size) {
				man.Assets = append(man.Assets, previous)
				if compressionPolicy.DeduplicateMedia {
					_ = a.store.deduplicateAsset(dest, previous.SHA256, previous.Size)
				}
				j.DoneFiles++
				_ = a.store.upsert(*j)
				continue
			}
		}
		expectedSize := m.Size
		if m.Type == "ok_video" {
			expectedSize = 0 // Boosty may report a different resolution’s size.
		}
		var size int64
		var digest string
		var downloadErr error
		if m.Type == "ok_video" {
			size, digest, downloadErr = downloadVideoFile(ctx, c.MediaGet, boosty.MediaURL(p, m), dest)
		} else {
			size, digest, downloadErr = downloadFile(ctx, c.MediaGet, boosty.MediaURL(p, m), dest, expectedSize)
		}
		if downloadErr != nil {
			man.Failures = append(man.Failures, fmt.Sprintf("%s: %s", m.Type, redactNetError(downloadErr)))
			continue
		}
		title := m.Title
		if title == "" {
			title = m.Type
		}
		man.Assets = append(man.Assets, ArchiveAsset{Filename: name, Type: m.Type, Title: title, Size: size, SHA256: digest})
		if compressionPolicy.DeduplicateMedia {
			if err := a.store.deduplicateAsset(dest, digest, size); err != nil {
				// Deduplication is an optimization; a complete downloaded file remains valid.
				// Never fail an archive or silently swap damaged data because the cache failed.
				fmt.Fprintf(os.Stderr, "dedup: %v\n", err)
			}
		}
		// A replaced video can leave its previous content-addressed object
		// without any post references. Release that old object immediately.
		if previous, known := prior[name]; known && previous.SHA256 != "" {
			_ = a.store.discardBlobIfUnreferenced(previous.SHA256)
		}
		j.DoneFiles++
		_ = a.store.upsert(*j)
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
	var html bytes.Buffer
	if e = pageTemplate.Execute(&html, renderData{ArchiveManifest: man, Blocks: archiveContentBlocks(p, man.Assets), Comments: cv}); e != nil {
		return e
	}
	if e = writeAtomic(filepath.Join(folder, "index.html"), html.Bytes()); e != nil {
		return e
	}
	j.ArchiveUpdatedAt = man.CapturedAt
	if err := a.store.pruneArchiveRevisions(*j); err != nil {
		fmt.Fprintf(os.Stderr, "revisions cleanup: %v\n", err)
	}
	if len(man.Failures) > 0 {
		j.CommentsPartial = true
		return fmt.Errorf("архив частичный: %d ошибок; подробности в manifest.json", len(man.Failures))
	}
	return nil
}
