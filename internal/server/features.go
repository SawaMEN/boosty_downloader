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
	"os"
	"path/filepath"
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
	comments := make([]commentDTO, 0, len(result.Comments))
	for _, c := range result.Comments {
		comments = append(comments, viewComment(c, 0))
	}
	reply(w, 200, map[string]any{"comments": comments, "received": result.Received, "expected": result.Expected, "partial": result.Partial, "reason": result.Reason})
}

type commentDTO struct {
	ID        string       `json:"id"`
	Author    string       `json:"author"`
	Text      string       `json:"text"`
	CreatedAt int64        `json:"createdAt"`
	Replies   []commentDTO `json:"replies"`
}

func viewComment(c boosty.Comment, depth int) commentDTO {
	name := c.Author.Name
	if name == "" {
		name = "Пользователь"
	}
	out := commentDTO{ID: c.ID, Author: name, Text: commentText(c), CreatedAt: c.CreatedAt, Replies: []commentDTO{}}
	if depth >= 15 {
		return out
	}
	for _, child := range c.Replies.Data {
		out.Replies = append(out.Replies, viewComment(child, depth+1))
	}
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

// Stream a portable ZIP. All paths come from our controlled archive directory.
// No secrets, settings, or server-side auth files are included.
func (a *App) zipJob(w http.ResponseWriter, r *http.Request) {
	j, ok := a.readableJob(r)
	if !ok || (j.State != "done" && j.State != "partial") {
		fail(w, 404, "архив не готов")
		return
	}
	base := a.store.archiveDir(j.OwnerID, j.Author, j.PostID)
	if _, err := os.Stat(filepath.Join(base, "manifest.json")); err != nil {
		fail(w, 404, "архив отсутствует")
		return
	}
	// Only portable snapshot artefacts belong in a shared ZIP. Raw Boosty
	// payloads can contain signed CDN URLs and must never be redistributed.
	var manifest ArchiveManifest
	manBytes, err := os.ReadFile(filepath.Join(base, "manifest.json"))
	if err != nil || json.Unmarshal(manBytes, &manifest) != nil {
		fail(w, 500, "повреждён манифест архива")
		return
	}
	allowed := map[string]bool{"index.html": true, "manifest.json": true}
	for _, asset := range manifest.Assets {
		if safeArchivePart(asset.Filename) {
			allowed["media/"+asset.Filename] = true
		}
	}
	files := []string{}
	err = filepath.WalkDir(base, func(path string, item os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if item.IsDir() {
			if item.Type()&os.ModeSymlink != 0 {
				return errors.New("symbolic link in archive")
			}
			return nil
		}
		if !item.Type().IsRegular() {
			return errors.New("необычный тип файла в архиве")
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if !allowed[filepath.ToSlash(rel)] {
			return nil
		}
		files = append(files, path)
		if len(files) > 5000 {
			return errors.New("слишком много файлов в архиве")
		}
		return nil
	})
	if err != nil {
		fail(w, 500, "ошибка чтения архива")
		return
	}
	sort.Strings(files)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", j.Author+"-"+j.PostID+".zip"))
	w.Header().Set("Cache-Control", "no-store")
	writer := zip.NewWriter(w)
	defer writer.Close()
	for _, path := range files {
		if r.Context().Err() != nil {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			f.Close()
			return
		}
		entry, err := writer.Create(filepath.ToSlash(rel))
		if err == nil {
			if rel == "manifest.json" && j.OwnerID != principal(r).ID {
				sanitized := manifest
				sanitized.Subscription = nil
				sanitized.ContentSHA256 = "" // Fingerprint may encode the private tier.
				err = json.NewEncoder(entry).Encode(sanitized)
			} else {
				_, err = io.Copy(entry, f)
			}
		}
		f.Close()
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
	b, err := os.ReadFile(filepath.Join(a.store.archiveDir(j.OwnerID, j.Author, j.PostID), "manifest.json"))
	if err != nil {
		fail(w, 404, "архив ещё не готов")
		return
	}
	// A subscriber's tier is private metadata, not part of a share grant.
	if j.OwnerID != principal(r).ID {
		var man ArchiveManifest
		if json.Unmarshal(b, &man) != nil {
			fail(w, 500, "повреждён манифест архива")
			return
		}
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
