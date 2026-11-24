package boosty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func replyExpansionFixture(count int) Comment {
	root := Comment{ID: "root", ReplyCount: 150}
	root.Replies.Data = make([]Comment, count)
	for i := range root.Replies.Data {
		root.Replies.Data[i].ID = strconv.Itoa(i + 1)
	}
	return root
}

func TestCommentReplyExpansion(t *testing.T) {
	for _, mode := range []string{"complete", "ignored", "changed", "duplicate", "error", "cursor"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				root := replyExpansionFixture(100)
				last := true
				switch r.URL.Query().Get("reply_limit") {
				case "100":
				case "300":
					switch mode {
					case "error":
						http.Error(w, "offline", 503)
						return
					case "ignored":
					default:
						root = replyExpansionFixture(150)
						switch mode {
						case "changed":
							root.Replies.Data[0].Author.Name = "changed"
						case "duplicate":
							root.Replies.Data[149].ID = root.Replies.Data[0].ID
						case "cursor":
							last = false
						}
					}
				default:
					t.Error("unexpected reply limit")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []Comment{root}, "extra": map[string]any{"isLast": last}})
			}))
			defer api.Close()
			client := New(Tokens{}, nil)
			client.api, client.apiBase = api.Client(), api.URL
			got, err := client.Comments(context.Background(), "author", "post", 151)
			want := 101
			if mode == "complete" {
				want = 151
			}
			if err != nil || got.Received != want || got.Partial != (mode != "complete") || requests.Load() != 2 {
				t.Fatalf("reply expansion: received=%d partial=%v requests=%d err=%v", got.Received, got.Partial, requests.Load(), err)
			}
			if mode != "error" {
				var envelope struct {
					Pages []json.RawMessage `json:"pages"`
				}
				if err := json.Unmarshal(got.Raw, &envelope); err != nil || len(envelope.Pages) != 2 {
					t.Fatal("reply retry raw responses missing")
				}
			}
		})
	}
}

func TestCommentReplyExpansionRejectsReorderedChildren(t *testing.T) {
	old := commentPage{Data: []Comment{replyExpansionFixture(100)}}
	next := commentPage{Data: []Comment{replyExpansionFixture(150)}}
	children := next.Data[0].Replies.Data
	children[0], children[1] = children[1], children[0]
	if safeCommentReplyExpansion(old, next) {
		t.Fatal("reordered reply history accepted")
	}
}
