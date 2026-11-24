package boosty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestCommentsCursorPagination(t *testing.T) {
	for _, mode := range []string{"complete", "repeat", "cycle", "overlap", "http-error", "missing-replies", "unsafe-cursor", "empty-page"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				cursor := r.URL.Query().Get("offset")
				first := map[string]any{"data": []Comment{{ID: "1"}}, "extra": map[string]any{"isLast": false, "offset": "next&value=2"}}
				if mode == "unsafe-cursor" {
					first["extra"] = map[string]any{"isLast": false, "offset": "bad\nvalue"}
				}
				if cursor == "" {
					_ = json.NewEncoder(w).Encode(first)
					return
				}
				if cursor != "next&value=2" {
					t.Errorf("unexpected cursor: %q", cursor)
				}
				switch mode {
				case "repeat":
					_ = json.NewEncoder(w).Encode(first)
				case "cycle":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []Comment{{ID: "2"}}, "extra": map[string]any{"isLast": false, "offset": cursor}})
				case "overlap":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []Comment{{ID: "2"}, {ID: "1"}}, "extra": map[string]any{"isLast": true}})
				case "http-error":
					http.Error(w, "offline", 503)
				case "missing-replies":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []Comment{{ID: "2", ReplyCount: 3}}, "extra": map[string]any{"isLast": true}})
				case "empty-page":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []Comment{}, "extra": map[string]any{"isLast": false, "offset": "another"}})
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []Comment{{ID: "2"}}, "extra": map[string]any{"isLast": true}})
				}
			}))
			defer api.Close()
			client := New(Tokens{}, nil)
			client.api, client.apiBase = api.Client(), api.URL
			got, err := client.Comments(context.Background(), "author", "post", 2)
			wantCount := 1
			if mode == "complete" || mode == "cycle" || mode == "missing-replies" {
				wantCount = 2
			}
			if err != nil || got.Received != wantCount || got.Partial != (mode != "complete") {
				t.Fatalf("unexpected result: %+v %v", got, err)
			}
			wantRequests := 2
			if mode == "unsafe-cursor" {
				wantRequests = 1
			}
			if requests != wantRequests {
				t.Fatalf("requests=%d want=%d", requests, wantRequests)
			}
			if mode != "http-error" && mode != "unsafe-cursor" {
				var envelope struct {
					Pages []json.RawMessage `json:"pages"`
				}
				if err := json.Unmarshal(got.Raw, &envelope); err != nil || len(envelope.Pages) != 2 {
					t.Fatal("raw responses were not preserved")
				}
			}
		})
	}
}

func TestCommentsPaginationPageBudget(t *testing.T) {
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		id := strconv.Itoa(requests)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []Comment{{ID: id}}, "extra": map[string]any{"isLast": false, "offset": id}})
	}))
	defer api.Close()
	client := New(Tokens{}, nil)
	client.api, client.apiBase = api.Client(), api.URL
	got, err := client.Comments(context.Background(), "author", "post", 0)
	if err != nil || !got.Partial || requests != maxCommentPages || got.Received != maxCommentPages {
		t.Fatalf("unbounded or incorrect pagination: requests=%d result=%+v err=%v", requests, got, err)
	}
}

func TestCommentsDuplicateIDsNotCountedTwice(t *testing.T) {
	n, partial := countComments([]Comment{{ID: "one"}, {ID: "one"}})
	if n != 1 || !partial {
		t.Fatalf("duplicate inflated comment count: %d %v", n, partial)
	}
}
