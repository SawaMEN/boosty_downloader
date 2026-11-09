package boosty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PostURL rejects arbitrary remote URLs. Only Boosty post URLs are accepted.
func PostURL(raw string) (author, id string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme != "https" || (u.Hostname() != "boosty.to" && u.Hostname() != "www.boosty.to") || u.Port() != "" || u.User != nil {
		return "", "", errors.New("требуется HTTPS-ссылка на пост boosty.to")
	}
	p := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(p) != 3 || p[1] != "posts" {
		return "", "", errors.New("ожидается ссылка /автор/posts/идентификатор")
	}
	for i := range p {
		p[i], err = url.PathUnescape(p[i])
		if err != nil {
			return "", "", errors.New("неверная кодировка ссылки")
		}
	}
	if !safeSegment(p[0]) || !safeSegment(p[2]) {
		return "", "", errors.New("недопустимый идентификатор поста")
	}
	return p[0], p[2], nil
}
func safeSegment(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// Public media transport blocks private, loopback, link-local and reserved IPs,
// including redirects and DNS-resolved addresses. No proxy env variables are used.
func permittedIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if v := ip.To4(); v != nil {
		if v[0] == 0 || v[0] == 10 || v[0] == 127 || v[0] >= 224 || v[0] == 169 && v[1] == 254 || v[0] == 100 && v[1] >= 64 && v[1] <= 127 || v[0] == 192 && v[1] == 0 && v[2] == 0 || v[0] == 192 && v[1] == 0 && v[2] == 2 || v[0] == 198 && v[1] == 18 || v[0] == 198 && v[1] == 19 || v[0] == 198 && v[1] == 51 && v[2] == 100 || v[0] == 203 && v[1] == 0 && v[2] == 113 {
			return false
		}
	}
	return true
}
func validateMediaURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("разрешены только HTTPS-адреса медиа")
	}
	if p := u.Port(); p != "" && p != "443" {
		return errors.New("недопустимый порт медиа")
	}
	return nil
}
func safeMediaClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: 12 * time.Second}
		var last error
		for _, ip := range ips {
			if !permittedIP(ip.IP) {
				continue
			}
			c, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if e == nil {
				return c, nil
			}
			last = e
		}
		if last != nil {
			return nil, last
		}
		return nil, errors.New("сетевой адрес запрещён")
	}
	tr.ResponseHeaderTimeout = 25 * time.Second
	tr.MaxIdleConnsPerHost = 4
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("слишком много переадресаций")
		}
		return validateMediaURL(req.URL.String())
	}}
}

type Tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	DeviceID     string `json:"deviceId,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
}
type Media struct {
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	Title      string      `json:"title"`
	URL        string      `json:"url"`
	Size       int64       `json:"size"`
	PlayerURLs []PlayerURL `json:"playerUrls"`
}
type PlayerURL struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}
type Post struct {
	Raw         json.RawMessage   `json:"-"`
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	HasAccess   bool              `json:"hasAccess"`
	PublishTime int64             `json:"publishTime"`
	SignedQuery string            `json:"signedQuery"`
	Data        []json.RawMessage `json:"data"`
	Count       struct {
		Comments int `json:"comments"`
	} `json:"count"`
	User struct {
		Name    string `json:"name"`
		BlogURL string `json:"blogUrl"`
	} `json:"user"`
}
type Comment struct {
	ID         string            `json:"id"`
	Content    json.RawMessage   `json:"content"`
	Data       []json.RawMessage `json:"data"`
	ReplyCount int               `json:"replyCount"`
	Replies    struct {
		Data []Comment `json:"data"`
	} `json:"replies"`
	Author struct {
		Name string `json:"name"`
	} `json:"author"`
	CreatedAt int64 `json:"createdAt"`
}
type CommentResult struct {
	Raw      json.RawMessage `json:"-"`
	Comments []Comment       `json:"comments"`
	Partial  bool            `json:"partial"`
	Expected int             `json:"expected"`
	Received int             `json:"received"`
	Reason   string          `json:"reason,omitempty"`
}

type Client struct {
	mu      sync.Mutex
	tokens  Tokens
	save    func(Tokens) error
	api     *http.Client
	media   *http.Client
	apiBase string
}

func New(tokens Tokens, save func(Tokens) error) *Client {
	return &Client{tokens: tokens, save: save, api: safeMediaClient(), media: safeMediaClient(), apiBase: "https://api.boosty.to"}
}
func (c *Client) SetTokens(t Tokens) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.save != nil {
		if err := c.save(t); err != nil {
			return err
		}
	}
	c.tokens = t
	return nil
}
func (c *Client) HasToken() bool   { c.mu.Lock(); defer c.mu.Unlock(); return c.tokens.AccessToken != "" }
func (c *Client) Snapshot() Tokens { c.mu.Lock(); defer c.mu.Unlock(); return c.tokens }
func (c *Client) doJSON(ctx context.Context, route string, result any) error {
	c.mu.Lock()
	t := c.tokens
	c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+route, nil)
		if e != nil {
			return e
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 BoostyArchiver/0.1")
		if t.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+t.AccessToken)
		}
		resp, e := c.api.Do(req)
		if e != nil {
			return e
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && t.RefreshToken != "" {
			resp.Body.Close()
			if e = c.refresh(ctx, t); e != nil {
				return fmt.Errorf("сессия Boosty истекла: %w", e)
			}
			t = c.Snapshot()
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("Boosty API вернул HTTP %d", resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 12<<20)).Decode(result)
	}
	return errors.New("не удалось авторизоваться в Boosty")
}
func (c *Client) refresh(ctx context.Context, previous Tokens) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens.RefreshToken != previous.RefreshToken {
		return nil
	}
	if previous.DeviceID == "" {
		return errors.New("для обновления токена требуется deviceId")
	}
	b, _ := json.Marshal(map[string]string{"device_id": previous.DeviceID, "refresh_token": previous.RefreshToken})
	req, e := http.NewRequestWithContext(ctx, "POST", c.apiBase+"/oauth/token/", bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	res, e := c.api.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("обновление отклонено (HTTP %d)", res.StatusCode)
	}
	var v struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
	}
	if e := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&v); e != nil {
		return e
	}
	if v.Access == "" || v.Refresh == "" {
		return errors.New("Boosty не вернул токены")
	}
	previous.AccessToken = v.Access
	previous.RefreshToken = v.Refresh
	previous.ExpiresAt = time.Now().Unix() + v.Expires
	if c.save != nil {
		if e := c.save(previous); e != nil {
			return e
		}
	}
	c.tokens = previous
	return nil
}
func (c *Client) Post(ctx context.Context, author, id string) (*Post, error) {
	if !safeSegment(author) || !safeSegment(id) {
		return nil, errors.New("недопустимый путь")
	}
	var raw json.RawMessage
	e := c.doJSON(ctx, "/v1/blog/"+url.PathEscape(author)+"/post/"+url.PathEscape(id), &raw)
	if e != nil {
		return nil, e
	}
	var p Post
	if e = json.Unmarshal(raw, &p); e != nil {
		return nil, e
	}
	p.Raw = raw
	if p.ID == "" {
		return nil, errors.New("пустой пост")
	}
	return &p, nil
}
func countComments(arr []Comment) (n int, truncated bool) {
	for _, x := range arr {
		n++
		n1, t := countComments(x.Replies.Data)
		n += n1
		if t || x.ReplyCount > len(x.Replies.Data) {
			truncated = true
		}
	}
	return
}
func (c *Client) Comments(ctx context.Context, author, id string, expected int) (CommentResult, error) {
	var v struct {
		Data []Comment `json:"data"`
	}
	route := "/v1/blog/" + url.PathEscape(author) + "/post/" + url.PathEscape(id) + "/comment/?limit=101&reply_limit=100"
	var raw json.RawMessage
	if e := c.doJSON(ctx, route, &raw); e != nil {
		return CommentResult{}, e
	}
	if e := json.Unmarshal(raw, &v); e != nil {
		return CommentResult{}, e
	}
	n, trimmed := countComments(v.Data)
	part := trimmed || len(v.Data) >= 101 || (expected > 0 && n < expected)
	reason := ""
	if part {
		reason = "Boosty ограничил доступное количество комментариев или ответов"
	}
	return CommentResult{Raw: raw, Comments: v.Data, Partial: part, Expected: expected, Received: n, Reason: reason}, nil
}
func ExtractMedia(p *Post) []Media {
	out := make([]Media, 0)
	for _, raw := range p.Data {
		var m Media
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Type {
		case "ok_video", "audio_file", "file", "image":
			out = append(out, m)
		}
	}
	return out
}
func BestVideo(m Media) string {
	order := []string{"ultra_hd", "full_hd", "high", "medium", "low", "tiny"}
	for _, q := range order {
		for _, p := range m.PlayerURLs {
			if p.Type == q && p.URL != "" {
				return p.URL
			}
		}
	}
	for _, p := range m.PlayerURLs {
		if p.URL != "" {
			return p.URL
		}
	}
	return ""
}
func MediaURL(p *Post, m Media) string {
	raw := m.URL
	if m.Type == "ok_video" {
		raw = BestVideo(m)
	}
	if raw == "" {
		return ""
	}
	if (m.Type == "audio_file" || m.Type == "file") && p.SignedQuery != "" {
		u, e := url.Parse(raw)
		if e != nil {
			return ""
		}
		extra := strings.TrimPrefix(p.SignedQuery, "?")
		if u.RawQuery == "" {
			u.RawQuery = extra
		} else {
			u.RawQuery += "&" + extra
		}
		raw = u.String()
	}
	return raw
}
func MediaName(m Media, i int) string {
	ext := ".bin"
	switch m.Type {
	case "ok_video":
		ext = ".mp4"
	case "audio_file":
		ext = ".mp3"
	case "image":
		ext = ".jpg"
	case "file":
		ext = path.Ext(m.Title)
	}
	base := m.ID
	if base == "" {
		base = strconv.Itoa(i)
	}
	return safeFilename(base) + ext
}
func safeFilename(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}
func (c *Client) MediaGet(ctx context.Context, raw string, rangeHeader string) (*http.Response, error) {
	if err := validateMediaURL(raw); err != nil {
		return nil, err
	}
	req, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return c.media.Do(req)
}
