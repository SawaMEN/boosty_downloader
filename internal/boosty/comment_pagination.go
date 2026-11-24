package boosty

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

const (
	maxCommentPage      = 300
	maxCommentPages     = 16
	maxCommentRawBytes  = 24 << 20
	maxCommentCursorLen = 200
)

type commentPage struct {
	Data  []Comment        `json:"data"`
	Extra commentPageExtra `json:"extra"`
}

// Follow only explicit continuation hints on the same post endpoint. Some
// Boosty responses ignore offset; a duplicate or nonadvancing page must not
// be counted as new comments. Reply pagination remains unverified.
func (c *Client) Comments(ctx context.Context, author, id string, expected int) (CommentResult, error) {
	if !safeSegment(author) || !safeSegment(id) {
		return CommentResult{}, errors.New("недопустимый путь")
	}
	limit := 101
	if expected >= maxCommentPage {
		limit = maxCommentPage
	} else if expected >= limit {
		limit = expected + 1
	}
	route := "/v1/blog/" + url.PathEscape(author) + "/post/" + url.PathEscape(id) +
		"/comment/?limit=" + strconv.Itoa(limit) + "&reply_limit=100"
	var result CommentResult
	result.Expected = expected
	var pages []json.RawMessage
	rawBytes := 0
	seenCursors := make(map[string]bool)
	seenIDs := make(map[string]bool)
	nodes := 0
	cursor := ""
	reason := ""
	for len(pages) < maxCommentPages {
		endpoint := route
		if cursor != "" {
			endpoint += "&offset=" + url.QueryEscape(cursor)
		}
		var raw json.RawMessage
		if err := c.doJSON(ctx, endpoint, &raw); err != nil {
			if len(pages) == 0 || ctx.Err() != nil {
				return CommentResult{}, err
			}
			reason = "Не удалось получить следующую страницу комментариев"
			break
		}
		var page commentPage
		if err := json.Unmarshal(raw, &page); err != nil || page.Data == nil {
			if len(pages) == 0 {
				return CommentResult{}, errors.New("Boosty не вернул корректный массив комментариев")
			}
			reason = "Boosty вернул некорректную следующую страницу комментариев"
			break
		}
		if len(raw) > maxCommentRawBytes-rawBytes {
			reason = "Достигнут предел размера ответов комментариев"
			break
		}
		pages = append(pages, raw)
		rawBytes += len(raw)
		// Validate the entire page before accepting any new root, so a
		// repeated or overlapping response cannot inflate Received.
		pageIDs := make(map[string]bool)
		pageNodes := 0
		overlap := false
		truncated := WalkCommentTree(page.Data, func(node *Comment, _ int) {
			pageNodes++
			if node.ID == "" || seenIDs[node.ID] || pageIDs[node.ID] {
				overlap = true
			}
			pageIDs[node.ID] = true
		})
		if len(pages) > 1 && (overlap || truncated || nodes+pageNodes > MaxCommentTreeNodes) {
			reason = "Boosty вернул повторяющуюся или слишком большую страницу комментариев"
			break
		}
		result.Comments = append(result.Comments, page.Data...)
		for id := range pageIDs {
			seenIDs[id] = true
		}
		nodes += pageNodes
		if overlap {
			reason = "Boosty вернул повторяющиеся или неопознанные комментарии"
			break
		}
		if truncated || nodes >= MaxCommentTreeNodes {
			if truncated || commentPageHasMore(page.Extra) || ((len(pages) > 1 || len(page.Data) >= limit) && page.Extra.IsLast == nil) {
				reason = "Достигнут предел дерева комментариев"
			}
			break
		}
		if !commentPageHasMore(page.Extra) {
			if (len(pages) > 1 || len(page.Data) >= limit) && page.Extra.IsLast == nil {
				reason = "Boosty не подтвердил последнюю страницу комментариев"
			}
			break
		}
		next := page.Extra.Offset
		if len(page.Data) == 0 || next == "" || len(next) > maxCommentCursorLen ||
			strings.ContainsAny(next, "\r\n") || seenCursors[next] {
			reason = "Boosty не подтвердил последнюю страницу комментариев"
			break
		}
		if len(pages) == maxCommentPages {
			reason = "Достигнут предел страниц комментариев"
			break
		}
		seenCursors[next] = true
		cursor = next
	}
	if result.Comments == nil {
		result.Comments = []Comment{}
	}
	result.Received, result.Partial = countComments(result.Comments)
	switch {
	case reason != "":
		result.Partial, result.Reason = true, reason
	case result.Partial:
		result.Reason = "Boosty вернул не все ответы на комментарии"
	case expected > 0 && result.Received < expected:
		result.Partial = true
		result.Reason = "Boosty вернул меньше комментариев, чем указано в посте"
	}
	// Preserve the historical single-page raw format. Multiple pages
	// retain all successfully decoded responses, even a rejected repeat,
	// in a private forensic envelope (not part of the browser response).
	if len(pages) == 1 {
		result.Raw = pages[0]
	} else if len(pages) > 1 {
		raw, err := json.Marshal(struct {
			Pages []json.RawMessage `json:"pages"`
		}{Pages: pages})
		if err != nil {
			return CommentResult{}, err
		}
		result.Raw = raw
	}
	return result, nil
}
