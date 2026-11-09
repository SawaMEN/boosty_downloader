package boosty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestSubscriptionsPaginateAndFilter(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/user/subscriptions" || r.URL.Query().Get("with_follow") != "true" || r.URL.Query().Get("limit") != "100" {
			t.Errorf("bad URL %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer test-account-token" {
			t.Error("missing bearer token")
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		list := make([]map[string]any, 0)
		for i := offset; i < offset+100 && i < 103; i++ {
			item := map[string]any{"blog": map[string]any{"blogUrl": fmt.Sprintf("author%03d", i), "title": fmt.Sprintf("Автор %d", i)}, "subscriptionLevel": map[string]any{"name": "Level"}, "price": 100, "isArchived": false}
			if i == 101 {
				item["isArchived"] = true
			}
			if i == 102 {
				item["blog"].(map[string]any)["blogUrl"] = "../../bad"
			}
			list = append(list, item)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": list, "total": 103, "limit": 100, "offset": offset})
	}))
	defer server.Close()
	c := New(Tokens{AccessToken: "test-account-token"}, nil)
	c.api = server.Client()
	c.apiBase = server.URL
	result, err := c.Subscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || result.Partial || len(result.Authors) != 101 || result.Total != 103 {
		t.Fatalf("pagination/filter mismatch: requests=%d result=%+v", requests, result)
	}
	if result.Authors[0].Blog != "author000" || !result.Authors[0].Paid {
		t.Errorf("bad first: %+v", result.Authors[0])
	}
}
func TestSubscriptionsOffsetIgnoredIsPartial(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		list := make([]map[string]any, 100)
		for i := range list {
			list[i] = map[string]any{"blog": map[string]any{"blogUrl": fmt.Sprintf("author%03d", i)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": list, "total": 150, "limit": 100, "offset": 0})
	}))
	defer server.Close()
	c := New(Tokens{AccessToken: "test"}, nil)
	c.api = server.Client()
	c.apiBase = server.URL
	result, err := c.Subscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial || count != 2 || len(result.Authors) != 100 {
		t.Errorf("offset not detected: %+v count=%d", result, count)
	}
}
func TestSubscriptionsRequiresAccount(t *testing.T) {
	c := New(Tokens{}, nil)
	if _, err := c.Subscriptions(context.Background()); err == nil {
		t.Fatal("unauthorized subscriptions")
	}
}
