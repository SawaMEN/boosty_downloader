package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxArchiveRevisions = 8

type ArchiveRevision struct {
	ID           string                `json:"id"`
	CapturedAt   time.Time             `json:"capturedAt"`
	Title        string                `json:"title"`
	AssetCount   int                   `json:"assetCount"`
	Subscription *CapturedSubscription `json:"subscription,omitempty"`
}

func validRevisionID(id string) bool {
	if len(id) != 24 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func newRevisionID() (string, error) {
	var id [12]byte
	_, err := rand.Read(id[:])
	return hex.EncodeToString(id[:]), err
}

func (s *Storage) revisionsDir(job Job) string {
	return filepath.Join(s.root, "revisions", job.OwnerID, job.Author, job.PostID)
}

// archiveFiles reads an explicit allowlist from the validated manifest.
// Signed private API responses never get exposed through the revision HTTP
// route; only local, regular files are hard-linked into the private snapshot.
func archiveFiles(base string) ([]string, ArchiveManifest, error) {
	_, man, err := readArchiveManifestNoFollow(base)
	if err != nil {
		return nil, man, err
	}
	if len(man.Assets) > 5000 {
		return nil, man, errors.New("слишком много файлов в архиве")
	}
	files := []string{"index.html", "manifest.json"}
	for _, name := range []string{
		"post.json", "post.json.zst",
		"comments.json", "comments.json.zst",
		"comments-raw.json", "comments-raw.json.zst",
	} {
		files = append(files, name)
	}
	used := map[string]bool{}
	for _, asset := range man.Assets {
		if !safeArchivePart(asset.Filename) {
			return nil, man, errors.New("неверное имя файла в манифесте")
		}
		rel := filepath.Join("media", asset.Filename)
		if !used[rel] {
			files = append(files, rel)
			used[rel] = true
		}
	}
	return files, man, nil
}

func linkArchiveSnapshot(src, dst string) (ArchiveManifest, error) {
	files, man, err := archiveFiles(src)
	if err != nil {
		return man, err
	}
	if err := os.MkdirAll(dst, 0700); err != nil {
		return man, err
	}
	for _, rel := range files {
		source := filepath.Join(src, rel)
		info, err := os.Lstat(source)
		if os.IsNotExist(err) && rel != "manifest.json" && rel != "index.html" && !strings.HasPrefix(rel, "media"+string(os.PathSeparator)) {
			continue
		}
		if err != nil {
			return man, err
		}
		if !info.Mode().IsRegular() {
			return man, errors.New("архив содержит неподдерживаемый тип файла")
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return man, err
		}
		if err := os.Link(source, target); err != nil {
			return man, fmt.Errorf("невозможно безопасно создать версию: нужны hardlinks того же тома: %w", err)
		}
	}
	return man, nil
}

// saveArchiveRevision keeps previous finished bytes before a refresh. A failed
// new download must not destroy the last known-good version. All large media
// remain shared inodes, so a revision does not duplicate physical video data.
func (s *Storage) saveArchiveRevision(job Job) (string, error) {
	if !safeArchivePart(job.OwnerID) || !safeArchivePart(job.Author) || !safeArchivePart(job.PostID) {
		return "", errors.New("неверный путь архива")
	}
	live := s.archiveDir(job.OwnerID, job.Author, job.PostID)
	if _, err := os.Lstat(filepath.Join(live, "manifest.json")); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	root := s.revisionsDir(job)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	id, err := newRevisionID()
	if err != nil {
		return "", err
	}
	stage := filepath.Join(root, ".incomplete-"+id)
	defer os.RemoveAll(stage)
	man, err := linkArchiveSnapshot(live, stage)
	if err != nil {
		return "", err
	}
	if man.Author != job.Author || man.PostID != job.PostID {
		return "", errors.New("манифест старой копии не соответствует публикации")
	}
	if err := os.Rename(stage, filepath.Join(root, id)); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Storage) listArchiveRevisions(job Job) ([]ArchiveRevision, error) {
	root := s.revisionsDir(job)
	dirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return []ArchiveRevision{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]ArchiveRevision, 0)
	for _, entry := range dirs {
		if !validRevisionID(entry.Name()) || !entry.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, entry.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var man ArchiveManifest
		if json.Unmarshal(b, &man) != nil || man.Author != job.Author || man.PostID != job.PostID {
			continue
		}
		result = append(result, ArchiveRevision{
			ID: entry.Name(), CapturedAt: man.CapturedAt, Title: man.Title,
			AssetCount: len(man.Assets), Subscription: man.Subscription,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CapturedAt.Equal(result[j].CapturedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].CapturedAt.After(result[j].CapturedAt)
	})
	return result, nil
}

func (s *Storage) pruneArchiveRevisions(job Job) error {
	list, err := s.listArchiveRevisions(job)
	if err != nil {
		return err
	}
	if len(list) <= maxArchiveRevisions {
		return nil
	}
	for _, version := range list[maxArchiveRevisions:] {
		if err := os.RemoveAll(filepath.Join(s.revisionsDir(job), version.ID)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Storage) restoreArchiveRevision(owner, id, rev string) (Job, error) {
	if !validRevisionID(rev) {
		return Job{}, errors.New("неверный ID версии")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	if j == nil || j.OwnerID != owner || j.DeletedAt != nil {
		return Job{}, errors.New("архив не найден")
	}
	if j.State == "queued" || j.State == "running" {
		return Job{}, errors.New("сначала дождитесь завершения загрузки")
	}
	if !safeArchivePart(j.OwnerID) || !safeArchivePart(j.Author) || !safeArchivePart(j.PostID) {
		return Job{}, errors.New("неверный путь архива")
	}
	revision := filepath.Join(s.revisionsDir(*j), rev)
	info, err := os.Lstat(revision)
	if err != nil || !info.IsDir() {
		return Job{}, errors.New("версия не найдена")
	}
	live := s.archiveDir(j.OwnerID, j.Author, j.PostID)
	nonce, err := newRevisionID()
	if err != nil {
		return Job{}, err
	}
	stage := filepath.Join(filepath.Dir(live), ".restore-"+nonce)
	backup := filepath.Join(filepath.Dir(live), ".previous-"+nonce)
	defer os.RemoveAll(stage)
	man, err := linkArchiveSnapshot(revision, stage)
	if err != nil {
		return Job{}, err
	}
	if man.Author != j.Author || man.PostID != j.PostID {
		return Job{}, errors.New("версия принадлежит другой публикации")
	}
	// Preserve the current snapshot in history, including when restoring an
	// older one; if saving that snapshot fails, the live copy is untouched.
	if _, err := s.saveArchiveRevision(*j); err != nil {
		return Job{}, err
	}
	if err := os.Rename(live, backup); err != nil {
		return Job{}, err
	}
	if err := os.Rename(stage, live); err != nil {
		_ = os.Rename(backup, live)
		return Job{}, err
	}
	before := *j
	j.Title = man.Title
	j.ArchiveUpdatedAt = man.CapturedAt
	j.Subscription = man.Subscription
	j.RequiredTier = man.RequiredTier
	j.DoneFiles = len(man.Assets)
	j.TotalFiles = len(man.Assets)
	j.CommentsPartial = len(man.Failures) > 0 || man.CommentsStatus == "частично"
	j.State = "done"
	if j.CommentsPartial {
		j.State = "partial"
	}
	j.Error = ""
	j.AutoSync = false
	j.SharedWith = nil
	j.SharedForAll = false
	j.UpdatedAt = time.Now().UTC()
	if err := s.saveJobsLocked(); err != nil {
		*j = before
		_ = os.RemoveAll(live)
		_ = os.Rename(backup, live)
		return Job{}, err
	}
	restored := *j
	_ = os.RemoveAll(backup)
	_ = s.pruneArchiveRevisions(restored)
	return restored, nil
}

func (a *App) listJobRevisions(w http.ResponseWriter, r *http.Request) {
	job, ok := a.ownedJob(r)
	if !ok {
		fail(w, 404, "архив не найден")
		return
	}
	items, err := a.store.listArchiveRevisions(job)
	if err != nil {
		fail(w, 500, "не удалось прочитать версии")
		return
	}
	reply(w, 200, items)
}

func (a *App) restoreJobRevision(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.restoreArchiveRevision(principal(r).ID, r.PathValue("id"), r.PathValue("rev"))
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, job)
}

func (a *App) revisionFile(w http.ResponseWriter, r *http.Request) {
	job, ok := a.ownedJob(r)
	if !ok || (job.State != "done" && job.State != "partial") || !validRevisionID(r.PathValue("rev")) {
		http.NotFound(w, r)
		return
	}
	base := filepath.Join(a.store.revisionsDir(job), r.PathValue("rev"))
	file := r.PathValue("file")
	if file == "" {
		file = "index.html"
	}
	if file != "index.html" {
		if !strings.HasPrefix(file, "media/") || strings.Count(file, "/") != 1 {
			http.NotFound(w, r)
			return
		}
		_, manifest, err := readArchiveManifestNoFollow(base)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		allowed := false
		for _, asset := range manifest.Assets {
			if safeArchivePart(asset.Filename) && file == "media/"+asset.Filename {
				allowed = true
				break
			}
		}
		if !allowed {
			http.NotFound(w, r)
			return
		}
	}
	serveArchiveRegular(w, r, base, file)
}
