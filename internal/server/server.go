package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SawaMEN/boosty_downloader/internal/boosty"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui/*
var ui embed.FS

type Config struct {
	Address       string
	DataDir       string
	AdminUser     string
	AdminPassword string
	SecretKey     string
	SyncInterval  time.Duration
}
type App struct {
	cfg                Config
	store              *Storage
	accounts           *Accounts
	clients            map[string]*boosty.Client
	clientsMu          sync.Mutex
	queue              chan string
	importQueue        chan string
	subscriptionSource func(context.Context, string) (boosty.SubscriptionList, error)
	feedSource         func(context.Context, string, string, string) (boosty.FeedResult, error)
	ctx                context.Context
	cancel             context.CancelFunc
	wg                 sync.WaitGroup
	videoQueue         chan VideoTask
	videoMu            sync.Mutex
	videoStatuses      map[string]VideoJobStatus
	videoFailures      map[string]time.Time
	trashQueue         chan struct{}
	trashMu            sync.Mutex
	trashSweepQueued   bool
	trashSweepRunning  bool
}

func New(cfg Config) (*App, error) {
	if cfg.AdminUser == "" || len(cfg.AdminPassword) < 12 {
		return nil, errors.New("задайте BD_ADMIN_USER и пароль BD_ADMIN_PASSWORD от 12 символов")
	}
	if os.Getenv("BD_SQLITE_SHADOW") == "1" && os.Getenv("BD_SQLITE_VERIFY") == "1" {
		return nil, errors.New("BD_SQLITE_SHADOW и BD_SQLITE_VERIFY не должны использоваться одновременно")
	}
	// A crash may have interrupted the JSON two-file transaction. Resolve the
	// journal before comparing either JSON file with an offline SQLite export.
	if err := recoverMetadataPair(cfg.DataDir); err != nil {
		return nil, fmt.Errorf("восстановление метаданных не удалось: %w", err)
	}
	// Validate against the raw JSON before any legacy migration or default
	// serialization can rewrite authoritative files. No workers run yet.
	if os.Getenv("BD_SQLITE_VERIFY") == "1" {
		if err := verifySQLiteExport(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("проверка SQLite не пройдена; JSON остаётся основной базой: %w", err)
		}
	}
	s, err := newStorage(cfg.DataDir, cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	accounts, err := newAccounts(cfg.DataDir, cfg.AdminUser, cfg.AdminPassword)
	if err != nil {
		return nil, err
	}
	if err = accounts.initializePolicy(cfg.SyncInterval); err != nil {
		return nil, err
	}
	if err = s.migrateLegacyAuth(); err != nil {
		return nil, err
	}
	if err = s.migrateLegacyJobs(); err != nil {
		return nil, err
	}
	if err = s.migrateArchiveTimestamps(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if os.Getenv("BD_SQLITE_SHADOW") == "1" {
		if err := s.enableSQLiteShadow(ctx); err != nil {
			cancel()
			return nil, fmt.Errorf("режим SQLite shadow не включён; JSON остаётся основным: %w", err)
		}
	}
	a := &App{cfg: cfg, store: s, accounts: accounts, clients: make(map[string]*boosty.Client), queue: make(chan string, 128), importQueue: make(chan string, 64), videoQueue: make(chan VideoTask, 64), trashQueue: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	a.wg.Add(6)
	if s.shadow != nil {
		a.wg.Add(1)
		go func() { defer a.wg.Done(); s.sqliteShadowLoop(ctx) }()
	}
	go func() { defer a.wg.Done(); a.worker() }()
	go func() { defer a.wg.Done(); a.importWorker() }()
	go func() { defer a.wg.Done(); a.syncLoop() }()
	go func() { defer a.wg.Done(); a.importSyncLoop() }()
	go func() { defer a.wg.Done(); a.videoWorker() }()
	go func() { defer a.wg.Done(); a.trashCleanupLoop() }()
	for _, id := range s.queued() {
		a.queueJob(id)
	}
	for _, id := range s.queuedImports() {
		a.importQueue <- id
	}
	return a, nil
}
func (a *App) client(owner string) (*boosty.Client, error) {
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	if c := a.clients[owner]; c != nil {
		return c, nil
	}
	t, err := a.store.loadUserTokens(owner)
	if err != nil {
		return nil, err
	}
	c := boosty.New(t, func(t boosty.Tokens) error { return a.store.saveUserTokens(owner, t) })
	a.clients[owner] = c
	return c, nil
}

// Close stops discovery, streaming downloads and scheduled work. Persisted
// queued jobs are picked up by the next service process.
func (a *App) Close() {
	a.cancel()
	a.wg.Wait()
	if a.store.shadow != nil {
		_ = a.store.shadow.meta.close()
	}
}
func (a *App) queueJob(id string) bool {
	select {
	case <-a.ctx.Done():
		return false
	case a.queue <- id:
		return true
	}
}
func (a *App) worker() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case id := <-a.queue:
			a.runJob(id)
			if a.accounts.policy().Video.Enabled && a.accounts.policy().Video.Automatic {
				select {
				case a.videoQueue <- VideoTask{JobID: id}:
				default:
				}
			}
		}
	}
}
func reply(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}

