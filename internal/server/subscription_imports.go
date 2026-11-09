package server

import (
	"context"
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

// AuthorImport records discovery progress, not media download completion.
// Each discovered accessible post gets its own persistent Job and archive.
type AuthorImport struct {
	ID              string    `json:"id"`
	OwnerID         string    `json:"ownerId"`
	Author          string    `json:"author"`
	Title           string    `json:"title"`
	State           string    `json:"state"` // queued, running, done, failed
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	NextOffset      string    `json:"nextOffset,omitempty"`
	Scanned         int       `json:"scanned"`
	Queued          int       `json:"queued"`
	Skipped         int       `json:"skipped"`
	MaxPosts        int       `json:"maxPosts"` // 0 = all, with a hard cap of 2000.
	Limited         bool      `json:"limited,omitempty"`
	IncludeComments bool      `json:"includeComments"`
	IncludeFiles    bool      `json:"includeFiles"`
	IncludeVideos   bool      `json:"includeVideos"`
	IncludeAudio    bool      `json:"includeAudio"`
	AutoSync        bool      `json:"autoSync"`
	Incremental     bool      `json:"incremental,omitempty"` // Scheduled catch-up stops at the first previously discovered post.
}

const importHardLimit = 2000

var errImportCanceled = errors.New("импорт остановлен пользователем")

func (a *App) lookupSubscriptions(ctx context.Context, owner string) (boosty.SubscriptionList, error) {
	if a.subscriptionSource != nil {
		return a.subscriptionSource(ctx, owner)
	}
	client, err := a.client(owner)
	if err != nil {
		return boosty.SubscriptionList{}, err
	}
	return client.Subscriptions(ctx)
}

func (a *App) subscribedAuthors(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	result, err := a.lookupSubscriptions(ctx, principal(r).ID)
	if err != nil {
		fail(w, 502, "не удалось получить подписки Boosty: "+err.Error())
		return
	}
	reply(w, 200, result)
}

func (a *App) importsForUser(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, a.store.importsFor(principal(r).ID))
}

func (a *App) importSubscriptions(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Authors         []string `json:"authors"`
		MaxPosts        int      `json:"maxPosts"`
		IncludeComments bool     `json:"includeComments"`
		IncludeFiles    bool     `json:"includeFiles"`
		IncludeVideos   bool     `json:"includeVideos"`
		IncludeAudio    bool     `json:"includeAudio"`
		AutoSync        bool     `json:"autoSync"`
	}
	if decodeRequest(w, r, &input) != nil {
		return
	}
	if len(input.Authors) < 1 || len(input.Authors) > 20 || input.MaxPosts < 0 || input.MaxPosts > importHardLimit {
		fail(w, 400, "выберите от 1 до 20 авторов; лимит постов: от 0 до 2000")
		return
	}
	selected := make(map[string]bool, len(input.Authors))
	for _, author := range input.Authors {
		if !boosty.ValidBlog(author) || !safeArchivePart(author) || selected[author] {
			fail(w, 400, "некорректный или повторяющийся автор")
			return
		}
		selected[author] = true
	}
	// Validate against actual subscriptions. A client cannot ask the server to
	// import an arbitrary blog via this privileged onboarding workflow.
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	listing, err := a.lookupSubscriptions(ctx, principal(r).ID)
	if err != nil {
		fail(w, 502, "не удалось проверить подписки: "+err.Error())
		return
	}
	known := map[string]string{}
	for _, author := range listing.Authors {
		known[author.Blog] = author.Title
	}
	for author := range selected {
		if _, ok := known[author]; !ok {
			fail(w, 403, "выбранный автор отсутствует в активных подписках Boosty")
			return
		}
	}
	// Refuse new batches before storing them when the discovery queue is full.
	if len(a.importQueue)+len(input.Authors) > cap(a.importQueue) {
		fail(w, 429, "слишком много активных импортов")
		return
	}
	now := time.Now().UTC()
	imports := make([]AuthorImport, 0, len(input.Authors))
	for _, author := range input.Authors {
		b := make([]byte, 12)
		if _, err = rand.Read(b); err != nil {
			fail(w, 500, "ошибка создания импорта")
			return
		}
		imports = append(imports, AuthorImport{ID: hex.EncodeToString(b), OwnerID: principal(r).ID, Author: author, Title: known[author], State: "queued", CreatedAt: now, UpdatedAt: now, MaxPosts: input.MaxPosts, IncludeComments: input.IncludeComments, IncludeFiles: input.IncludeFiles, IncludeVideos: input.IncludeVideos, IncludeAudio: input.IncludeAudio, AutoSync: input.AutoSync})
	}
	started, err := a.store.createImports(imports)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	for _, item := range started {
		a.importQueue <- item.ID
	}
	reply(w, http.StatusAccepted, map[string]any{"imports": started, "alreadyRunning": len(imports) - len(started)})
}

