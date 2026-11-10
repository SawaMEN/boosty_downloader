package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SawaMEN/boosty_downloader/internal/boosty"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Storage struct {
	mu      sync.Mutex
	blobMu  sync.Mutex
	root    string
	key     []byte
	jobs    map[string]*Job
	authors map[string][]string
	imports map[string]*AuthorImport
}
// CapturedSubscription is a snapshot of the owner's Boosty subscription
// when their account was checked. It is NOT a claim about the post's minimum
// required tier, and never grants access to another owner's archive.
type CapturedSubscription struct {
	Level      string    `json:"level,omitempty"`
	Paid       bool      `json:"paid"`
	Paused     bool      `json:"paused,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
	Source     string    `json:"source"`
}

func captureSubscription(author boosty.SubscriptionAuthor, at time.Time) *CapturedSubscription {
	return &CapturedSubscription{
		Level: author.Level, Paid: author.Paid, Paused: author.Paused,
		ObservedAt: at.UTC(), Source: "boosty_subscriptions",
	}
}

type Job struct {
	ID                string                `json:"id"`
	URL               string                `json:"url"`
	Author            string                `json:"author"`
	PostID            string                `json:"postId"`
	Title             string                `json:"title"`
	State             string                `json:"state"`
	Error             string                `json:"error,omitempty"`
	CreatedAt         time.Time             `json:"createdAt"`
	UpdatedAt         time.Time             `json:"updatedAt"`
	ArchiveUpdatedAt  time.Time             `json:"archiveUpdatedAt,omitempty"`
	IncludeComments   bool                  `json:"includeComments"`
	IncludeFiles      bool                  `json:"includeFiles"`
	IncludeVideos     bool                  `json:"includeVideos"`
	IncludeAudio      bool                  `json:"includeAudio"`
	AutoSync          bool                  `json:"autoSync"`
	DoneFiles         int                   `json:"doneFiles"`
	TotalFiles        int                   `json:"totalFiles"`
	CommentsPartial   bool                  `json:"commentsPartial"`
	OwnerID           string                `json:"ownerId"`
	SharedWith        []string              `json:"sharedWith,omitempty"`
	SharedForAll      bool                  `json:"sharedForAll,omitempty"`
	DeletedAt         *time.Time            `json:"deletedAt,omitempty"`
	Subscription      *CapturedSubscription `json:"subscription,omitempty"`
}

func newStorage(root, keyString string) (*Storage, error) {
	key, e := base64.StdEncoding.DecodeString(keyString)
	if e != nil || len(key) != 32 {
		return nil, errors.New("BD_SECRET_KEY должен содержать 32 байта в base64")
	}
	if e = os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	if e = os.Chmod(root, 0700); e != nil {
		return nil, e
	}
	s := &Storage{root: root, key: key, jobs: make(map[string]*Job), authors: make(map[string][]string), imports: make(map[string]*AuthorImport)}
	raw, e := os.ReadFile(filepath.Join(root, "jobs.json"))
	if e == nil {
		if e = json.Unmarshal(raw, &s.jobs); e != nil {
			return nil, fmt.Errorf("повреждена база задач: %w", e)
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	rawAuthors, e := os.ReadFile(filepath.Join(root, "authors.json"))
	if e == nil {
		if e = json.Unmarshal(rawAuthors, &s.authors); e != nil {
			return nil, fmt.Errorf("повреждён каталог авторов: %w", e)
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if s.authors == nil {
		s.authors = map[string][]string{}
	}
	for _, j := range s.jobs {
		if j.State == "running" {
			j.State = "queued"
			j.Error = ""
		}
	}
	if e = s.loadImports(); e != nil {
		return nil, e
	}
	return s, nil
}
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, e := os.CreateTemp(dir, ".write-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(name, path)
}
func (s *Storage) saveJobsLocked() error {
	b, e := json.MarshalIndent(s.jobs, "", "  ")
	if e != nil {
		return e
	}
	return writeAtomic(filepath.Join(s.root, "jobs.json"), b)
}
func (s *Storage) upsert(job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.UpdatedAt = time.Now().UTC()
	s.jobs[job.ID] = &job
	return s.saveJobsLocked()
}
func (s *Storage) get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.DeletedAt != nil {
		return Job{}, false
	}
	return *j, true
}
func (s *Storage) all() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.DeletedAt == nil {
			out = append(out, *j)
		}
	}
	return out
}
func (s *Storage) queued() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0)
	for id, j := range s.jobs {
		if j.DeletedAt == nil && j.State == "queued" {
			out = append(out, id)
		}
	}
	return out
}
func (s *Storage) saveTokens(t boosty.Tokens) error {
	b, e := json.Marshal(t)
	if e != nil {
		return e
	}
	block, e := aes.NewCipher(s.key)
	if e != nil {
		return e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return e
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	out := gcm.Seal(nonce, nonce, b, nil)
	return writeAtomic(filepath.Join(s.root, "boosty-auth.enc"), out)
}
func (s *Storage) loadTokens() (boosty.Tokens, error) {
	b, e := os.ReadFile(filepath.Join(s.root, "boosty-auth.enc"))
	if os.IsNotExist(e) {
		return boosty.Tokens{}, nil
	}
	if e != nil {
		return boosty.Tokens{}, e
	}
	block, e := aes.NewCipher(s.key)
	if e != nil {
		return boosty.Tokens{}, e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return boosty.Tokens{}, e
	}
	if len(b) < gcm.NonceSize() {
		return boosty.Tokens{}, errors.New("неверный формат хранилища авторизации")
	}
	pt, e := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], nil)
	if e != nil {
		return boosty.Tokens{}, e
	}
	var t boosty.Tokens
	return t, json.Unmarshal(pt, &t)
}
func (s *Storage) archiveDir(owner, author, id string) string {
	return filepath.Join(s.root, "library", owner, author, id)
}

func (s *Storage) migrateLegacyJobs() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.OwnerID != "" {
			continue
		}
		if !safeArchivePart(j.Author) || !safeArchivePart(j.PostID) {
			return errors.New("неверный путь старого архива")
		}
		old := filepath.Join(s.root, "library", j.Author, j.PostID)
		dst := s.archiveDir("admin", j.Author, j.PostID)
		if _, e := os.Stat(old); e == nil {
			if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
				return e
			}
			if _, e = os.Stat(dst); os.IsNotExist(e) {
				if e = os.Rename(old, dst); e != nil {
					return e
				}
			}
		}
		j.OwnerID = "admin"
	}
	return s.saveJobsLocked()
}
func safeArchivePart(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func (s *Storage) saveUserTokens(owner string, t boosty.Tokens) error {
	if !safeArchivePart(owner) {
		return errors.New("неверный ID пользователя")
	}
	b, e := json.Marshal(t)
	if e != nil {
		return e
	}
	block, e := aes.NewCipher(s.key)
	if e != nil {
		return e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return e
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Join(s.root, "vault"), 0700); e != nil {
		return e
	}
	return writeAtomic(filepath.Join(s.root, "vault", owner+".enc"), gcm.Seal(nonce, nonce, b, []byte(owner)))
}
func (s *Storage) loadUserTokens(owner string) (boosty.Tokens, error) {
	if !safeArchivePart(owner) {
		return boosty.Tokens{}, errors.New("неверный ID пользователя")
	}
	b, e := os.ReadFile(filepath.Join(s.root, "vault", owner+".enc"))
	if os.IsNotExist(e) {
		return boosty.Tokens{}, nil
	}
	if e != nil {
		return boosty.Tokens{}, e
	}
	block, e := aes.NewCipher(s.key)
	if e != nil {
		return boosty.Tokens{}, e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return boosty.Tokens{}, e
	}
	if len(b) < gcm.NonceSize() {
		return boosty.Tokens{}, errors.New("повреждены учётные данные")
	}
	pt, e := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte(owner))
	if e != nil {
		return boosty.Tokens{}, e
	}
	var t boosty.Tokens
	e = json.Unmarshal(pt, &t)
	return t, e
}
func (s *Storage) migrateLegacyAuth() error {
	old := filepath.Join(s.root, "boosty-auth.enc")
	if _, e := os.Stat(old); os.IsNotExist(e) {
		return nil
	}
	if _, e := os.Stat(filepath.Join(s.root, "vault", "admin.enc")); e == nil {
		return nil
	}
	t, e := s.loadTokens()
	if e != nil {
		return e
	}
	if e = s.saveUserTokens("admin", t); e != nil {
		return e
	}
	return os.Remove(old)
}

// migrateArchiveTimestamps backfills the last completed snapshot date, not
// the timestamp of unrelated job-status or access-control changes.
func (s *Storage) migrateArchiveTimestamps() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, j := range s.jobs {
		if !j.ArchiveUpdatedAt.IsZero() || (j.State != "done" && j.State != "partial") {
			continue
		}
		if !safeArchivePart(j.OwnerID) || !safeArchivePart(j.Author) || !safeArchivePart(j.PostID) {
			continue
		}
		base := s.archiveDir(j.OwnerID, j.Author, j.PostID)
		var man struct {
			CapturedAt time.Time `json:"capturedAt"`
		}
		if data, err := os.ReadFile(filepath.Join(base, "manifest.json")); err == nil {
			_ = json.Unmarshal(data, &man)
		}
		if !man.CapturedAt.IsZero() {
			j.ArchiveUpdatedAt = man.CapturedAt
		} else if stat, err := os.Stat(filepath.Join(base, "index.html")); err == nil {
			j.ArchiveUpdatedAt = stat.ModTime().UTC()
		} else {
			continue
		}
		changed = true
	}
	if changed {
		return s.saveJobsLocked()
	}
	return nil
}
