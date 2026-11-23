package server

import (
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
	jobs, _ := a.store.catalogJobs(r.Context(), userID, "shared", a.catalogSQLReads)
	for _, j := range jobs {
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
		// Keep the legacy shared list consistent with the permission-aware
		// catalog: a share grant does not reveal other recipients or internal IDs.
		j.Subscription = nil
		j.SharedWith = nil
		j.OwnerID = ""
		j.Error = ""
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
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return false, err
	}
	j, ok := s.jobs[jobID]
	if !ok || j.OwnerID != owner || j.DeletedAt != nil {
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
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return false, err
	}
	j, ok := s.jobs[jobID]
	if !ok || j.OwnerID != owner || j.DeletedAt != nil {
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
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[jobID]
	if !ok || j.OwnerID != owner || j.DeletedAt != nil {
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
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return Job{}, false
	}
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
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return Job{}, false, err
	}
	for _, old := range s.jobs {
		if old.OwnerID == j.OwnerID && old.Author == j.Author && old.PostID == j.PostID {
			if old.DeletedAt != nil {
				return Job{}, false, errors.New("архив находится в корзине — сначала восстановите его")
			}
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

// removeOwned hides finished copies atomically in jobs.json, retaining bytes
// until permanent deletion. Restoring never re-grants previous shares.
func (s *Storage) removeOwned(owner, id, author string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return 0, err
	}
	if author != "" {
		for _, v := range s.imports {
			if v.OwnerID == owner && v.Author == author && (v.State == "queued" || v.State == "running") {
				return 0, errors.New("сначала остановите импорт автора")
			}
		}
	}
	var selected []*Job
	for key, j := range s.jobs {
		if j.OwnerID != owner || (id != "" && id != key) || (author != "" && j.Author != author) || j.DeletedAt != nil {
			continue
		}
		if j.State == "running" || j.State == "queued" {
			return 0, errors.New("сначала завершите активные загрузки автора")
		}
		if !safeArchivePart(j.OwnerID) || !safeArchivePart(j.Author) || !safeArchivePart(j.PostID) {
			return 0, errors.New("неверные параметры пути архива")
		}
		selected = append(selected, j)
	}
	if len(selected) == 0 {
		return 0, errors.New("архив не найден")
	}
	now := time.Now().UTC()
	previous := make([]Job, len(selected))
	for i, j := range selected {
		previous[i] = *j
		j.DeletedAt = &now
		j.AutoSync = false
		j.SharedForAll = false
		j.SharedWith = nil
		j.UpdatedAt = now
	}
	if err := s.saveJobsLocked(); err != nil {
		for i, j := range selected {
			*j = previous[i]
		}
		return 0, err
	}
	return len(selected), nil
}

func (s *Storage) trashed(owner string) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return nil
	}
	out := make([]Job, 0)
	for _, j := range s.jobs {
		if j.OwnerID == owner && j.DeletedAt != nil {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DeletedAt.After(*out[j].DeletedAt)
	})
	return out
}

// trashedJob returns an immutable snapshot for choosing the same physical
// archive gate used by ZIP, media, revision restores and background refreshes.
// The purge itself must revalidate DeletedAt and retention under Storage.mu:
// an owner may restore and re-trash the entry after this lookup.
func (s *Storage) trashedJob(owner, id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return Job{}, false
	}
	job := s.jobs[id]
	if job == nil || job.OwnerID != owner || job.DeletedAt == nil {
		return Job{}, false
	}
	return *job, true
}

// HTTP/manual and scheduled sweeps share the same storage-level purge gate.
// Direct Storage.purgeOwned(Before) calls must be equally safe.
func (a *App) purgeTrashedJob(owner, id string, cutoff time.Time) (int, error) {
	return a.store.purgeOwnedBefore(owner, id, "", cutoff)
}

func (s *Storage) restoreOwned(owner, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok || j.OwnerID != owner || j.DeletedAt == nil {
		return Job{}, errors.New("архив не найден в корзине")
	}
	if (j.State == "done" || j.State == "partial") && (!safeArchivePart(j.OwnerID) || !safeArchivePart(j.Author) || !safeArchivePart(j.PostID)) {
		return Job{}, errors.New("неверный путь архива")
	}
	if j.State == "done" || j.State == "partial" {
		info, err := os.Lstat(s.archiveDir(j.OwnerID, j.Author, j.PostID))
		if err != nil || !info.IsDir() {
			return Job{}, errors.New("файлы архива отсутствуют — восстановление невозможно")
		}
	}
	old := *j
	j.DeletedAt = nil
	j.UpdatedAt = time.Now().UTC()
	if err := s.saveJobsLocked(); err != nil {
		*j = old
		return Job{}, err
	}
	return *j, nil
}

// purgeOwned permanently removes only previously trashed entries, staging
// folders for atomic metadata rollback.
func (s *Storage) purgeOwned(owner, id, author string) (int, error) {
	return s.purgeOwnedBefore(owner, id, author, time.Time{})
}