func (s *Storage) loadImports() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(s.root, "imports.json"))
	if err == nil {
		if err = jsonUnmarshalImports(raw, &s.imports); err != nil {
			return fmt.Errorf("повреждена база импорта: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if s.imports == nil {
		s.imports = map[string]*AuthorImport{}
	}
	for _, v := range s.imports {
		if v.State == "running" {
			v.State = "queued"
		}
	}
	return s.saveImportsLocked()
}

// Kept separate from JSON encoding in handlers to allow migration later.
func jsonUnmarshalImports(raw []byte, into *map[string]*AuthorImport) error {
	return json.Unmarshal(raw, into)
}

func (s *Storage) saveImportsLocked() error {
	raw, err := json.MarshalIndent(s.imports, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.root, "imports.json"), raw)
}
func (s *Storage) importsFor(owner string) []AuthorImport {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuthorImport, 0)
	for _, v := range s.imports {
		if v.OwnerID == owner {
			out = append(out, *v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}
func (s *Storage) queuedImports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, v := range s.imports {
		if v.State == "queued" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
func (s *Storage) createImports(imports []AuthorImport) ([]AuthorImport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ongoing := map[string]bool{}
	for _, v := range s.imports {
		if v.State == "queued" || v.State == "running" {
			ongoing[v.OwnerID+"/"+v.Author] = true
		}
	}
	var added []AuthorImport
	for _, v := range imports {
		if ongoing[v.OwnerID+"/"+v.Author] {
			continue
		}
		ongoing[v.OwnerID+"/"+v.Author] = true
		item := v
		s.imports[v.ID] = &item
		added = append(added, v)
	}
	if len(added) == 0 {
		return []AuthorImport{}, nil
	}
	if err := s.saveImportsLocked(); err != nil {
		for _, v := range added {
			delete(s.imports, v.ID)
		}
		return nil, err
	}
	return added, nil
}
func (s *Storage) cancelImport(owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.imports[id]
	if !ok || v.OwnerID != owner {
		return errors.New("импорт не найден")
	}
	if v.State != "queued" && v.State != "running" {
		return errors.New("импорт уже завершён")
	}
	old := *v
	v.State = "canceled"
	v.UpdatedAt = time.Now().UTC()
	if err := s.saveImportsLocked(); err != nil {
		*v = old
		return err
	}
	return nil
}
func (s *Storage) importCanceled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.imports[id]
	return v == nil || v.State == "canceled"
}

// importSchedule lets the archive owner enable or disable future author scans.
// Existing archived post refresh follows the same flag, so one toggle controls
// both discovery and updates for that author.
func (a *App) importSchedule(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AutoSync bool `json:"autoSync"`
	}
	if decodeRequest(w, r, &input) != nil {
		return
	}
	if err := a.store.setImportSchedule(principal(r).ID, r.PathValue("id"), input.AutoSync); err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, map[string]bool{"ok": true, "autoSync": input.AutoSync})
}

