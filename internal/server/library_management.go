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

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

// Sharing is read-only, explicitly granted to an existing local account, and
// revocable by the archive owner. It does not share Boosty credentials.
func (a *App) readableJob(r *http.Request) (Job, bool) {
	j, ok := a.store.get(r.PathValue("id"))
	if !ok {
		return Job{}, false
	}
	userID := principal(r).ID
	if j.OwnerID == userID {
		return j, true
	}
	if !a.accounts.allowed(userID, "browseShared") {
		return Job{}, false
	}
	if j.State != "done" && j.State != "partial" {
		return Job{}, false
	}
	if j.SharedForAll {
		return j, true
	}
	for _, member := range j.SharedWith {
		if member == userID {
			return j, true
		}
	}
	return Job{}, false
}

// Shared archive search is restricted to completed copies visible to the
// logged-in user. All-members visibility never grants anonymous access.
func (a *App) sharedJobs(w http.ResponseWriter, r *http.Request) {
	userID := principal(r).ID
	authorFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("author")))
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len(authorFilter) > 128 || len(query) > 128 {
		fail(w, 400, "слишком длинный поисковый запрос")
		return
	}
	type sharedEntry struct {
		Job
		OwnerName string `json:"ownerName"`
	}
	results := make([]sharedEntry, 0)
	for _, j := range a.store.all() {
		if j.OwnerID == userID || (j.State != "done" && j.State != "partial") {
			continue
		}
		if !j.SharedForAll && !contains(j.SharedWith, userID) {
			continue
		}
		if authorFilter != "" && !strings.Contains(strings.ToLower(j.Author), authorFilter) {
			continue
		}
		ownerName := a.accounts.username(j.OwnerID)
		if ownerName == "" {
			ownerName = "Удалённый участник"
		}
		if query != "" && !strings.Contains(strings.ToLower(j.Author), query) &&
			!strings.Contains(strings.ToLower(j.Title), query) &&
			!strings.Contains(strings.ToLower(ownerName), query) {
			continue
		}
		results = append(results, sharedEntry{Job: j, OwnerName: ownerName})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].ArchiveUpdatedAt.Equal(results[j].ArchiveUpdatedAt) {
			return results[i].ID < results[j].ID
		}
		return results[i].ArchiveUpdatedAt.After(results[j].ArchiveUpdatedAt)
	})
	reply(w, 200, results)
}

// Only the owner can expose a completed archive to all registered users.
func (a *App) setJobVisibility(w http.ResponseWriter, r *http.Request) {
	j, ok := a.ownedJob(r)
	if !ok {
		fail(w, 404, "архив не найден")
		return
	}
	if j.State != "done" && j.State != "partial" {
		fail(w, 409, "можно делиться только завершённым архивом")
		return
	}
	var input struct {
		Everyone bool `json:"everyone"`
	}
	if decodeRequest(w, r, &input) != nil {
		return
	}
	if input.Everyone && !a.accounts.allowed(principal(r).ID, "sharePublic") {
		fail(w, 403, "публикация для всех отключена администратором")
		return
	}
	changed, err := a.store.setEveryone(j.ID, j.OwnerID, input.Everyone)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, map[string]any{"everyone": input.Everyone, "changed": changed})
}

func (s *Storage) setEveryone(jobID, owner string, everyone bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok || j.OwnerID != owner {
		return false, errors.New("архив не найден")
	}
	if j.State != "done" && j.State != "partial" {
		return false, errors.New("архив ещё не готов")
	}
	if j.SharedForAll == everyone {
		return false, nil
	}
	j.SharedForAll = everyone
	j.UpdatedAt = time.Now().UTC()
	if err := s.saveJobsLocked(); err != nil {
		j.SharedForAll = !everyone
		return false, err
	}
	return true, nil
}

func (a *App) jobShares(w http.ResponseWriter, r *http.Request) {
	j, ok := a.ownedJob(r)
	if !ok {
		fail(w, 404, "архив не найден")
		return
	}
	if r.Method == "GET" {
		users := make([]string, 0, len(j.SharedWith))
		for _, id := range j.SharedWith {
			if name := a.accounts.username(id); name != "" {
				users = append(users, name)
			}
		}
		sort.Strings(users)
		reply(w, 200, map[string]any{"users": users, "everyone": j.SharedForAll, "archiveUpdatedAt": j.ArchiveUpdatedAt})
		return
	}
	if j.State != "done" && j.State != "partial" {
		fail(w, 409, "можно делиться только завершённым архивом")
		return
	}
	var input struct {
		Username string `json:"username"`
	}
	if decodeRequest(w, r, &input) != nil {
		return
	}
	if r.Method == "POST" && !a.accounts.allowed(principal(r).ID, "shareDirect") {
		fail(w, 403, "персональный доступ запрещён администратором")
		return
	}
	target, exists := a.accounts.userByName(strings.TrimSpace(input.Username))
	if !exists {
		fail(w, 404, "пользователь не найден")
		return
	}
	if target.ID == j.OwnerID {
		fail(w, 400, "архив уже принадлежит вам")
		return
	}
	changed, err := a.store.changeShare(j.ID, j.OwnerID, target.ID, r.Method == "POST")
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, map[string]any{"ok": true, "changed": changed})
}

