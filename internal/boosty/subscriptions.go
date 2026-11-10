package boosty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// SubscriptionAuthor is a safe-to-expose subset of the account's Boosty subscriptions.
// The Boosty API is unofficial; no signed media or authentication URLs are returned.
type SubscriptionAuthor struct {
	Blog    string `json:"blog"`
	Title   string `json:"title"`
	Level   string `json:"level,omitempty"`
	LevelID string `json:"levelId,omitempty"`
	Price   int64  `json:"price,omitempty"`
	Paid    bool   `json:"paid"`
	Paused  bool   `json:"paused"`
}

type SubscriptionList struct {
	Authors []SubscriptionAuthor `json:"authors"`
	Total   int                  `json:"total"`
	Partial bool                 `json:"partial"`
}

const subscriptionsPageSize = 100
const subscriptionsMaxPages = 10 // Defensive upper bound for an unofficial API.

// Subscriptions retrieves both paid and free followed authors. The active
// subscriptions list is the sole authority for which authors the import wizard
// offers; the server validates selections again before queuing work.
func (c *Client) Subscriptions(ctx context.Context) (SubscriptionList, error) {
	if !c.HasToken() {
		return SubscriptionList{}, errors.New("сначала подключите аккаунт Boosty")
	}
	found := map[string]SubscriptionAuthor{}
	expected := 0
	partial := false
	for page := 0; page < subscriptionsMaxPages; page++ {
		offset := page * subscriptionsPageSize
		route := "/v1/user/subscriptions?limit=" + strconv.Itoa(subscriptionsPageSize) + "&with_follow=true&offset=" + strconv.Itoa(offset)
		var result struct {
			Data []struct {
				Blog struct {
					BlogURL string `json:"blogUrl"`
					Title   string `json:"title"`
				} `json:"blog"`
				Level struct {
					Name string          `json:"name"`
					ID   json.RawMessage `json:"id"`
				} `json:"subscriptionLevel"`
				Price       int64 `json:"price"`
				IsArchived  bool  `json:"isArchived"`
				IsSuspended bool  `json:"isSuspended"`
				IsPause     bool  `json:"isPause"`
			} `json:"data"`
			Total  int `json:"total"`
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := c.doJSON(ctx, route, &result); err != nil {
			if page > 0 {
				return SubscriptionList{}, fmt.Errorf("страница %d списка подписок: %w", page+1, err)
			}
			return SubscriptionList{}, err
		}
		if page == 0 {
			expected = result.Total
		} else if result.Offset != offset {
			// Some API versions ignore offset. Never present repeated first-page
			// results as a complete list.
			partial = true
			break
		}
		for _, item := range result.Data {
			slug := item.Blog.BlogURL
			if !ValidBlog(slug) || item.IsArchived || item.IsSuspended {
				continue
			}
			if _, ok := found[slug]; ok {
				continue
			}
			title := item.Blog.Title
			if title == "" {
				title = slug
			}
			levelID := strings.Trim(string(item.Level.ID), "\"")
			if len(levelID) > 80 || (levelID != "" && !safeSegment(levelID)) {
				levelID = ""
			}
			levelName := []rune(item.Level.Name)
			if len(levelName) > 128 {
				levelName = levelName[:128]
			}
			price := item.Price
			if price < 0 {
				price = 0
			}
			found[slug] = SubscriptionAuthor{
				Blog: slug, Title: title, Level: string(levelName),
				LevelID: levelID, Price: price,
				Paid: price > 0, Paused: item.IsPause,
			}
		}
		if len(result.Data) < subscriptionsPageSize || (result.Total > 0 && offset+len(result.Data) >= result.Total) {
			break
		}
		if page == subscriptionsMaxPages-1 {
			partial = true
		}
	}
	out := make([]SubscriptionAuthor, 0, len(found))
	for _, author := range found {
		out = append(out, author)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Blog < out[j].Blog })
	if expected > subscriptionsPageSize*subscriptionsMaxPages {
		partial = true
	}
	return SubscriptionList{Authors: out, Total: expected, Partial: partial}, nil
}

// PostURLFor builds a trusted post URL from an API-provided author and ID.
func PostURLFor(author, id string) string {
	return "https://boosty.to/" + url.PathEscape(author) + "/posts/" + url.PathEscape(id)
}
