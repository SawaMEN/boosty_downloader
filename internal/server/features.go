package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SawaMEN/boosty_downloader/internal/boosty"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PostDetails provides the live post without exposing signed CDN URLs or credentials.
func (a *App) postDetails(w http.ResponseWriter, r *http.Request) {
	author, id, err := boosty.PostURL(r.URL.Query().Get("url"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := a.client(principal(r).ID)
	if err != nil {
		fail(w, 500, "ошибка хранилища Boosty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	post, err := c.Post(ctx, author, id)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	type mediaItem struct {
		Index    int    `json:"index"`
		Type     string `json:"type"`
		Title    string `json:"title"`
		Size     int64  `json:"size"`
		Playable bool   `json:"playable"`
	}
	media := boosty.ExtractMedia(post)
	items := make([]mediaItem, 0, len(media))
	for i, m := range media {
		mime := m.Type == "ok_video" || m.Type == "audio_file"
		items = append(items, mediaItem{Index: i, Type: m.Type, Title: m.Title, Size: m.Size, Playable: mime})
	}
	blocks := postContentBlocks(post)
	reply(w, 200, map[string]any{
		"title": post.Title, "author": author, "postId": id, "source": r.URL.Query().Get("url"),
		"hasAccess": post.HasAccess, "publishTime": post.PublishTime,
		"media": items, "blocks": blocks, "comments": post.Count.Comments,
	})
}

// Comments are intentionally loaded separately; a post may contain a large discussion.
func (a *App) postComments(w http.ResponseWriter, r *http.Request) {
	author, id, err := boosty.PostURL(r.URL.Query().Get("url"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	c, err := a.client(principal(r).ID)
	if err != nil {
		fail(w, 500, "ошибка хранилища Boosty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	post, err := c.Post(ctx, author, id)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	if !post.HasAccess {
		fail(w, 403, "недостаточно прав для просмотра комментариев")
		return
	}
	result, err := c.Comments(ctx, author, id, post.Count.Comments)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	comments := viewComments(result.Comments)
	reply(w, 200, map[string]any{"comments": comments, "received": result.Received, "expected": result.Expected, "partial": result.Partial, "reason": result.Reason})
}

type commentDTO struct {
	ID        string       `json:"id"`
	Author    string       `json:"author"`
	Text      string       `json:"text"`
	CreatedAt int64        `json:"createdAt"`
	Replies   []commentDTO `json:"replies"`
}

func viewComments(comments []boosty.Comment) []commentDTO {
	out := make([]commentDTO, 0, len(comments))
	parents := make([]*commentDTO, boosty.MaxCommentTreeDepth)
	boosty.WalkCommentTree(comments, func(c *boosty.Comment, depth int) {
		name := c.Author.Name
		if name == "" {
			name = "Пользователь"
		}
		body := commentText(*c)
		if c.IsDeleted {
			body = "Комментарий удалён"
		}
		dto := commentDTO{ID: c.ID, Author: name, Text: body, CreatedAt: c.CreatedAt, Replies: []commentDTO{}}
		dest := &out
		if depth > 0 {
			dest = &parents[depth-1].Replies
		}
		*dest = append(*dest, dto)
		parents[depth] = &(*dest)[len(*dest)-1]
	})
	return out
}

func (a *App) retryJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 24 {
		fail(w, 400, "неверный идентификатор")
		return
	}
	if len(a.queue) >= cap(a.queue) {
		fail(w, 429, "очередь переполнена")
		return
	}
	job, err := a.store.transitionToQueued(id, principal(r).ID)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	a.queueJob(id)
	reply(w, 202, job)
}

// portableZipFiles builds a strict, bounded allowlist from the manifest rather
// than walking the entire archive directory. Raw Boosty API payloads, temporary
// downloads, and unrelated files must never affect the portable archive.
func portableZipFiles(manifest ArchiveManifest) ([]string, error) {
	const maxZIPFiles = 5000
	names := map[string]struct{}{"index.html": {}, "manifest.json": {}}
	if len(manifest.Assets) > maxZIPFiles {
		return nil, errors.New("слишком много файлов в манифесте")
	}
	for _, asset := range manifest.Assets {
		if !safeArchivePart(asset.Filename) {
			return nil, errors.New("небезопасное имя файла в манифесте")
		}
		names["media/"+asset.Filename] = struct{}{}
		if len(names) > maxZIPFiles {
			return nil, errors.New("слишком много файлов в архиве")
		}
	}
	files := make([]string, 0, len(names))
	for name := range names {
		files = append(files, name)
	}
	sort.Strings(files)
	return files, nil
}

// Stream a portable ZIP with a manifest-based allowlist. Every included file
// is checked via O_NOFOLLOW before sending the response headers. This catches
// known missing, symlinked or special media without falsely returning HTTP 200.
// Concurrent external filesystem modifications may still interrupt streaming,
// so callers must stop other writers for guaranteed consistent snapshots.
func (a *App) zipJob(w http.ResponseWriter, r *http.Request) {
	j, ok := a.readableJob(r)
	if !ok || (j.State != "done" && j.State != "partial") {
		fail(w, 404, "архив не готов")
		return
	}
	base := a.store.archiveDir(j.OwnerID, j.Author, j.PostID)
	// Never package raw Boosty responses containing signed CDN URLs.
	_, manifest, err := readArchiveManifestNoFollow(base)
	if err != nil {
		fail(w, 404, "манифест архива отсутствует или повреждён")
		return
	}
	files, err := portableZipFiles(manifest)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	// Preflight the exact list before creating a streaming ZIP. Opening and
	// closing each descriptor avoids exhausting fd limits for large archives.
	for _, name := range files {
		if r.Context().Err() != nil {
			return
		}
		file, openErr := openArchiveRegular(base, name)
		if openErr != nil {
			fail(w, 409, "архив неполный или повреждён; проверьте файлы")
			return
		}
		if err := file.Close(); err != nil {
			fail(w, 409, "не удалось проверить файл архива")
			return
		}
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", j.Author+"-"+j.PostID+".zip"))
	w.Header().Set("Cache-Control", "no-store")
	writer := zip.NewWriter(w)
	defer writer.Close()
	for _, name := range files {
		if r.Context().Err() != nil {
			return
		}
		file, err := openArchiveRegular(base, name)
		if err != nil {
			// The archive changed after preflight. Returning early ensures the
			// response cannot pretend to contain all the declared assets.
			return
		}
		entry, err := writer.Create(name)
		if err == nil {
			if name == "manifest.json" && j.OwnerID != principal(r).ID {
				sanitized := manifest
				sanitized.Subscription = nil
				sanitized.ContentSHA256 = ""
				err = json.NewEncoder(entry).Encode(sanitized)
			} else {
				_, err = io.Copy(entry, file)
			}
		}
		_ = file.Close()
		if err != nil {
			return
		}
	}
}

func (a *App) jobManifest(w http.ResponseWriter, r *http.Request) {
	j, ok := a.readableJob(r)
	if !ok {
		fail(w, 404, "задача не найдена")
		return
	}
	b, man, err := readArchiveManifestNoFollow(a.store.archiveDir(j.OwnerID, j.Author, j.PostID))
	if err != nil {
		fail(w, 404, "архив ещё не готов")
		return
	}
	// A subscriber's tier is private metadata, not part of a share grant.
	if j.OwnerID != principal(r).ID {
		man.Subscription = nil
		man.ContentSHA256 = "" // Fingerprint may encode the private tier.
		reply(w, 200, man)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, map[string]any{"syncIntervalHours": a.syncInterval().Hours(), "queued": len(a.queue), "maxQueued": cap(a.queue), "maxImportPosts": a.accounts.policy().MaxImportPosts, "maxScanPosts": a.accounts.policy().MaxScanPosts})
}

func (a *App) syncLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.syncOnce()
		}
	}
}
func (a *App) syncOnce() {
	interval := a.syncInterval()
	if interval <= 0 {
		return
	}
	jobs := a.store.all()
	sort.Slice(jobs, func(i, j int) bool { return lastArchiveDate(jobs[i]).Before(lastArchiveDate(jobs[j])) })
	selected := 0
	for _, j := range jobs {
		if selected >= 20 {
			return
		}
		if j.OwnerID == "" || !a.accounts.allowed(j.OwnerID, "autoArchive") || !a.accounts.allowed(j.OwnerID, "createArchives") || !j.AutoSync || (j.State != "done" && j.State != "partial") || time.Since(lastArchiveDate(j)) < interval {
			continue
		}
		if len(a.queue) >= cap(a.queue) {
			return
		}
		j.State = "queued"
		j.Error = ""
		j.DoneFiles, j.TotalFiles = 0, 0
		j.CommentsPartial = false
		if _, err := a.store.transitionToQueued(j.ID, j.OwnerID); err == nil {
			a.queueJob(j.ID)
			selected++
		}
	}
}

func normalizeInterval(raw string) time.Duration {
	if raw == "" {
		return 24 * time.Hour
	}
	h, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || h < 0 || h > 720 {
		return 24 * time.Hour
	}
	return time.Duration(h) * time.Hour
}

func postURL(author, id string) string {
	return "https://boosty.to/" + url.PathEscape(author) + "/posts/" + url.PathEscape(id)
}

func (a *App) feed(w http.ResponseWriter, r *http.Request) {
	author := strings.TrimSpace(r.URL.Query().Get("author"))
	if !boosty.ValidBlog(author) {
		fail(w, 400, "укажите правильное имя автора Boosty")
		return
	}
	c, err := a.client(principal(r).ID)
	if err != nil {
		fail(w, 500, "ошибка авторизации Boosty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	out, err := c.Feed(ctx, author, r.URL.Query().Get("offset"))
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	reply(w, 200, out)
}

// The content's capture time drives synchronization, never sharing or queue
// metadata changes. Legacy unsnapshotted jobs retain their previous schedule.
func lastArchiveDate(j Job) time.Time {
	if !j.ArchiveUpdatedAt.IsZero() {
		return j.ArchiveUpdatedAt
	}
	return j.UpdatedAt
}
