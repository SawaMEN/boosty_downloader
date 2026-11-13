package boosty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCommentsLargerSinglePageWhenExpectedExceedsDefault(t *testing.T) {
	const count = 205
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "206" || r.URL.Query().Get("reply_limit") != "100" {
			t.Errorf("wrong bounded comment request: %s", r.URL.RawQuery)
		}
		data := make([]Comment, count)
		for i := range data {
			data[i].ID = strconv.Itoa(i + 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "extra": map[string]any{"isLast": true}})
	}))
	defer api.Close()
	client := New(Tokens{}, nil)
	client.api = api.Client()
	client.apiBase = api.URL
	result, err := client.Comments(context.Background(), "author", "post", count)
	if err != nil || result.Received != count || result.Partial {
		t.Fatalf("expected complete 205-comment response: %+v, %v", result, err)
	}
	if len(result.Raw) == 0 {
		t.Fatal("raw API response missing from forensic archive")
	}
}

func TestCommentsSinglePageIsBoundedAndIncomplete(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "300" {
			t.Errorf("did not cap request at 300: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "first"}}})
	}))
	defer api.Close()
	client := New(Tokens{}, nil)
	client.api = api.Client()
	client.apiBase = api.URL
	result, err := client.Comments(context.Background(), "author", "post", 500)
	if err != nil || !result.Partial || result.Received != 1 {
		t.Fatalf("missing comments mistakenly marked complete: %+v, %v", result, err)
	}
	if !strings.Contains(result.Reason, "меньше") {
		t.Fatalf("missing incomplete reason: %q", result.Reason)
	}
}

func TestCommentsDeletionStubsDoNotCountAsLive(t *testing.T) {
	data := []Comment{
		{ID: "deleted", IsDeleted: true},
		{ID: "thread", ReplyCount: 1},
	}
	data[1].Replies.Data = []Comment{
		{ID: "reply-deleted", IsDeleted: true},
		{ID: "reply-live"},
	}
	count, partial := countComments(data)
	if count != 2 || partial {
		t.Fatalf("deleted stubs distorted live-count or completeness: count=%d partial=%v", count, partial)
	}
	data[1].ReplyCount = 2
	if _, partial := countComments(data); !partial {
		t.Fatal("missing live reply not reported")
	}
}

func TestCommentsDetectServerCursorAndMissingReplies(t *testing.T) {
	testCases := []struct {
		name         string
		page         any
		wantPartial  bool
		wantReceived int
	}{
		{
			name: "explicit server has more", wantPartial: true, wantReceived: 1,
			page: map[string]any{"data": []any{map[string]any{"id": "1"}}, "extra": map[string]any{"isLast": false}},
		},
		{
			name: "cursor without last-page flag", wantPartial: true, wantReceived: 1,
			page: map[string]any{"data": []any{map[string]any{"id": "1"}}, "extra": map[string]any{"offset": "1654884900:923396"}},
		},
		{
			name: "deleted root not counted", wantPartial: false, wantReceived: 1,
			page: map[string]any{"data": []any{
				map[string]any{"id": "d", "isDeleted": true},
				map[string]any{"id": "1"},
			}, "extra": map[string]any{"isLast": true}},
		},
		{
			name: "reply continuation", wantPartial: true, wantReceived: 1,
			page: map[string]any{"data": []any{map[string]any{
				"id": "1", "replyCount": 0,
				"replies": map[string]any{"data": []any{}, "extra": map[string]any{"isLast": false}},
			}}},
		},
		{
			name: "reply count beyond loaded", wantPartial: true, wantReceived: 1,
			page: map[string]any{"data": []any{map[string]any{"id": "1", "replyCount": 3, "replies": map[string]any{"data": []any{}}}}},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.page)
			}))
			defer api.Close()
			client := New(Tokens{}, nil)
			client.api = api.Client()
			client.apiBase = api.URL
			result, err := client.Comments(context.Background(), "author", "post", 0)
			if err != nil || result.Partial != tc.wantPartial || result.Received != tc.wantReceived {
				t.Fatalf("incorrect response classification: %+v, %v", result, err)
			}
		})
	}
}

func TestCommentsRejectsMissingArrayOrUnsafePath(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null}`} {
		t.Run(body, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer api.Close()
			client := New(Tokens{}, nil)
			client.api = api.Client()
			client.apiBase = api.URL
			if _, err := client.Comments(context.Background(), "author", "post", 0); err == nil {
				t.Fatalf("missing comment array incorrectly recorded as zero: %s", body)
			}
		})
	}
	client := New(Tokens{}, nil)
	if _, err := client.Comments(context.Background(), "..", "post", 0); err == nil {
		t.Fatal("unsafe author path accepted")
	}
}