func (s *Storage) changeShare(jobID, owner, target string, grant bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok || j.OwnerID != owner {
		return false, errors.New("архив не найден")
	}
	if j.State != "done" && j.State != "partial" {
		return false, errors.New("архив ещё не готов")
	}
	previous := append([]string{}, j.SharedWith...)
	found := false
	selected := make([]string, 0, len(previous)+1)
	for _, id := range previous {
		if id == target {
			found = true
			continue
		}
		selected = append(selected, id)
	}
	if grant {
		if found {
			return false, nil
		}
		if len(selected) >= 100 {
			return false, errors.New("достигнут лимит 100 участников")
		}
		selected = append(selected, target)
	} else if !found {
		return false, nil
	}
	j.SharedWith = selected
	j.UpdatedAt = time.Now().UTC()
	if err := s.saveJobsLocked(); err != nil {
		j.SharedWith = previous
		return false, err
	}
	return true, nil
}

func (s *Storage) transitionToQueued(jobID, owner string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok || j.OwnerID != owner {
		return Job{}, errors.New("архив не найден")
	}
	if j.State == "queued" || j.State == "running" {
		return Job{}, errors.New("задача уже выполняется")
	}
	old := *j
	j.State = "queued"
	j.Error = ""
	j.DoneFiles = 0
	j.TotalFiles = 0
	j.CommentsPartial = false
	j.UpdatedAt = time.Now().UTC()
	if err := s.saveJobsLocked(); err != nil {
		*j = old
		return Job{}, err
	}
	return *j, nil
}
func (s *Storage) claimQueued(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.State != "queued" {
		return Job{}, false
	}
	old := *j
	j.State = "running"
	j.UpdatedAt = time.Now().UTC()
	if s.saveJobsLocked() != nil {
		*j = old
		return Job{}, false
	}
	return *j, true
}

func (s *Storage) createUniqueJob(j Job) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.jobs {
		if old.OwnerID == j.OwnerID && old.Author == j.Author && old.PostID == j.PostID {
			return *old, false, nil
		}
	}
	j.UpdatedAt = time.Now().UTC()
	s.jobs[j.ID] = &j
	if err := s.saveJobsLocked(); err != nil {
		delete(s.jobs, j.ID)
		return Job{}, false, err
	}
	return j, true, nil
}

// Removes complete/failed archives only. Stages existing folders in the same
// filesystem before committing metadata, allowing rollback on write failure.
// Running or queued downloads are rejected instead of racing with the worker.
func (s *Storage) removeOwned(owner, id, author string) (int, error) {
	s.mu.Lock()
	if author != "" {
		for _, v := range s.imports {
			if v.OwnerID == owner && v.Author == author && (v.State == "queued" || v.State == "running") {
				s.mu.Unlock()
				return 0, errors.New("сначала остановите импорт автора")
			}
		}
	}
	type moved struct{ src, dst string }
	var moves []moved
	var ids []string
	folders := map[string]bool{}
	for key, j := range s.jobs {
		if j.OwnerID != owner || (id != "" && id != key) || (author != "" && j.Author != author) {
			continue
		}
		if j.State == "running" || j.State == "queued" {
			s.mu.Unlock()
			return 0, errors.New("сначала завершите активные загрузки автора")
		}
		if !safeArchivePart(j.OwnerID) || !safeArchivePart(j.Author) || !safeArchivePart(j.PostID) {
			s.mu.Unlock()
			return 0, errors.New("неверные параметры пути архива")
		}
		ids = append(ids, key)
		folders[s.archiveDir(j.OwnerID, j.Author, j.PostID)] = true
	}
	if len(ids) == 0 {
		s.mu.Unlock()
		return 0, errors.New("архив не найден")
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	for _, j := range s.jobs {
		if !selected[j.ID] {
			delete(folders, s.archiveDir(j.OwnerID, j.Author, j.PostID))
		}
	}
	trash := filepath.Join(s.root, "trash")
	if err := os.MkdirAll(trash, 0700); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	rollback := func() {
		for i := len(moves) - 1; i >= 0; i-- {
			_ = os.Rename(moves[i].dst, moves[i].src)
		}
	}
	for folder := range folders {
		info, err := os.Lstat(folder)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			rollback()
			s.mu.Unlock()
			return 0, err
		}
		if !info.IsDir() {
			rollback()
			s.mu.Unlock()
			return 0, errors.New("путь архива не является каталогом")
		}
		b := make([]byte, 12)
		if _, err = rand.Read(b); err != nil {
			rollback()
			s.mu.Unlock()
			return 0, err
		}
		dest := filepath.Join(trash, hex.EncodeToString(b))
		if err = os.Rename(folder, dest); err != nil {
			rollback()
			s.mu.Unlock()
			return 0, err
		}
		moves = append(moves, moved{folder, dest})
	}
	backup := map[string]*Job{}
	for _, key := range ids {
		backup[key] = s.jobs[key]
		delete(s.jobs, key)
	}
	if err := s.saveJobsLocked(); err != nil {
		for key, j := range backup {
			s.jobs[key] = j
		}
		rollback()
		s.mu.Unlock()
		return 0, err
	}
	s.mu.Unlock()
	var cleanupErr error
	for _, move := range moves {
		if err := os.RemoveAll(move.dst); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr == nil {
		cleanupErr = s.collectUnusedBlobs()
	}
	if cleanupErr != nil {
		return len(ids), fmt.Errorf("архивы удалены из медиатеки, но очистка корзины не завершена: %w", cleanupErr)
	}
	return len(ids), nil
}

func (a *App) deleteJob(w http.ResponseWriter, r *http.Request) {
	count, err := a.store.removeOwned(principal(r).ID, r.PathValue("id"), "")
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, map[string]any{"deleted": count})
}
func (a *App) deleteAuthorArchives(w http.ResponseWriter, r *http.Request) {
	author := r.PathValue("author")
	if !boosty.ValidBlog(author) || !safeArchivePart(author) {
		fail(w, 400, "неверное имя автора")
		return
	}
	var input struct {
		Confirm string `json:"confirm"`
	}
	if decodeRequest(w, r, &input) != nil {
		return
	}
	if input.Confirm != author {
		fail(w, 400, "для подтверждения укажите имя автора")
		return
	}
	count, err := a.store.removeOwned(principal(r).ID, "", author)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, map[string]any{"deleted": count})
}

