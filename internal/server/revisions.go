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
		// Pin a regular source file using openat(O_NOFOLLOW), including
		// every parent component, before hard-linking it into history.
		opened, err := openArchiveRegular(src, filepath.ToSlash(rel))
		if os.IsNotExist(err) && rel != "manifest.json" && rel != "index.html" && !strings.HasPrefix(rel, "media"+string(os.PathSeparator)) {
			continue
		}
		if err != nil {
			return man, err
		}
		sourceInfo, err := opened.Stat()
		if err != nil {
			opened.Close()
			return man, err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			opened.Close()
			return man, err
		}
		if err := os.Link(source, target); err != nil {
			opened.Close()
			return man, fmt.Errorf("невозможно безопасно создать версию: нужны hardlinks того же тома: %w", err)
		}
		// Linking still resolves a pathname. Ensure it resolved to the
		// exact inode opened safely above, not a swapped file or parent.
		linkedInfo, linkErr := os.Lstat(target)
		if linkErr != nil || !linkedInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, linkedInfo) {
			_ = os.Remove(target)
			opened.Close()
			return man, errors.New("archive source changed while linking a revision")
		}
		if err := opened.Close(); err != nil {
			return man, err
		}
	}
	return man, nil
}

// saveArchiveRevision keeps previous finished bytes before a refresh. A failed
// new download must not destroy the last known-good version. All large media
// remain shared inodes, so a revision does not duplicate physical video data.
func (s *Storage) saveArchiveRevision(job Job) (string, error) {
	// SQL-first history staging and its publication journal must share the
	// metadata writer lock. JSON-only restore already owns this lock.
	if s.primary != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
			return "", err
		}
		current := s.jobs[job.ID]
		if current == nil || current.OwnerID != job.OwnerID || current.Author != job.Author ||
			current.PostID != job.PostID || current.DeletedAt != nil {
			return "", errors.New("SQL history job identity changed")
		}
	}
	if !safeArchivePart(job.OwnerID) || !safeArchivePart(job.Author) || !safeArchivePart(job.PostID) {
		return "", errors.New("неверный путь архива")
	}
	live := s.archiveDir(job.OwnerID, job.Author, job.PostID)
	if _, err := purgeSafePath(s.root, live); err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(live, "manifest.json")); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	root := s.revisionsDir(job)
	if _, err := purgeSafePath(s.root, root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	id, err := newRevisionID()
	if err != nil {
		return "", err
	}
	stage := filepath.Join(root, ".incomplete-"+id)
	if err := os.Mkdir(stage, 0700); err != nil {
		return "", err
	}
	defer func() {
		// A prepared SQL history journal owns its stage until recovery.
		if s.primary != nil {
			if _, err := os.Lstat(filepath.Join(s.root, sqliteHistoryJournalName)); !os.IsNotExist(err) {
				return
			}
		}
		_ = os.RemoveAll(stage)
	}()
	man, err := linkArchiveSnapshot(live, stage)
	if err != nil {
		return "", err
	}
	if man.Author != job.Author || man.PostID != job.PostID {
		return "", errors.New("манифест старой копии не соответствует публикации")
	}

	// A revision is the only durable fallback when replacing the live
	// directory. Fsync hardlink directory entries before returning success.
	media := filepath.Join(stage, "media")
	if info, err := os.Lstat(media); err == nil && info.IsDir() {
		if err := syncMetadataDirectory(media); err != nil {
			return "", err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := syncMetadataDirectory(stage); err != nil {
		return "", err
	}
	if err := syncMetadataDirectory(root); err != nil {
		return "", err
	}
	// A newly created owner/author/post history path must survive power
	// loss as well as the stage itself.
	for dir := root; ; dir = filepath.Dir(dir) {
		if err := syncMetadataDirectory(dir); err != nil {
			return "", err
		}
		if filepath.Clean(dir) == filepath.Clean(s.root) {
			break
		}
		if filepath.Dir(dir) == dir {
			return "", errors.New("history path escaped data directory")
		}
	}
	if s.primary != nil {
		if err := s.publishSQLHistoryLocked(job, id, stage); err != nil {
			return "", err
		}
	} else if err := durableRevisionRestoreRename(s.root, stage, filepath.Join(root, id)); err != nil {
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
	// Until a SQL history deletion journal exists, retain historical
	// snapshots rather than silently remove files still indexed in SQL.
	if s.primary != nil {
		return nil
	}
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
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return Job{}, err
	}
	if s.primary != nil {
		return Job{}, errors.New("revision restoration in candidate SQL-primary requires a separate filesystem cutover")
	}
	// A prior failed attempt must be reconciled before starting a second
	// restore or exposing a different metadata generation to the journal.
	if err := recoverArchiveRestoreLocked(s.root); err != nil {
		return Job{}, err
	}
	if err := recoverArchivePurgeLocked(s.root); err != nil {
		return Job{}, err
	}
	j := s.jobs[id]
	if j == nil || j.OwnerID != owner || j.DeletedAt != nil {
		return Job{}, errors.New("архив не найден")
	}
	if j.State == "queued" || j.State == "running" {
		return Job{}, errors.New("сначала дождитесь завершения загрузки")
	}
	if !safeArchivePart(j.OwnerID) || !safeArchivePart(j.Author) || !safeArchivePart(j.PostID) || !validRevisionID(j.ID) {
		return Job{}, errors.New("неверный путь архива")
	}
	live := s.archiveDir(j.OwnerID, j.Author, j.PostID)
	revision := filepath.Join(s.revisionsDir(*j), rev)
	if source, err := purgeSafePath(s.root, revision); err != nil || source == nil {
		return Job{}, errors.New("версия не найдена или небезопасна")
	}
	if existing, err := purgeSafePath(s.root, live); err != nil || existing == nil {
		return Job{}, errors.New("действующий архив отсутствует или небезопасен")
	}
	before, err := os.ReadFile(filepath.Join(s.root, "jobs.json"))
	if err != nil {
		return Job{}, err
	}
	var diskJobs map[string]Job
	if err := json.Unmarshal(before, &diskJobs); err != nil || diskJobs == nil {
		return Job{}, errors.New("некорректные метаданные перед восстановлением версии")
	}
	current := make(map[string]Job, len(s.jobs))
	for key, item := range s.jobs {
		if item == nil || diskJobs[key].ID != key {
			return Job{}, errors.New("ошибка соответствия задания и jobs.json")
		}
		current[key] = *item
	}
	if !sqliteShadowEqual(current, diskJobs) {
		return Job{}, errors.New("jobs.json не соответствует памяти перед восстановлением")
	}
	old := *j
	oldManifest, err := revisionRestoreManifestDigest(live, old.Author, old.PostID)
	if err != nil {
		return Job{}, err
	}
	nonce, err := newRevisionID()
	if err != nil {
		return Job{}, err
	}
	journal := revisionRestoreJournal{
		Version: 1, JobID: j.ID, Owner: j.OwnerID, Author: j.Author,
		Post: j.PostID, Revision: rev, Nonce: nonce, BeforeHash: metadataDigest(before),
		OldManifest: oldManifest,
	}
	_, stage, backup, err := revisionRestorePaths(s.root, journal)
	if err != nil {
		return Job{}, err
	}
	for _, dir := range []string{stage, backup} {
		if exists, err := purgeSafePath(s.root, dir); err != nil || exists != nil {
			return Job{}, errors.New("временное место восстановления занято или небезопасно")
		}
	}
	// Keep the previous finished version before touching its live directory.
	// A pre-journal crash can leave an extra history snapshot, but never loses
	// the original live archive or changes ownership/grants.
	if _, err := s.saveArchiveRevision(old); err != nil {
		return Job{}, err
	}
	man, err := linkArchiveSnapshot(revision, stage)
	if err != nil {
		_ = os.RemoveAll(stage)
		return Job{}, err
	}
	if man.Author != old.Author || man.PostID != old.PostID {
		_ = os.RemoveAll(stage)
		return Job{}, errors.New("версия принадлежит другой публикации")
	}
	journal.NewManifest, err = revisionRestoreManifestDigest(stage, old.Author, old.PostID)
	if err != nil {
		_ = os.RemoveAll(stage)
		return Job{}, err
	}
	journal.OldDevice, journal.OldInode, err = revisionRestoreIdentity(s.root, live)
	if err != nil {
		_ = os.RemoveAll(stage)
		return Job{}, err
	}
	journal.NewDevice, journal.NewInode, err = revisionRestoreIdentity(s.root, stage)
	if err != nil {
		_ = os.RemoveAll(stage)
		return Job{}, err
	}
	// Persist the stage hardlinks and its new directory entries before a
	// journal could ever authorize metadata to point at the new live folder.
	if media, err := purgeSafePath(s.root, filepath.Join(stage, "media")); err != nil {
		_ = os.RemoveAll(stage)
		return Job{}, err
	} else if media != nil {
		if err := syncMetadataDirectory(filepath.Join(stage, "media")); err != nil {
			_ = os.RemoveAll(stage)
			return Job{}, err
		}
	}
	if err := syncMetadataDirectory(stage); err != nil {
		_ = os.RemoveAll(stage)
		return Job{}, err
	}
	if err := syncMetadataDirectory(filepath.Dir(stage)); err != nil {
		_ = os.RemoveAll(stage)
		return Job{}, err
	}
	next := restoredArchiveJob(old, man)
	afterJobs := make(map[string]Job, len(current))
	for key, value := range current {
		afterJobs[key] = value
	}
	afterJobs[id] = next
	after, err := json.MarshalIndent(afterJobs, "", "  ")
	if err != nil {
		_ = os.RemoveAll(stage)
		return Job{}, err
	}
	journal.AfterHash = metadataDigest(after)
	// An error after an atomic metadata rename may still mean the restore
	// committed. Classify from disk generation, not a returned fsync error.
	abort := func(cause error) (Job, error) {
		if recoveryErr := recoverArchiveRestoreLocked(s.root); recoveryErr != nil {
			s.primaryFatal = fmt.Errorf("archive revision restore recovery required: %w; original error: %v", recoveryErr, cause)
			return Job{}, s.primaryFatal
		}
		actual, readErr := os.ReadFile(filepath.Join(s.root, "jobs.json"))
		if readErr != nil {
			s.primaryFatal = fmt.Errorf("archive revision restore cannot classify disk generation: %w", readErr)
			return Job{}, s.primaryFatal
		}
		switch metadataDigest(actual) {
		case journal.BeforeHash:
			*j = old
			return Job{}, cause
		case journal.AfterHash:
			*j = next
			s.notifySQLiteMedia()
			return next, nil
		default:
			s.primaryFatal = errors.New("archive revision restore ended with unknown metadata generation")
			return Job{}, s.primaryFatal
		}
	}
	if err := writeRevisionRestoreJournal(s.root, journal); err != nil {
		// writeSyncedMetadata may have published the journal before returning
		// fsync failure; recover it rather than unconditionally discarding it.
		if info, statErr := os.Lstat(filepath.Join(s.root, revisionRestoreJournalName)); statErr == nil && info.Mode().IsRegular() {
			return abort(err)
		}
		_ = os.RemoveAll(stage)
		return Job{}, err
	}
	if s.restoreCheckpoint != nil {
		if err := s.restoreCheckpoint("journal"); err != nil {
			return abort(err)
		}
	}
	if err := durableRevisionRestoreRename(s.root, live, backup); err != nil {
		return abort(err)
	}
	if s.restoreCheckpoint != nil {
		if err := s.restoreCheckpoint("old-moved"); err != nil {
			return abort(err)
		}
	}
	if err := durableRevisionRestoreRename(s.root, stage, live); err != nil {
		return abort(err)
	}
	if s.restoreCheckpoint != nil {
		if err := s.restoreCheckpoint("new-moved"); err != nil {
			return abort(err)
		}
	}
	*j = next
	if err := s.saveJobsLocked(); err != nil {
		return abort(err)
	}
	if s.restoreCheckpoint != nil {
		if err := s.restoreCheckpoint("metadata"); err != nil {
			return abort(err)
		}
	}
	if err := recoverArchiveRestoreLocked(s.root); err != nil {
		s.primaryFatal = fmt.Errorf("restored archive committed, cleanup pending until startup recovery: %w", err)
		return Job{}, s.primaryFatal
	}
	_ = s.pruneArchiveRevisions(next)
	s.notifySQLiteMedia()
	return next, nil
}

func revisionRestoreManifestDigest(dir, author, post string) (string, error) {
	data, man, err := readArchiveManifestNoFollow(dir)
	if err != nil {
		return "", fmt.Errorf("manifest of version restore is missing or unsafe: %w", err)
	}
	if man.Author != author || man.PostID != post {
		return "", errors.New("manifest of restored version belongs to another post")
	}
	return metadataDigest(data), nil
}

func restoredArchiveJob(current Job, man ArchiveManifest) Job {
	current.Title = man.Title
	current.ArchiveUpdatedAt = man.CapturedAt
	current.Subscription = man.Subscription
	current.RequiredTier = man.RequiredTier
	current.DoneFiles = len(man.Assets)
	current.TotalFiles = len(man.Assets)
	current.CommentsPartial = len(man.Failures) > 0 || man.CommentsStatus == "частично"
	current.State = "done"
	if current.CommentsPartial {
		current.State = "partial"
	}
	current.Error = ""
	current.AutoSync = false
	current.SharedWith = nil
	current.SharedForAll = false
	current.UpdatedAt = time.Now().UTC()
	return current
}

func (a *App) listJobRevisions(w http.ResponseWriter, r *http.Request) {
	job, ok := a.ownedJob(r)
	if !ok {
		fail(w, 404, "архив не найден")
		return
	}
	unlock, ready := a.archiveGates.tryRead(job)
	if !ready {
		fail(w, http.StatusConflict, "архив обновляется или восстанавливается")
		return
	}
	defer unlock()
	job, ok = a.ownedJob(r)
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
	current, ok := a.ownedJob(r)
	if !ok {
		fail(w, 409, "архив не найден")
		return
	}
	unlock, ready := a.archiveGates.tryWrite(current)
	if !ready {
		fail(w, http.StatusConflict, "архив используется: завершите скачивание или обновление")
		return
	}
	defer unlock()
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
	unlock, ready := a.archiveGates.tryRead(job)
	if !ready {
		fail(w, http.StatusConflict, "архив обновляется или восстанавливается")
		return
	}
	defer unlock()
	job, ok = a.ownedJob(r)
	if !ok || (job.State != "done" && job.State != "partial") {
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
