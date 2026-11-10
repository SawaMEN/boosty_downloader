package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PBKDF2-HMAC-SHA256 in the Go standard library, with per-user random salt.
// The stored format includes parameters for future migration to Argon2id.
const passwordIterations = 310000
const sessionTTL = 7 * 24 * time.Hour
const sessionCookie = "bd_session"

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,32}$`)

type Account struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Password  string    `json:"passwordHash"`
	Admin     bool      `json:"admin"`
	CreatedAt time.Time `json:"createdAt"`
}
type LoginSession struct {
	UserID  string    `json:"userId"`
	Expires time.Time `json:"expires"`
}
type accountFile struct {
	Accounts           map[string]Account         `json:"accounts"`
	Sessions           map[string]LoginSession    `json:"sessions"`
	RegistrationOpen   bool                       `json:"registrationOpen"`
	DefaultPermissions map[string]bool            `json:"defaultPermissions,omitempty"`
	UserPermissions    map[string]map[string]bool `json:"userPermissions,omitempty"`
	SyncIntervalHours  *int                       `json:"syncIntervalHours,omitempty"`
	MaxScanPosts       int                        `json:"maxScanPosts,omitempty"`
	MaxImportPosts     int                        `json:"maxImportPosts,omitempty"`
	CompressionLevel   *int                       `json:"compressionLevel,omitempty"`
	DeduplicateMedia   *bool                      `json:"deduplicateMedia,omitempty"`
	MinFreeGiB         *int                       `json:"minFreeGiB,omitempty"`
	MaxUserGiB         *int                       `json:"maxUserGiB,omitempty"`
	VideoPolicy        *VideoPolicy               `json:"videoPolicy,omitempty"`
	AutoUpdatePolicy   *AutoUpdatePolicy          `json:"autoUpdatePolicy,omitempty"`
	TrashPolicy        *TrashPolicy               `json:"trashPolicy,omitempty"`
}
type Accounts struct {
	mu     sync.Mutex
	path   string
	data   accountFile
	failed map[string][]time.Time
}

func passwordKey(pass string, salt []byte, count int) []byte {
	key := []byte(pass)
	var ctr [4]byte
	ctr[3] = 1
	h := hmac.New(sha256.New, key)
	h.Write(salt)
	h.Write(ctr[:])
	u := h.Sum(nil)
	out := append([]byte{}, u...)
	for i := 1; i < count; i++ {
		h = hmac.New(sha256.New, key)
		h.Write(u)
		u = h.Sum(u[:0])
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}
func hashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", errors.New("пароль должен содержать от 12 до 1024 байт")
	}
	salt := make([]byte, 24)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256:%d:%s:%s", passwordIterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(passwordKey(password, salt, passwordIterations))), nil
}
func verifyPassword(password, stored string) bool {
	p := strings.Split(stored, ":")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" || p[1] != "310000" {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(p[2])
	want, e2 := base64.RawStdEncoding.DecodeString(p[3])
	if e1 != nil || e2 != nil || len(salt) != 24 || len(want) != 32 {
		return false
	}
	got := passwordKey(password, salt, passwordIterations)
	return subtle.ConstantTimeCompare(got, want) == 1
}
func newAccounts(root, adminUser, adminPassword string) (*Accounts, error) {
	if !usernamePattern.MatchString(adminUser) {
		return nil, errors.New("недопустимый логин администратора")
	}
	a := &Accounts{path: filepath.Join(root, "accounts.json"), data: accountFile{Accounts: map[string]Account{}, Sessions: map[string]LoginSession{}}, failed: make(map[string][]time.Time)}
	raw, err := os.ReadFile(a.path)
	if err == nil {
		if err = json.Unmarshal(raw, &a.data); err != nil {
			return nil, fmt.Errorf("база пользователей повреждена: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if a.data.Accounts == nil {
		a.data.Accounts = map[string]Account{}
	}
	if a.data.Sessions == nil {
		a.data.Sessions = map[string]LoginSession{}
	}
	if len(a.data.Accounts) == 0 {
		hash, err := hashPassword(adminPassword)
		if err != nil {
			return nil, err
		}
		a.data.Accounts["admin"] = Account{ID: "admin", Username: adminUser, Password: hash, Admin: true, CreatedAt: time.Now().UTC()}
		if err = a.saveLocked(); err != nil {
			return nil, err
		}
	}
	return a, nil
}
func (a *Accounts) saveLocked() error {
	b, err := json.MarshalIndent(a.data, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(a.path, b)
}
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (a *Accounts) register(user, password string) (Account, error) {
	if !usernamePattern.MatchString(user) {
		return Account{}, errors.New("логин: 3–32 латинских символа, цифры, точка, дефис или подчёркивание")
	}
	if !a.registration() {
		return Account{}, errors.New("регистрация закрыта администратором")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return Account{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.data.RegistrationOpen {
		return Account{}, errors.New("регистрация пока закрыта администратором")
	}
	if len(a.data.Accounts) >= 1000 {
		return Account{}, errors.New("достигнут лимит пользователей")
	}
	for _, u := range a.data.Accounts {
		if strings.EqualFold(u.Username, user) {
			return Account{}, errors.New("логин уже занят")
		}
	}
	id, err := randomHex(16)
	if err != nil {
		return Account{}, err
	}
	acc := Account{ID: id, Username: user, Password: hash, CreatedAt: time.Now().UTC()}
	a.data.Accounts[id] = acc
	return acc, a.saveLocked()
}
func (a *Accounts) authenticate(user, password, peer string) (Account, error) {
	if host, _, e := net.SplitHostPort(peer); e == nil {
		peer = host
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	filtered := a.failed[peer][:0]
	for _, v := range a.failed[peer] {
		if now.Sub(v) < 15*time.Minute {
			filtered = append(filtered, v)
		}
	}
	a.failed[peer] = filtered
	if len(filtered) >= 10 {
		return Account{}, errors.New("слишком много попыток входа; повторите позже")
	}
	for _, u := range a.data.Accounts {
		if strings.EqualFold(u.Username, user) && verifyPassword(password, u.Password) {
			delete(a.failed, peer)
			return u, nil
		}
	}
	a.failed[peer] = append(a.failed[peer], now)
	return Account{}, errors.New("неверный логин или пароль")
}
func (a *Accounts) session(id string) (string, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(digest[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data.Sessions[key] = LoginSession{UserID: id, Expires: time.Now().UTC().Add(sessionTTL)}
	if err = a.saveLocked(); err != nil {
		return "", err
	}
	return token, nil
}
func (a *Accounts) current(token string) (Account, bool) {
	if len(token) != 64 {
		return Account{}, false
	}
	digest := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(digest[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.data.Sessions[key]
	if !ok || time.Now().After(s.Expires) {
		return Account{}, false
	}
	u, ok := a.data.Accounts[s.UserID]
	return u, ok
}
func (a *Accounts) logout(token string) error {
	digest := sha256.Sum256([]byte(token))
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.data.Sessions, hex.EncodeToString(digest[:]))
	return a.saveLocked()
}
func (a *Accounts) setRegistration(open bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data.RegistrationOpen = open
	return a.saveLocked()
}
func (a *Accounts) registration() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.data.RegistrationOpen
}
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}
func setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// ResetAdminPassword is a maintenance operation. Stop the running service
// before executing it so no concurrent process can overwrite accounts.json.
func ResetAdminPassword(root, adminName, seedPassword, newPassword string) error {
	a, err := newAccounts(root, adminName, seedPassword)
	if err != nil {
		return err
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	acc, ok := a.data.Accounts["admin"]
	if !ok || !acc.Admin {
		return errors.New("учётная запись администратора отсутствует")
	}
	acc.Password = hash
	a.data.Accounts["admin"] = acc
	// Revoke old sessions after password reset.
	for key, sess := range a.data.Sessions {
		if sess.UserID == "admin" {
			delete(a.data.Sessions, key)
		}
	}
	return a.saveLocked()
}

// Name lookup intentionally reveals nothing until called by the owner of an archive.
func (a *Accounts) userByName(name string) (Account, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.data.Accounts {
		if strings.EqualFold(u.Username, name) {
			return u, true
		}
	}
	return Account{}, false
}
func (a *Accounts) username(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if u, ok := a.data.Accounts[id]; ok {
		return u.Username
	}
	return ""
}