// Every permanent delete, including callers that bypass HTTP, obtains an
// archive writer gate BEFORE Storage.mu. Readers acquire the two locks in the
// reverse nesting order (gate, then mu), so taking mu while waiting for a gate
// would deadlock. Exact-ID purges lock the physical post; bulk author/owner
// purges lock all 128 stripes to cover a changing candidate set atomically.
func (s *Storage) purgeOwnedBefore(owner, id, author string, cutoff time.Time) (int, error) {
	var lockedJob Job
	var unlock func()
	var ok bool
	if id != "" {
		job, found := s.trashedJob(owner, id)
		if !found {
			return 0, errors.New("архив не найден")
		}
		if !safeArchivePart(job.OwnerID) || !safeArchivePart(job.Author) || !safeArchivePart(job.PostID) {
			return 0, errors.New("неверные параметры пути архива")
		}
		lockedJob = job
		unlock, ok = s.gates.tryWrite(job)
	} else {
		unlock, ok = s.gates.tryWriteAll()
	}
	if !ok {
		return 0, errors.New("архив используется: завершите скачивание или восстановление")
	}
	defer unlock()
	return s.purgeOwnedBeforeUnderGate(owner, id, author, cutoff, lockedJob)
}

// Compare retention within the same lock as the folder move and metadata
// delete. Restore/re-trash resets DeletedAt, which is rechecked after the
// physical archive gate was acquired. Call only from purgeOwnedBefore.
func (s *Storage) purgeOwnedBeforeUnderGate(owner, id, author string, cutoff time.Time, lockedJob Job) (int, error) {
	s.mu.Lock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	if s.primary != nil {
		s.mu.Unlock()
		return 0, errors.New("physical purge in candidate SQL-primary requires a separate filesystem transaction cutover")
	}
	// A previous completed purge may have been interrupted during cleanup.
	// Finish its recovery before planning another physical transaction.
	if err := recoverArchivePurgeLocked(s.root); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	if err := recoverArchiveRestoreLocked(s.root); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	if author != "" {
		for _, v := range s.imports {
			if v.OwnerID == owner && v.Author == author && (v.State == "queued" || v.State == "running") {
				s.mu.Unlock()
				return 0, errors.New("сначала остановите импорт автора")
			}
		}
	}
	var ids []string
	folders := map[string]bool{}
	for key, j := range s.jobs {
		if j.OwnerID != owner || (id != "" && id != key) || (author != "" && j.Author != author) || j.DeletedAt == nil || (!cutoff.IsZero() && !j.DeletedAt.Before(cutoff)) {
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
		if id != "" && (j.OwnerID != lockedJob.OwnerID || j.Author != lockedJob.Author || j.PostID != lockedJob.PostID) {
			s.mu.Unlock()
			return 0, errors.New("путь архива изменился во время удаления")
		}
		ids = append(ids, key)
		folders[s.archiveDir(j.OwnerID, j.Author, j.PostID)] = true
		folders[s.revisionsDir(*j)] = true
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
			delete(folders, s.revisionsDir(*j))
		}
	}
	// Stage physical folders only after a durable journal pins BEFORE and
	// AFTER jobs.json generations. A crash before metadata publication rolls
	// back every staged rename; after publication it completes removal.
	// A legacy JSON deployment may predate SQL-required timestamps. The
	// purge journal needs an exact JSON/memory generation match, not an SQL
	// migration validator that would refuse safe deletion of older archives.
	before, err := os.ReadFile(filepath.Join(s.root, "jobs.json"))
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	var diskJobs map[string]Job
	if err := json.Unmarshal(before, &diskJobs); err != nil || diskJobs == nil {
		s.mu.Unlock()
		return 0, errors.New("invalid JSON archive generation before physical purge")
	}
	for id, job := range diskJobs {
		if id != job.ID {
			s.mu.Unlock()
			return 0, errors.New("archive identity does not match JSON key")
		}
	}
	currentJobs := make(map[string]Job, len(s.jobs))
	for key, job := range s.jobs {
		if job == nil {
			s.mu.Unlock()
			return 0, errors.New("nil archive while planning permanent deletion")
		}
		currentJobs[key] = *job
	}
	if !sqliteShadowEqual(currentJobs, diskJobs) {
		s.mu.Unlock()
		return 0, errors.New("archive purge blocked: in-memory jobs differ from jobs.json")
	}
	afterJobs := make(map[string]Job, len(currentJobs)-len(ids))
	for key, job := range currentJobs {
		if !selected[key] {
			afterJobs[key] = job
		}
	}
	after, err := json.MarshalIndent(afterJobs, "", "  ")
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	moves, err := planPurgeMoves(s.root, folders)
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	journal := purgeJournal{Version: 1, BeforeHash: metadataDigest(before), AfterHash: metadataDigest(after), Moves: moves}
	backup := make(map[string]*Job, len(ids))
	// Recover and classify an error by the on-disk generation, not by the
	// point at which the write returned an error. An fsync error can occur
	// after rename: this must never cause metadata rollback over a purge
	// that already committed.
	abort := func(cause error) (int, error) {
		if recoveryErr := recoverArchivePurgeLocked(s.root); recoveryErr != nil {
			s.primaryFatal = fmt.Errorf("archive purge recovery failed: %w; original failure: %v", recoveryErr, cause)
			s.mu.Unlock()
			return 0, s.primaryFatal
		}
		actual, readErr := os.ReadFile(filepath.Join(s.root, "jobs.json"))
		if readErr != nil {
			s.primaryFatal = fmt.Errorf("archive purge cannot classify jobs generation: %w", readErr)
			s.mu.Unlock()
			return 0, s.primaryFatal
		}
		if metadataDigest(actual) == journal.AfterHash {
			for _, key := range ids {
				delete(s.jobs, key)
			}
			s.mu.Unlock()
			if blobErr := s.collectUnusedBlobs(); blobErr != nil {
				return len(ids), fmt.Errorf("archive purge committed but blob cleanup failed: %w", blobErr)
			}
			return len(ids), nil
		}
		if metadataDigest(actual) == journal.BeforeHash {
			for key, job := range backup {
				s.jobs[key] = job
			}
			s.mu.Unlock()
			return 0, cause
		}
		s.primaryFatal = errors.New("archive purge metadata changed to an unrecognized generation")
		s.mu.Unlock()
		return 0, s.primaryFatal
	}
	if err := writePurgeJournal(s.root, journal); err != nil {
		return abort(err)
	}
	if s.purgeCheckpoint != nil {
		if err := s.purgeCheckpoint("journal"); err != nil {
			return abort(err)
		}
	}
	for _, move := range moves {
		if err := purgeStageMove(s.root, move); err != nil {
			return abort(err)
		}
		if s.purgeCheckpoint != nil {
			if err := s.purgeCheckpoint("moved"); err != nil {
				return abort(err)
			}
		}
	}
	for _, key := range ids {
		backup[key] = s.jobs[key]
		delete(s.jobs, key)
	}
	if err := s.saveJobsLocked(); err != nil {
		return abort(err)
	}
	if s.purgeCheckpoint != nil {
		if err := s.purgeCheckpoint("metadata"); err != nil {
			return abort(err)
		}
	}
	if err := recoverArchivePurgeLocked(s.root); err != nil {
		s.primaryFatal = fmt.Errorf("archive purge committed, cleanup pending until recovery: %w", err)
		s.mu.Unlock()
		return len(ids), s.primaryFatal
	}
	s.notifySQLiteMedia()
	s.mu.Unlock()
	if cleanupErr := s.collectUnusedBlobs(); cleanupErr != nil {
		return len(ids), fmt.Errorf("archives purged but unreferenced blob cleanup failed: %w", cleanupErr)
	}
	return len(ids), nil
}

