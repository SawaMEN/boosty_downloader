package boosty

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostURL(t *testing.T) {
	good := []string{"https://boosty.to/author/posts/1234-uuid", "https://www.boosty.to/user.name/posts/hello_123"}
	for _, raw := range good {
		a, id, e := PostURL(raw)
		if e != nil || a == "" || id == "" {
			t.Fatalf("valid %s: %v", raw, e)
		}
	}
	bad := []string{"http://boosty.to/user/posts/1", "https://boosty.to.evil.org/user/posts/1", "https://boosty.to/../../etc/passwd", "https://boosty.to/user/other/1", "https://127.0.0.1/user/posts/1", "https://boosty.to:8443/a/posts/1", "https://boosty.to/a/posts/%2e%2e"}
	for _, raw := range bad {
		if _, _, e := PostURL(raw); e == nil {
			t.Errorf("accepted dangerous URL: %s", raw)
		}
	}
}
func TestIPGuard(t *testing.T) {
	bad := []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::123"}
	for _, s := range bad {
		if permittedIP(net.ParseIP(s)) {
			t.Errorf("allowed %s", s)
		}
	}
	if !permittedIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("rejected public IP")
	}
}
func TestMediaSelector(t *testing.T) {
	p := &Post{SignedQuery: "?signature=xyz"}
	m := Media{ID: "123", Type: "ok_video", PlayerURLs: []PlayerURL{{Type: "low", URL: "https://cdn.example/low"}, {Type: "full_hd", URL: "https://cdn.example/high"}}}
	if got := MediaURL(p, m); got != "https://cdn.example/high" {
		t.Fatal(got)
	}
	if got := MediaURL(p, Media{Type: "file", URL: "https://cdn.example/file"}); !strings.Contains(got, "signature=xyz") {
		t.Fatal(got)
	}
}
func TestCommentsDetectTruncation(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reply_limit") != "100" {
			t.Error("reply_limit not set")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "1", "replyCount": 5, "replies": map[string]any{"data": []any{}}}}})
	}))
	defer api.Close()
	client := New(Tokens{}, nil)
	client.api = api.Client()
	client.apiBase = api.URL
	result, e := client.Comments(context.Background(), "author", "post", 6)
	if e != nil {
		t.Fatal(e)
	}
	if !result.Partial || result.Received != 1 {
		t.Fatalf("should be partial: %+v", result)
	}
}
