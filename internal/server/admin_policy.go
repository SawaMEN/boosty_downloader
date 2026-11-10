package server

import (
	"errors"
	"net/http"
	"sort"
	"time"
)

// All capability decisions are enforced on the server. UI controls are only hints.
// Defaults apply to new users; a per-user override takes precedence.
var defaultCapabilities = map[string]bool{
	"connectBoosty":  true,
	"browsePosts":    true,
	"streamMedia":    true,
	"createArchives": true,
	"saveComments":   true,
	"saveFiles":      true,
	"saveVideos":     true,
	"saveAudio":      true,
	"importAuthors":  true,
	"autoArchive":    false,
	"shareDirect":    true,
	"sharePublic":    false,
	"browseShared":   true,
	"deleteArchives": true,
}

func policyKeys() []string {
	keys := make([]string, 0, len(defaultCapabilities))
	for k := range defaultCapabilities {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (a *Accounts) initializePolicy(initial time.Duration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.data.DefaultPermissions == nil {
		a.data.DefaultPermissions = make(map[string]bool)
	}
	for k, v := range defaultCapabilities {
		if _, ok := a.data.DefaultPermissions[k]; !ok {
			a.data.DefaultPermissions[k] = v
		}
	}
	if a.data.UserPermissions == nil {
		a.data.UserPermissions = make(map[string]map[string]bool)
	}
	if a.data.SyncIntervalHours == nil {
		hours := 24
		if initial >= time.Hour {
			hours = int(initial / time.Hour)
			if hours > 168 {
				hours = 168
			}
		}
		if initial == 0 {
			hours = 0
		}
		a.data.SyncIntervalHours = &hours
	}
	if a.data.CompressionLevel == nil {
		defaultLevel := 3
		a.data.CompressionLevel = &defaultLevel
	}
	if a.data.DeduplicateMedia == nil {
		enabled := true
		a.data.DeduplicateMedia = &enabled
	}
	if a.data.MinFreeGiB == nil {
		reserve := 1
		a.data.MinFreeGiB = &reserve
	}
	if a.data.MaxUserGiB == nil {
		limit := 0
		a.data.MaxUserGiB = &limit
	}
	if a.data.AutoUpdatePolicy == nil {
		defaults := defaultAutoUpdatePolicy()
		a.data.AutoUpdatePolicy = &defaults
	}
	if a.data.TrashPolicy == nil {
		defaults := defaultTrashPolicy()
		a.data.TrashPolicy = &defaults
	}
	if a.data.VideoPolicy == nil {
		defaults := defaultVideoPolicy()
		a.data.VideoPolicy = &defaults
	}
	if a.data.MaxScanPosts == 0 {
		a.data.MaxScanPosts = 50
	}
	if a.data.MaxImportPosts == 0 {
		a.data.MaxImportPosts = importHardLimit
	}
	return a.saveLocked()
}

func (a *Accounts) allowed(id, key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.data.Accounts[id]
	if !ok {
		return false
	}
	if u.Admin {
		return true
	}
	if _, exists := defaultCapabilities[key]; !exists {
		return false
	}
	if v, ok := a.data.UserPermissions[id][key]; ok {
		return v
	}
	return a.data.DefaultPermissions[key]
}

func (a *Accounts) effective(id string) map[string]bool {
	result := make(map[string]bool, len(defaultCapabilities))
	for _, key := range policyKeys() {
		result[key] = a.allowed(id, key)
	}
	return result
}

// Snapshot avoids holding the account mutex while responding to HTTP requests.
type AdminPolicy struct {
	Defaults          map[string]bool `json:"defaults"`
	SyncIntervalHours int             `json:"syncIntervalHours"`
	MaxScanPosts      int             `json:"maxScanPosts"`
	MaxImportPosts    int             `json:"maxImportPosts"`
	RegistrationOpen  bool            `json:"registrationOpen"`
	CompressionLevel  int             `json:"compressionLevel"`
	DeduplicateMedia  bool            `json:"deduplicateMedia"`
	MinFreeGiB        int             `json:"minFreeGiB"`
	MaxUserGiB        int             `json:"maxUserGiB"`
	Video             VideoPolicy     `json:"video"`

	Updates AutoUpdatePolicy `json:"updates"`
	Trash   TrashPolicy       `json:"trash"`
}

func (a *Accounts) policy() AdminPolicy {
	a.mu.Lock()
	defer a.mu.Unlock()
	defaults := make(map[string]bool, len(defaultCapabilities))
	for k := range defaultCapabilities {
		defaults[k] = a.data.DefaultPermissions[k]
	}
	hours := 0
	if a.data.SyncIntervalHours != nil {
		hours = *a.data.SyncIntervalHours
	}
	level, dedup := 0, false
	if a.data.CompressionLevel != nil {
		level = *a.data.CompressionLevel
	}
	if a.data.DeduplicateMedia != nil {
		dedup = *a.data.DeduplicateMedia
	}
	video := defaultVideoPolicy()
	if a.data.VideoPolicy != nil {
		video = *a.data.VideoPolicy
	}
	minFree, maxUser := 0, 0
	if a.data.MinFreeGiB != nil {
		minFree = *a.data.MinFreeGiB
	}
	if a.data.MaxUserGiB != nil {
		maxUser = *a.data.MaxUserGiB
	}
	updates := defaultAutoUpdatePolicy()
	if a.data.AutoUpdatePolicy != nil {
		updates = *a.data.AutoUpdatePolicy
	}
	trash := defaultTrashPolicy()
	if a.data.TrashPolicy != nil {
		trash = *a.data.TrashPolicy
	}
	return AdminPolicy{Trash: trash, Updates: updates, MinFreeGiB: minFree, MaxUserGiB: maxUser, Defaults: defaults, SyncIntervalHours: hours, MaxScanPosts: a.data.MaxScanPosts, MaxImportPosts: a.data.MaxImportPosts, RegistrationOpen: a.data.RegistrationOpen, CompressionLevel: level, DeduplicateMedia: dedup, Video: video}
}

func validatePolicy(p AdminPolicy) error {
	if err := validateAutoUpdatePolicy(p.Updates); err != nil {
		return err
	}
	if err := validateTrashPolicy(p.Trash); err != nil {
		return err
	}
	if err := validateVideoPolicy(p.Video); err != nil {
		return err
	}
	if p.MinFreeGiB < 0 || p.MinFreeGiB > 1024 {
		return errors.New("резерв свободного места: от 0 до 1024 ГиБ")
	}
	if p.MaxUserGiB < 0 || p.MaxUserGiB > 16384 {
		return errors.New("квота пользователя: от 0 до 16384 ГиБ")
	}
	if p.CompressionLevel < 0 || p.CompressionLevel > 19 {
		return errors.New("уровень Zstandard должен быть от 0 до 19; 0 отключает сжатие")
	}
	if p.SyncIntervalHours < 0 || p.SyncIntervalHours > 168 || (p.SyncIntervalHours != 0 && p.SyncIntervalHours < 1) {
		return errors.New("интервал: 0 (отключено) либо от 1 до 168 часов")
	}
	if p.MaxScanPosts < 1 || p.MaxScanPosts > 200 {
		return errors.New("за одну проверку можно просматривать от 1 до 200 постов")
	}
	if p.MaxImportPosts < 1 || p.MaxImportPosts > importHardLimit {
		return errors.New("лимит первичного импорта: от 1 до 2000 постов")
	}
	if len(p.Defaults) != len(defaultCapabilities) {
		return errors.New("передайте все разрешения")
	}
	for key := range p.Defaults {
		if _, ok := defaultCapabilities[key]; !ok {
			return errors.New("неизвестное разрешение: " + key)
		}
	}
	return nil
}
func (a *Accounts) changePolicy(p AdminPolicy) error {
	if err := validatePolicy(p); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Rollback on failed disk writes.
	before := a.data
	a.data.DefaultPermissions = p.Defaults
	a.data.SyncIntervalHours = &p.SyncIntervalHours
	a.data.MaxScanPosts = p.MaxScanPosts
	a.data.MaxImportPosts = p.MaxImportPosts
	a.data.RegistrationOpen = p.RegistrationOpen
	a.data.CompressionLevel = &p.CompressionLevel
	a.data.DeduplicateMedia = &p.DeduplicateMedia
	a.data.MinFreeGiB = &p.MinFreeGiB
	a.data.MaxUserGiB = &p.MaxUserGiB
	a.data.VideoPolicy = &p.Video
	a.data.AutoUpdatePolicy = &p.Updates
	a.data.TrashPolicy = &p.Trash
	if err := a.saveLocked(); err != nil {
		a.data = before
		return err
	}
	return nil
}

type AccountAdminView struct {
	ID          string          `json:"id"`
	Username    string          `json:"username"`
	Admin       bool            `json:"admin"`
	CreatedAt   time.Time       `json:"createdAt"`
	Overrides   map[string]bool `json:"overrides"`
	Permissions map[string]bool `json:"permissions"`
}

func (a *Accounts) listUsers() []AccountAdminView {
	a.mu.Lock()
	users := make([]AccountAdminView, 0, len(a.data.Accounts))
	for _, u := range a.data.Accounts {
		overrides := make(map[string]bool)
		effective := make(map[string]bool)
		for k, v := range a.data.UserPermissions[u.ID] {
			overrides[k] = v
		}
		for k := range defaultCapabilities {
			if u.Admin {
				effective[k] = true
			} else if v, ok := overrides[k]; ok {
				effective[k] = v
			} else {
				effective[k] = a.data.DefaultPermissions[k]
			}
		}
		users = append(users, AccountAdminView{ID: u.ID, Username: u.Username, Admin: u.Admin, CreatedAt: u.CreatedAt, Overrides: overrides, Permissions: effective})
	}
	a.mu.Unlock()
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	return users
}
func (a *Accounts) setOverrides(id string, overrides map[string]bool) error {
	if len(overrides) > len(defaultCapabilities) {
		return errors.New("слишком много разрешений")
	}
	for key := range overrides {
		if _, ok := defaultCapabilities[key]; !ok {
			return errors.New("неизвестное разрешение: " + key)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.data.Accounts[id]
	if !ok {
		return errors.New("пользователь не найден")
	}
	if u.Admin {
		return errors.New("права администратора не изменяются здесь")
	}
	prev, exists := a.data.UserPermissions[id]
	replacement := make(map[string]bool, len(overrides))
	for k, v := range overrides {
		replacement[k] = v
	}
	a.data.UserPermissions[id] = replacement
	if err := a.saveLocked(); err != nil {
		if exists {
			a.data.UserPermissions[id] = prev
		} else {
			delete(a.data.UserPermissions, id)
		}
		return err
	}
	return nil
}

func (a *App) require(key string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.accounts.allowed(principal(r).ID, key) {
			fail(w, http.StatusForbidden, "функция отключена администратором")
			return
		}
		next(w, r)
	}
}

func (a *App) adminPolicy(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	if r.Method == "GET" {
		reply(w, 200, a.accounts.policy())
		return
	}
	var p AdminPolicy
	if decodeRequest(w, r, &p) != nil {
		return
	}
	if err := a.accounts.changePolicy(p); err != nil {
		fail(w, 400, err.Error())
		return
	}
	reply(w, 200, a.accounts.policy())
}
func (a *App) adminUsers(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	reply(w, 200, a.accounts.listUsers())
}
func (a *App) adminUserPermissions(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	var request struct {
		Overrides map[string]bool `json:"overrides"`
	}
	if decodeRequest(w, r, &request) != nil {
		return
	}
	if request.Overrides == nil {
		request.Overrides = map[string]bool{}
	}
	if err := a.accounts.setOverrides(r.PathValue("id"), request.Overrides); err != nil {
		fail(w, 400, err.Error())
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (a *App) syncInterval() time.Duration {
	hours := a.accounts.policy().SyncIntervalHours
	if hours <= 0 {
		return 0
	}
	return time.Duration(hours) * time.Hour
}