func (a *App) listTrash(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, a.store.trashed(principal(r).ID))
}
func (a *App) restoreTrash(w http.ResponseWriter, r *http.Request) {
	j, err := a.store.restoreOwned(principal(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, j)
}
func (a *App) purgeTrash(w http.ResponseWriter, r *http.Request) {
	request := TrashAuditEntry{At: time.Now().UTC(), Trigger: "owner_request", OwnerID: principal(r).ID, JobID: r.PathValue("id")}
	if err := a.store.appendTrashAudit(request); err != nil {
		fail(w, http.StatusServiceUnavailable, "журнал очистки недоступен: удаление отменено")
		return
	}
	n, err := a.purgeTrashedJob(principal(r).ID, r.PathValue("id"), time.Time{})
	if err != nil {
		_ = a.store.appendTrashAudit(TrashAuditEntry{At: time.Now().UTC(), Trigger: "owner_failed", OwnerID: principal(r).ID, JobID: r.PathValue("id"), Error: err.Error()})
		fail(w, 409, err.Error())
		return
	}
	entry := TrashAuditEntry{At: time.Now().UTC(), Trigger: "owner", OwnerID: principal(r).ID, JobID: r.PathValue("id"), Removed: n}
	if auditErr := a.store.appendTrashAudit(entry); auditErr != nil {
		reply(w, 200, map[string]any{"purged": n, "warning": "архив удалён, но журнал не удалось сохранить"})
		return
	}
	reply(w, 200, map[string]any{"purged": n})
}

func (a *App) deleteJob(w http.ResponseWriter, r *http.Request) {
	count, err := a.store.removeOwned(principal(r).ID, r.PathValue("id"), "")
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, map[string]any{"deleted": count, "trashed": count})
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
	reply(w, 200, map[string]any{"deleted": count, "trashed": count})
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
