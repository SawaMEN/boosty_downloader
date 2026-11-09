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
	"sync"
	"time"
)

type Storage struct {
	mu   sync.Mutex
	root string
	key  []byte
	jobs map[string]*Job
}
type Job struct {
	ID              string    `json:"id"`
	URL             string    `json:"url"`
	Author          string    `json:"author"`
	PostID          string    `json:"postId"`
	Title           string    `json:"title"`
	State           string    `json:"state"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	IncludeComments bool      `json:"includeComments"`
	IncludeFiles    bool      `json:"includeFiles"`
	IncludeVideos   bool      `json:"includeVideos"`
	IncludeAudio    bool      `json:"includeAudio"`
	DoneFiles       int       `json:"doneFiles"`
	TotalFiles      int       `json:"totalFiles"`
	CommentsPartial bool      `json:"commentsPartial"`
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
	s := &Storage{root: root, key: key, jobs: make(map[string]*Job)}
	raw, e := os.ReadFile(filepath.Join(root, "jobs.json"))
	if e == nil {
		if e = json.Unmarshal(raw, &s.jobs); e != nil {
			return nil, fmt.Errorf("повреждена база задач: %w", e)
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	for _, j := range s.jobs {
		if j.State == "running" {
			j.State = "queued"
			j.Error = ""
		}
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
	if !ok {
		return Job{}, false
	}
	return *j, true
}
func (s *Storage) all() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	return out
}
func (s *Storage) queued() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0)
	for id, j := range s.jobs {
		if j.State == "queued" {
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
func (s *Storage) archiveDir(author, id string) string {
	return filepath.Join(s.root, "library", author, id)
}