func (s *Storage) setImportSchedule(owner, id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.imports[id]
	if v == nil || v.OwnerID != owner {
		return errors.New("импорт не найден")
	}
	if v.State != "done" {
		return errors.New("изменить расписание можно после завершения поиска")
	}
	old := v.AutoSync
	v.AutoSync = enabled
	previous := make(map[string]bool)
	for jobID, j := range s.jobs {
		if j.OwnerID == owner && strings.EqualFold(j.Author, v.Author) {
			previous[jobID] = j.AutoSync
			j.AutoSync = enabled
		}
	}
	rollback := func() {
		v.AutoSync = old
		for jobID, enabled := range previous {
			s.jobs[jobID].AutoSync = enabled
		}
		_ = s.saveJobsLocked()
		_ = s.saveImportsLocked()
	}
	if err := s.saveJobsLocked(); err != nil {
		rollback()
		return err
	}
	if err := s.saveImportsLocked(); err != nil {
		rollback()
		return err
	}
	return nil
}

// scheduleDueImports reuses the latest import per author instead of creating
// unbounded history entries every polling interval. The first catch-up pass
// starts from the newest posts and stops on an already known post.
func (s *Storage) scheduleDueImports(now time.Time, interval time.Duration, limit int) ([]string, error) {
	if interval <= 0 || limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := map[string]*AuthorImport{}
	busy := map[string]bool{}
	for _, v := range s.imports {
		key := v.OwnerID + "/" + strings.ToLower(v.Author)
		if old := latest[key]; old == nil || old.CreatedAt.Before(v.CreatedAt) || (old.CreatedAt.Equal(v.CreatedAt) && old.ID < v.ID) {
			latest[key] = v
		}
		if v.State == "queued" || v.State == "running" {
			busy[key] = true
		}
	}
	candidates := make([]*AuthorImport, 0, len(latest))
	for key, v := range latest {
		if !busy[key] && v.State == "done" && v.AutoSync && !v.UpdatedAt.IsZero() && now.Sub(v.UpdatedAt) >= interval {
			candidates = append(candidates, v)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt) })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	backups := make([]AuthorImport, len(candidates))
	ids := make([]string, 0, len(candidates))
	for i, v := range candidates {
		backups[i] = *v
		v.State = "queued"
		v.Error = ""
		v.NextOffset = ""
		v.Scanned, v.Queued, v.Skipped = 0, 0, 0
		v.Limited = false
		v.Incremental = true
		v.UpdatedAt = now
		if v.MaxPosts == 0 || v.MaxPosts > 50 {
			v.MaxPosts = 50
		}
		ids = append(ids, v.ID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err := s.saveImportsLocked(); err != nil {
		for i, v := range candidates {
			*v = backups[i]
		}
		return nil, err
	}
	return ids, nil
}

// The interval is deliberately the same as archive sync; no extra setting is
// necessary for initial deployment. Queue space is bounded by the existing
// worker channel and the scheduler chooses the oldest due authors first.
func (a *App) importSyncLoop() {
	if a.cfg.SyncInterval <= 0 {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	a.scheduleAutoImports(time.Now().UTC())
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.scheduleAutoImports(time.Now().UTC())
		}
	}
}
func (a *App) scheduleAutoImports(now time.Time) {
	slots := cap(a.importQueue) - len(a.importQueue)
	if slots < 1 {
		return
	}
	if slots > 10 {
		slots = 10
	}
	ids, err := a.store.scheduleDueImports(now, a.cfg.SyncInterval, slots)
	if err != nil {
		return
	} // Next tick will retry, keeping the existing import intact.
	for _, id := range ids {
		select {
		case <-a.ctx.Done():
			return
		case a.importQueue <- id:
		}
	}
}

