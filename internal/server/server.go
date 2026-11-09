package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	"path/filepath"
	"sort"
	"strconv"
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
}
type App struct {
	cfg    Config
	store  *Storage
	boosty *boosty.Client
	queue  chan string
}

func New(cfg Config) (*App, error) {
	if cfg.AdminUser == "" || len(cfg.AdminPassword) < 12 {
		return nil, errors.New("задайте BD_ADMIN_USER и пароль BD_ADMIN_PASSWORD длиной от 12 символов")
	}
	s, e := newStorage(cfg.DataDir, cfg.SecretKey)
	if e != nil {
		return nil, e
	}
	t, e := s.loadTokens()
	if e != nil {
		return nil, e
	}
	a := &App{cfg: cfg, store: s, boosty: boosty.New(t, s.saveTokens), queue: make(chan string, 128)}
	go a.worker()
	for _, id := range s.queued() {
		a.queue <- id
	}
	return a, nil
}
func (a *App) worker() {
	for id := range a.queue {
		a.runJob(id)
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
func (a *App) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		ah := sha256.Sum256([]byte(u))
		bh := sha256.Sum256([]byte(a.cfg.AdminUser))
		ph := sha256.Sum256([]byte(p))
		qh := sha256.Sum256([]byte(a.cfg.AdminPassword))
		if !ok || subtle.ConstantTimeCompare(ah[:], bh[:]) != 1 || subtle.ConstantTimeCompare(ph[:], qh[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Boosty Downloader"`)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Authentication required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; media-src 'self'")
		w.Header().Set("Cache-Control", "no-store")
		b, e := ui.ReadFile("ui/index.html")
		if e != nil {
			http.Error(w, "UI unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("GET /ui/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		b, _ := ui.ReadFile("ui/app.js")
		w.Write(b)
	})
	mux.HandleFunc("GET /ui/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		b, _ := ui.ReadFile("ui/style.css")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/auth", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]bool{"connected": a.boosty.HasToken()})
	})
	mux.HandleFunc("POST /api/auth", func(w http.ResponseWriter, r *http.Request) {
		var t boosty.Tokens
		if e := decodeRequest(w, r, &t); e != nil {
			return
		}
		if len(t.AccessToken) > 16384 || len(t.RefreshToken) > 16384 || len(t.DeviceID) > 1024 {
			fail(w, 400, "слишком длинный токен")
			return
		}
		if e := a.boosty.SetTokens(t); e != nil {
			fail(w, 500, "не удалось сохранить авторизацию")
			return
		}
		reply(w, 200, map[string]bool{"connected": t.AccessToken != ""})
	})
	mux.HandleFunc("GET /api/post", a.preview)
	mux.HandleFunc("GET /api/stream", a.stream)
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobs := a.store.all()
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
		reply(w, 200, jobs)
	})
	mux.HandleFunc("POST /api/jobs", a.newJob)
	mux.HandleFunc("GET /library/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; media-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		http.StripPrefix("/library/", http.FileServer(http.Dir(filepath.Join(a.cfg.DataDir, "library")))).ServeHTTP(w, r)
	})
	protected := a.auth(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}
func decodeRequest(w http.ResponseWriter, r *http.Request, v any) error {
	if r.Header.Get("Content-Type") != "application/json" {
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
func (a *App) preview(w http.ResponseWriter, r *http.Request) {
	author, id, e := boosty.PostURL(r.URL.Query().Get("url"))
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	post, e := a.boosty.Post(ctx, author, id)
	if e != nil {
		fail(w, 502, e.Error())
		return
	}
	type item struct {
		Index int    `json:"index"`
		Type  string `json:"type"`
		Title string `json:"title"`
		Size  int64  `json:"size"`
	}
	medias := boosty.ExtractMedia(post)
	items := make([]item, 0, len(medias))
	for i, m := range medias {
		items = append(items, item{i, m.Type, m.Title, m.Size})
	}
	reply(w, 200, map[string]any{"title": post.Title, "author": author, "postId": id, "hasAccess": post.HasAccess, "media": items, "comments": post.Count.Comments})
}
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
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
	defer cancel()
	p, e := a.boosty.Post(ctx, author, id)
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
	res, e := a.boosty.MediaGet(ctx, boosty.MediaURL(p, m), r.Header.Get("Range"))
	if e != nil {
		fail(w, 502, "ошибка CDN: "+e.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		fail(w, 502, fmt.Sprintf("CDN HTTP %d", res.StatusCode))
		return
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := res.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", boosty.MediaName(m, i)))
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
	}
	if e := decodeRequest(w, r, &body); e != nil {
		return
	}
	author, id, e := boosty.PostURL(body.URL)
	if e != nil {
		fail(w, 400, e.Error())
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
	j := Job{ID: hex.EncodeToString(buf), Author: author, PostID: id, URL: "https://boosty.to/" + url.PathEscape(author) + "/posts/" + url.PathEscape(id), State: "queued", CreatedAt: time.Now().UTC(), IncludeComments: body.IncludeComments, IncludeFiles: body.IncludeFiles, IncludeVideos: body.IncludeVideos, IncludeAudio: body.IncludeAudio}
	if e := a.store.upsert(j); e != nil {
		fail(w, 500, "не удалось создать задачу")
		return
	}
	a.queue <- j.ID
	reply(w, 202, j)
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
	return Config{Address: addr, DataDir: data, AdminUser: user, AdminPassword: os.Getenv("BD_ADMIN_PASSWORD"), SecretKey: os.Getenv("BD_SECRET_KEY")}
}
