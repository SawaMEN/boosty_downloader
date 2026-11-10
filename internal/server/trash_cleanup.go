package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	trashCleanupInterval = time.Hour
	trashSweepBatch      = 20
	trashAuditCapacity   = 200
)

// TrashPolicy is intentionally opt-in. Disabling automatic cleanup never
// prevents a user from manually emptying an individual archive.
type TrashPolicy struct {
	AutoPurge     bool `json:"autoPurge"`
	RetentionDays int  `json:"retentionDays"`
}

func defaultTrashPolicy() TrashPolicy {
	return TrashPolicy{RetentionDays: 30}
}

func validateTrashPolicy(p TrashPolicy) error {
	if p.RetentionDays < 1 || p.RetentionDays > 3650 {
		return errors.New("срок хранения в корзине должен быть от 1 до 3650 дней")
	}
	return nil
}

type TrashAuditEntry struct {
	At      time.Time `json:"at"`
	Trigger string    `json:"trigger"`
	OwnerID string    `json:"ownerId,omitempty"`
	JobID   string    `json:"jobId,omitempty"`
	Removed int       `json:"removed,omitempty"`
	Failed  int       `json:"failed,omitempty"`
	Error   string    `json:"error,omitempty"`
}

type trashCandidate struct {
	owner string
	id    string
}

func (s *Storage) expiredTrash(cutoff time.Time, limit int) ([]trashCandidate, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type item struct {
		key   trashCandidate
		since time.Time
	}
	items := make([]item, 0)
	for _, job := range s.jobs {
		if job.DeletedAt != nil && job.DeletedAt.Before(cutoff) {
			items = append(items, item{
				key:   trashCandidate{owner: job.OwnerID, id: job.ID},
				since: *job.DeletedAt,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].since.Equal(items[j].since) {
			return items[i].key.id < items[j].key.id
		}
		return items[i].since.Before(items[j].since)
	})
	total := len(items)
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	out := make([]trashCandidate, 0, len(items))
	for _, item := range items {
		out = append(out, item.key)
	}
	return out, total
}

func (s *Storage) readTrashAudit() ([]TrashAuditEntry, error) {
	raw, err := os.ReadFile(filepath.Join(s.root, "trash-cleanup-log.json"))
	if os.IsNotExist(err) {
		return []TrashAuditEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var log []TrashAuditEntry
	if err := json.Unmarshal(raw, &log); err != nil {
		return nil, err
	}
	if log == nil {
		log = []TrashAuditEntry{}
	}
	return log, nil
}

func (s *Storage) appendTrashAudit(event TrashAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readTrashAudit()
	if err != nil {
		return err
	}
	entries = append(entries, event)
	if len(entries) > trashAuditCapacity {
		entries = entries[len(entries)-trashAuditCapacity:]
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.root, "trash-cleanup-log.json"), raw)
}

// A dedicated worker handles scheduled and administrator-triggered sweeps.
// There is never more than one sweep in progress in a running instance.
func (a *App) trashCleanupLoop() {
	ticker := time.NewTicker(trashCleanupInterval)
	defer ticker.Stop()
	if a.accounts.policy().Trash.AutoPurge {
		a.runTrashSweep("automatic")
	}
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if a.accounts.policy().Trash.AutoPurge {
				a.runTrashSweep("automatic")
			}
		case <-a.trashQueue:
			a.runTrashSweep("manual")
			a.trashMu.Lock()
			a.trashSweepQueued = false
			a.trashMu.Unlock()
		}
	}
}

func (a *App) queueTrashCleanup() bool {
	a.trashMu.Lock()
	defer a.trashMu.Unlock()
	if a.trashSweepQueued {
		return false
	}
	select {
	case <-a.ctx.Done():
		return false
	case a.trashQueue <- struct{}{}:
		a.trashSweepQueued = true
		return true
	default:
		return false
	}
}

func (a *App) runTrashSweep(trigger string) {
	a.trashMu.Lock()
	a.trashSweepRunning = true
	a.trashMu.Unlock()
	defer func() {
		a.trashMu.Lock()
		a.trashSweepRunning = false
		a.trashMu.Unlock()
	}()

	policy := a.accounts.policy().Trash
	cutoff := time.Now().UTC().Add(-time.Duration(policy.RetentionDays) * 24 * time.Hour)
	candidates, total := a.store.expiredTrash(cutoff, trashSweepBatch)
	removed, failed := 0, 0
	for _, job := range candidates {
		if a.ctx.Err() != nil {
			break
		}
		if trigger == "automatic" && !a.accounts.policy().Trash.AutoPurge {
			break
		}
		n, err := a.store.purgeOwnedBefore(job.owner, job.id, "", cutoff)
		// The owner may have restored or already emptied this record.
		if n == 0 && err != nil && err.Error() == "архив не найден" {
			continue
		}
		removed += n
		if err != nil {
			failed++
		}
		item := TrashAuditEntry{
			At: time.Now().UTC(), Trigger: trigger,
			OwnerID: job.owner, JobID: job.id, Removed: n,
		}
		if err != nil {
			item.Failed = 1
			item.Error = err.Error()
		}
		_ = a.store.appendTrashAudit(item)
	}
	if total > 0 || trigger == "manual" {
		summary := TrashAuditEntry{
			At: time.Now().UTC(), Trigger: trigger + "_summary",
			Removed: removed, Failed: failed,
		}
		_ = a.store.appendTrashAudit(summary)
	}
}

func (a *App) adminTrashStatus(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, http.StatusForbidden, "только администратор")
		return
	}
	policy := a.accounts.policy().Trash
	cutoff := time.Now().UTC().Add(-time.Duration(policy.RetentionDays) * 24 * time.Hour)
	_, expired := a.store.expiredTrash(cutoff, 0)
	history, err := a.store.readTrashAudit()
	if err != nil {
		fail(w, 500, "журнал корзины недоступен")
		return
	}
	a.trashMu.Lock()
	running, queued := a.trashSweepRunning, a.trashSweepQueued
	a.trashMu.Unlock()
	// The history is in chronological order on disk, newest first in UI.
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}
	if len(history) > 50 {
		history = history[:50]
	}
	reply(w, 200, map[string]any{
		"policy": policy, "expired": expired, "running": running,
		"queued": queued, "history": history,
	})
}

func (a *App) adminTrashCleanup(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, http.StatusForbidden, "только администратор")
		return
	}
	if !a.queueTrashCleanup() {
		fail(w, http.StatusConflict, "очистка уже запланирована или сервис остановлен")
		return
	}
	reply(w, http.StatusAccepted, map[string]bool{"queued": true})
}