func (a *App) cancelImport(w http.ResponseWriter, r *http.Request) {
	if err := a.store.cancelImport(principal(r).ID, r.PathValue("id")); err != nil {
		fail(w, 409, err.Error())
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (s *Storage) updateImport(v AuthorImport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.imports[v.ID]; !ok {
		return errors.New("импорт не найден")
	} else if old.State == "canceled" {
		return errImportCanceled
	}
	v.UpdatedAt = time.Now().UTC()
	s.imports[v.ID] = &v
	return s.saveImportsLocked()
}
func (s *Storage) claimImport(id string) (AuthorImport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.imports[id]
	if !ok || v.State != "queued" {
		return AuthorImport{}, false
	}
	old := *v
	v.State = "running"
	v.UpdatedAt = time.Now().UTC()
	if s.saveImportsLocked() != nil {
		*v = old
		return AuthorImport{}, false
	}
	return *v, true
}
func (a *App) importWorker() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case id := <-a.importQueue:
			v, ok := a.store.claimImport(id)
			if !ok {
				continue
			}
			if err := a.runImport(&v); err != nil {
				if errors.Is(err, errImportCanceled) {
					continue
				}
				v.State = "failed"
				v.Error = err.Error()
			} else {
				v.State = "done"
				v.Error = ""
			}
			if a.ctx.Err() != nil {
				v.State = "queued"
				v.Error = ""
			}
			_ = a.store.updateImport(v)
		}
	}
}
func (a *App) runImport(v *AuthorImport) error {
	c, err := a.client(v.OwnerID)
	if err != nil {
		return err
	}
	if !c.HasToken() && a.feedSource == nil {
		return errors.New("аккаунт Boosty отключён")
	}

	seenOffsets := map[string]bool{}
	if v.NextOffset != "" {
		seenOffsets[v.NextOffset] = true
	}
	for v.Scanned < importHardLimit {
		if a.ctx.Err() != nil {
			return a.ctx.Err()
		}
		if a.store.importCanceled(v.ID) {
			return errImportCanceled
		}
		if !c.HasToken() && a.feedSource == nil {
			return errors.New("аккаунт Boosty отключён")
		}
		if v.MaxPosts > 0 && v.Scanned >= v.MaxPosts {
			v.Limited = true
			return nil
		}
		ctx, cancel := context.WithTimeout(a.ctx, 50*time.Second)
		var feed boosty.FeedResult
		if a.feedSource != nil {
			feed, err = a.feedSource(ctx, v.OwnerID, v.Author, v.NextOffset)
		} else {
			feed, err = c.Feed(ctx, v.Author, v.NextOffset)
		}
		cancel()
		if err != nil {
			return err
		}
		if len(feed.Posts) == 0 {
			return nil
		}
		for _, p := range feed.Posts {
			if a.store.importCanceled(v.ID) {
				return errImportCanceled
			}
			if v.Scanned >= importHardLimit || v.MaxPosts > 0 && v.Scanned >= v.MaxPosts {
				v.Limited = true
				break
			}
			v.Scanned++
			if !p.HasAccess {
				v.Skipped++
				continue
			}
			b := make([]byte, 12)
			if _, err = rand.Read(b); err != nil {
				return err
			}
			j := Job{ID: hex.EncodeToString(b), OwnerID: v.OwnerID, Author: v.Author, PostID: p.ID, URL: boosty.PostURLFor(v.Author, p.ID), Title: p.Title, State: "queued", CreatedAt: time.Now().UTC(), IncludeComments: v.IncludeComments, IncludeFiles: v.IncludeFiles, IncludeVideos: v.IncludeVideos, IncludeAudio: v.IncludeAudio, AutoSync: v.AutoSync}
			job, created, e := a.store.createUniqueJob(j)
			if e != nil {
				return e
			}
			if created {
				v.Queued++
				if !a.queueJob(job.ID) {
					return a.ctx.Err()
				}
			} else {
				v.Skipped++
				if v.Incremental {
					// Existing newest-to-oldest posts mark the catch-up boundary.
					// Later manual full reimport can fill any older gaps.
					return a.store.updateImport(*v)
				}
			}
		}
		if err := a.store.updateImport(*v); err != nil {
			return err
		}
		if v.Limited {
			return nil
		}
		if !feed.HasMore || feed.NextOffset == "" {
			return nil
		}
		if len(feed.NextOffset) > 200 || seenOffsets[feed.NextOffset] {
			return errors.New("Boosty возвратил повторяющийся offset списка постов")
		}
		seenOffsets[feed.NextOffset] = true
		v.NextOffset = feed.NextOffset
		if err := a.store.updateImport(*v); err != nil {
			return err
		}
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	v.Limited = true
	return nil
}