type accountContext struct{}

func principal(r *http.Request) Account {
	v, _ := r.Context().Value(accountContext{}).(Account)
	return v
}
func (a *App) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.accounts.current(sessionToken(r))
		if !ok {
			fail(w, 401, "войдите в учётную запись загрузчика")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountContext{}, u)))
	})
}
func (a *App) stateChangeGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if r.Header.Get("X-BD-Request") != "1" {
				fail(w, 403, "неверный CSRF-заголовок")
				return
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				fail(w, 403, "запрос другого сайта запрещён")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (a *App) Handler() http.Handler {
	public := http.NewServeMux()
	public.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	public.HandleFunc("GET /api/public-config", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]bool{"registrationOpen": a.accounts.registration()})
	})
	public.HandleFunc("POST /api/account/login", a.login)
	public.HandleFunc("POST /api/account/register", a.register)
	public.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; media-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		b, e := ui.ReadFile("ui/index.html")
		if e != nil {
			http.Error(w, "UI unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	public.HandleFunc("GET /ui/{asset}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("asset")
		switch name {
		case "app.js", "library.js", "player.js", "subscriptions.js", "admin.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case "style.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		default:
			http.NotFound(w, r)
			return
		}
		b, e := ui.ReadFile("ui/" + name)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=180")
		_, _ = w.Write(b)
	})
	private := http.NewServeMux()
	private.HandleFunc("GET /api/account", func(w http.ResponseWriter, r *http.Request) {
		u := principal(r)
		reply(w, 200, map[string]any{"id": u.ID, "username": u.Username, "admin": u.Admin, "permissions": a.accounts.effective(u.ID)})
	})
	private.HandleFunc("POST /api/account/logout", func(w http.ResponseWriter, r *http.Request) {
		_ = a.accounts.logout(sessionToken(r))
		setSessionCookie(w, "", -1)
		reply(w, 200, map[string]bool{"ok": true})
	})
	private.HandleFunc("GET /api/admin/video", a.videoAdmin)
	private.HandleFunc("POST /api/admin/video/jobs/{id}", a.videoEnqueue)
	private.HandleFunc("GET /api/admin/policy", a.adminPolicy)
	private.HandleFunc("PUT /api/admin/policy", a.adminPolicy)
	private.HandleFunc("GET /api/admin/storage", a.adminStorage)
	private.HandleFunc("GET /api/admin/sqlite-shadow", a.adminSQLiteShadow)
	private.HandleFunc("GET /api/admin/trash", a.adminTrashStatus)
	private.HandleFunc("POST /api/admin/trash/cleanup", a.adminTrashCleanup)
	private.HandleFunc("GET /api/admin/updates", a.adminUpdates)
	private.HandleFunc("GET /api/admin/users", a.adminUsers)
	private.HandleFunc("PUT /api/admin/users/{id}/permissions", a.adminUserPermissions)
	private.HandleFunc("GET /api/admin/registration", a.adminRegistration)
	private.HandleFunc("POST /api/admin/registration", a.adminRegistration)
	private.HandleFunc("GET /api/subscriptions", a.require("importAuthors", a.subscribedAuthors))
	private.HandleFunc("POST /api/subscriptions/import", a.require("importAuthors", a.importSubscriptions))
	private.HandleFunc("GET /api/imports", a.importsForUser)
	private.HandleFunc("POST /api/imports/{id}/cancel", a.cancelImport)
	private.HandleFunc("PUT /api/imports/{id}/schedule", a.importSchedule)
	private.HandleFunc("GET /api/auth", a.boostyAuth)
	private.HandleFunc("POST /api/auth", a.require("connectBoosty", a.boostyAuth))
	private.HandleFunc("DELETE /api/auth", a.boostyAuth)
	private.HandleFunc("GET /api/post", a.require("browsePosts", a.postDetails))
	private.HandleFunc("GET /api/comments", a.require("browsePosts", a.postComments))
	private.HandleFunc("GET /api/feed", a.require("browsePosts", a.feed))
	private.HandleFunc("GET /api/settings", a.settings)
	private.HandleFunc("GET /api/stream", a.require("streamMedia", a.stream))
	private.HandleFunc("GET /api/shared", a.require("browseShared", a.sharedJobs))
	private.HandleFunc("GET /api/catalog", a.catalog)
	private.HandleFunc("GET /api/authors", a.authors)
	private.HandleFunc("POST /api/authors", a.authors)
	private.HandleFunc("DELETE /api/authors/{author}", a.unpinAuthor)
	private.HandleFunc("DELETE /api/authors/{author}/archives", a.require("deleteArchives", a.deleteAuthorArchives))
	private.HandleFunc("GET /api/jobs/{id}/shares", a.jobShares)
	private.HandleFunc("POST /api/jobs/{id}/shares", a.jobShares)
	private.HandleFunc("DELETE /api/jobs/{id}/shares", a.jobShares)
	private.HandleFunc("PUT /api/jobs/{id}/visibility", a.setJobVisibility)
	private.HandleFunc("DELETE /api/jobs/{id}", a.require("deleteArchives", a.deleteJob))
	private.HandleFunc("GET /api/trash", a.require("deleteArchives", a.listTrash))
	private.HandleFunc("POST /api/trash/{id}/restore", a.require("deleteArchives", a.restoreTrash))
	private.HandleFunc("DELETE /api/trash/{id}", a.require("deleteArchives", a.purgeTrash))
	private.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		owner := principal(r).ID
		jobs := a.store.all()
		selected := make([]Job, 0)
		for _, j := range jobs {
			if j.OwnerID == owner {
				selected = append(selected, j)
			}
		}
		sort.Slice(selected, func(i, j int) bool { return selected[i].CreatedAt.After(selected[j].CreatedAt) })
		reply(w, 200, selected)
	})
	private.HandleFunc("POST /api/jobs", a.require("createArchives", a.newJob))
	private.HandleFunc("POST /api/jobs/{id}/retry", a.require("createArchives", a.retryJob))
	private.HandleFunc("GET /api/jobs/{id}/zip", a.zipJob)
	private.HandleFunc("GET /api/jobs/{id}/manifest", a.jobManifest)
	private.HandleFunc("GET /api/jobs/{id}/revisions", a.listJobRevisions)
	private.HandleFunc("POST /api/jobs/{id}/revisions/{rev}/restore", a.require("createArchives", a.restoreJobRevision))
	private.HandleFunc("POST /api/jobs/{id}/verify", a.verifyJob)
	private.HandleFunc("GET /library/{id}/revisions/{rev}/{file...}", a.revisionFile)
	private.HandleFunc("GET /library/{id}/{file...}", a.libraryFile)
	secured := a.auth(private)
	return a.stateChangeGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/ui/") || r.URL.Path == "/healthz" || r.URL.Path == "/api/public-config" || r.URL.Path == "/api/account/login" || r.URL.Path == "/api/account/register" {
			public.ServeHTTP(w, r)
			return
		}
		secured.ServeHTTP(w, r)
	}))
}
func decodeRequest(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		fail(w, 415, "требуется application/json")
		return errors.New("content-type")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if e := dec.Decode(v); e != nil {
		fail(w, 400, "ошибка JSON")
		return e
	}
	if dec.Decode(new(any)) != io.EOF {
		fail(w, 400, "ожидается один объект JSON")
		return errors.New("trailing data")
	}
	return nil
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decodeRequest(w, r, &v) != nil {
		return
	}
	acc, err := a.accounts.authenticate(v.Username, v.Password, r.RemoteAddr)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	token, err := a.accounts.session(acc.ID)
	if err != nil {
		fail(w, 500, "не удалось открыть сессию")
		return
	}
	setSessionCookie(w, token, int(sessionTTL.Seconds()))
	reply(w, 200, map[string]any{"username": acc.Username, "admin": acc.Admin})
}
func (a *App) register(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decodeRequest(w, r, &v) != nil {
		return
	}
	acc, err := a.accounts.register(v.Username, v.Password)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	token, err := a.accounts.session(acc.ID)
	if err != nil {
		fail(w, 500, "не удалось открыть сессию")
		return
	}
	setSessionCookie(w, token, int(sessionTTL.Seconds()))
	reply(w, 201, map[string]any{"username": acc.Username, "admin": false})
}
func (a *App) adminRegistration(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	if r.Method == "GET" {
		reply(w, 200, map[string]bool{"registrationOpen": a.accounts.registration()})
		return
	}
	var v struct {
		RegistrationOpen bool `json:"registrationOpen"`
	}
	if decodeRequest(w, r, &v) != nil {
		return
	}
	if err := a.accounts.setRegistration(v.RegistrationOpen); err != nil {
		fail(w, 500, "не удалось сохранить настройки")
		return
	}
	reply(w, 200, map[string]bool{"registrationOpen": v.RegistrationOpen})
}
func (a *App) boostyAuth(w http.ResponseWriter, r *http.Request) {
	c, err := a.client(principal(r).ID)
	if err != nil {
		fail(w, 500, "ошибка хранилища Boosty")
		return
	}
	if r.Method == "GET" {
		reply(w, 200, map[string]bool{"connected": c.HasToken()})
		return
	}
	if r.Method == "DELETE" {
		if err = c.SetTokens(boosty.Tokens{}); err != nil {
			fail(w, 500, "не удалось очистить токены")
			return
		}
		reply(w, 200, map[string]bool{"connected": false})
		return
	}
	var v struct {
		Method       string `json:"method"`
		Value        string `json:"value"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		DeviceID     string `json:"deviceId"`
	}
	if decodeRequest(w, r, &v) != nil {
		return
	}
	t := boosty.Tokens{AccessToken: strings.TrimSpace(v.AccessToken), RefreshToken: strings.TrimSpace(v.RefreshToken), DeviceID: strings.TrimSpace(v.DeviceID)}
	if v.Method == "cookie" || v.Method == "json" {
		cookie := strings.TrimSpace(v.Value)
		if strings.HasPrefix(cookie, "auth=") {
			cookie = strings.SplitN(strings.TrimPrefix(cookie, "auth="), ";", 2)[0]
		}
		if decoded, e := url.PathUnescape(cookie); e == nil {
			cookie = decoded
		}
		var raw struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			DeviceID     string `json:"deviceId"`
		}
		if e := json.Unmarshal([]byte(cookie), &raw); e != nil {
			fail(w, 400, "неверный JSON cookie Boosty")
			return
		}
		t = boosty.Tokens{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, DeviceID: raw.DeviceID}
	} else if v.Method != "" && v.Method != "tokens" {
		fail(w, 400, "неподдерживаемый способ входа")
		return
	}
	if len(t.AccessToken) == 0 || len(t.AccessToken) > 16384 || len(t.RefreshToken) > 16384 || len(t.DeviceID) > 1024 {
		fail(w, 400, "укажите действительный access token")
		return
	}
	if err = c.SetTokens(t); err != nil {
		fail(w, 500, "не удалось сохранить токены")
		return
	}
	reply(w, 200, map[string]bool{"connected": true})
}
func (a *App) ownedJob(r *http.Request) (Job, bool) {
	j, ok := a.store.get(r.PathValue("id"))
	return j, ok && j.OwnerID == principal(r).ID
}
func (a *App) libraryFile(w http.ResponseWriter, r *http.Request) {
	j, ok := a.readableJob(r)
	if !ok || (j.State != "done" && j.State != "partial") {
		http.NotFound(w, r)
		return
	}
	base := a.store.archiveDir(j.OwnerID, j.Author, j.PostID)
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
			if safeArchivePart(asset.Filename) && "media/"+asset.Filename == file {
				allowed = true
				break
			}
		}
		if !allowed {
			http.NotFound(w, r)
			return
		}
	}
	// Serve the already opened regular inode; never follow a newly inserted
	// symlink between metadata validation and file streaming.
	serveArchiveRegular(w, r, base, file)
}
func (a *App) preview(w http.ResponseWriter, r *http.Request) { a.postDetails(w, r) }
func (a *App) stream(w http.ResponseWriter, r *http.Request) {
	author, id, e := boosty.PostURL(r.URL.Query().Get("url"))
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	i, e := strconv.Atoi(r.URL.Query().Get("index"))
	if e != nil || i < 0 || i > 1024 {
		fail(w, 400, "недопустимый номер медиа")
		return
	}
	c, e := a.client(principal(r).ID)
	if e != nil {
		fail(w, 500, "ошибка авторизации Boosty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
	defer cancel()
	p, e := c.Post(ctx, author, id)
	if e != nil {
		fail(w, 502, e.Error())
		return
	}
	if !p.HasAccess {
		fail(w, 403, "нет доступа к посту")
		return
	}
	medias := boosty.ExtractMedia(p)
	if i >= len(medias) {
		fail(w, 404, "медиа не найдено")
		return
	}
	m := medias[i]
	mediaURL := boosty.MediaURL(p, m)
	quality := r.URL.Query().Get("quality")
	if m.Type == "ok_video" && quality != "" {
		found := false
		for _, u := range m.PlayerURLs {
			if u.Type == quality && u.URL != "" {
				mediaURL = u.URL
				found = true
				break
			}
		}
		if !found {
			fail(w, 400, "указанное качество недоступно")
			return
		}
	}
	res, e := c.MediaGet(ctx, mediaURL, r.Header.Get("Range"), "")
	if e != nil {
		fail(w, 502, "ошибка CDN: "+redactNetError(e))
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		fail(w, 502, fmt.Sprintf("CDN HTTP %d", res.StatusCode))
		return
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := res.Header.Get(key); v != "" {
			w.Header().Set(key, v)
		}
	}
	if r.URL.Query().Get("inline") == "1" && (m.Type == "ok_video" || m.Type == "audio_file" || m.Type == "image") {
		w.Header().Set("Content-Disposition", "inline")
	} else {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", boosty.MediaName(m, i)))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}
func (a *App) newJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL             string `json:"url"`
		IncludeComments bool   `json:"includeComments"`
		IncludeFiles    bool   `json:"includeFiles"`
		IncludeVideos   bool   `json:"includeVideos"`
		IncludeAudio    bool   `json:"includeAudio"`
		AutoSync        bool   `json:"autoSync"`
	}
	if decodeRequest(w, r, &body) != nil {
		return
	}
	author, id, e := boosty.PostURL(body.URL)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	for _, feature := range []struct {
		requested bool
		key       string
	}{
		{body.IncludeComments, "saveComments"}, {body.IncludeFiles, "saveFiles"},
		{body.IncludeVideos, "saveVideos"}, {body.IncludeAudio, "saveAudio"},
	} {
		if feature.requested && !a.accounts.allowed(principal(r).ID, feature.key) {
			fail(w, 403, "тип вложений отключён администратором: "+feature.key)
			return
		}
	}
	if body.AutoSync && !a.accounts.allowed(principal(r).ID, "autoArchive") {
		fail(w, 403, "автоматическая архивация отключена администратором")
		return
	}
	if len(a.queue) >= cap(a.queue) {
		fail(w, 429, "очередь заполнена")
		return
	}
	buf := make([]byte, 12)
	if _, e = rand.Read(buf); e != nil {
		fail(w, 500, "невозможно создать задачу")
		return
	}
	j := Job{ID: hex.EncodeToString(buf), OwnerID: principal(r).ID, Author: author, PostID: id, URL: "https://boosty.to/" + url.PathEscape(author) + "/posts/" + url.PathEscape(id), State: "queued", CreatedAt: time.Now().UTC(), IncludeComments: body.IncludeComments, IncludeFiles: body.IncludeFiles, IncludeVideos: body.IncludeVideos, IncludeAudio: body.IncludeAudio, AutoSync: body.AutoSync}
	createdJob, created, err := a.store.createUniqueJob(j)
	if err != nil {
		fail(w, 500, "не удалось создать задачу")
		return
	}
	if !created {
		reply(w, 200, createdJob)
		return
	}
	a.queueJob(createdJob.ID)
	reply(w, 202, createdJob)
}
func FromEnv() Config {
	addr := os.Getenv("BD_LISTEN")
	if addr == "" {
		addr = ":8443"
	}
	data := os.Getenv("BD_DATA_DIR")
	if data == "" {
		data = "./state"
	}
	user := os.Getenv("BD_ADMIN_USER")
	if user == "" {
		user = "admin"
	}
	return Config{Address: addr, DataDir: data, AdminUser: user, AdminPassword: os.Getenv("BD_ADMIN_PASSWORD"), SecretKey: os.Getenv("BD_SECRET_KEY"), SyncInterval: normalizeInterval(os.Getenv("BD_SYNC_INTERVAL_HOURS"))}
}