func (a *App) authors(w http.ResponseWriter, r *http.Request) {
	me := principal(r).ID
	if r.Method == "GET" {
		preferred := a.store.authorsFor(me)
		count := map[string]int{}
		for _, j := range a.store.all() {
			if j.OwnerID == me {
				count[j.Author]++
			}
		}
		type authorView struct {
			Name     string `json:"name"`
			Pinned   bool   `json:"pinned"`
			Archives int    `json:"archives"`
		}
		names := map[string]bool{}
		for _, name := range preferred {
			names[name] = true
		}
		for name := range count {
			names[name] = true
		}
		result := make([]authorView, 0, len(names))
		for name := range names {
			result = append(result, authorView{name, contains(preferred, name), count[name]})
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].Pinned != result[j].Pinned {
				return result[i].Pinned
			}
			return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
		})
		reply(w, 200, result)
		return
	}
	var input struct {
		Author string `json:"author"`
	}
	if decodeRequest(w, r, &input) != nil {
		return
	}
	if !boosty.ValidBlog(input.Author) || !safeArchivePart(input.Author) {
		fail(w, 400, "недопустимое имя автора")
		return
	}
	if err := a.store.pinAuthor(me, input.Author); err != nil {
		fail(w, 400, err.Error())
		return
	}
	reply(w, 200, map[string]any{"ok": true})
}
func (a *App) unpinAuthor(w http.ResponseWriter, r *http.Request) {
	author := r.PathValue("author")
	if !boosty.ValidBlog(author) || !safeArchivePart(author) {
		fail(w, 400, "неверное имя автора")
		return
	}
	if err := a.store.unpinAuthor(principal(r).ID, author); err != nil {
		fail(w, 500, err.Error())
		return
	}
	reply(w, 200, map[string]any{"ok": true})
}
func contains(v []string, x string) bool {
	for _, item := range v {
		if item == x {
			return true
		}
	}
	return false
}

func (s *Storage) authorsFor(owner string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.authors[owner]...)
}
func (s *Storage) pinAuthor(owner, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := append([]string{}, s.authors[owner]...)
	if contains(prev, name) {
		return nil
	}
	if len(prev) >= 200 {
		return errors.New("достигнут лимит избранных авторов")
	}
	s.authors[owner] = append(prev, name)
	if err := s.saveAuthorsLocked(); err != nil {
		s.authors[owner] = prev
		return err
	}
	return nil
}
func (s *Storage) unpinAuthor(owner, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := append([]string{}, s.authors[owner]...)
	next := make([]string, 0, len(prev))
	for _, author := range prev {
		if author != name {
			next = append(next, author)
		}
	}
	s.authors[owner] = next
	if err := s.saveAuthorsLocked(); err != nil {
		s.authors[owner] = prev
		return err
	}
	return nil
}
func (s *Storage) saveAuthorsLocked() error {
	raw, err := json.MarshalIndent(s.authors, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.root, "authors.json"), raw)
}
