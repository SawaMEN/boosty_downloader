package boosty

import "reflect"

// Only retry when the existing 100-reply request plausibly clipped a
// thread. Small gaps can reflect hidden/deleted replies rather than a limit.
func needsCommentReplyExpansion(comments []Comment) bool {
	needed := false
	truncated := WalkCommentTree(comments, func(node *Comment, _ int) {
		if (node.ReplyCount > 100 || len(node.Replies.Data) >= 100) &&
			(node.ReplyCount > len(node.Replies.Data) || commentPageHasMore(node.Replies.Extra) || len(node.Replies.Data) >= 100) {
			needed = true
		}
	})
	return needed && !truncated
}

// Only replace a page when it is a strictly larger, consistent expansion
// of the original tree. Reject changed roots, reordered/missing old
// children, duplicates, changed content and changed outer page cursor.
func safeCommentReplyExpansion(old, next commentPage) bool {
	if len(old.Data) != len(next.Data) || !reflect.DeepEqual(old.Extra, next.Extra) {
		return false
	}
	inventory := func(comments []Comment) (int, bool) {
		seen := make(map[string]bool)
		valid := true
		n := 0
		truncated := WalkCommentTree(comments, func(node *Comment, _ int) {
			n++
			if node.ID == "" || seen[node.ID] {
				valid = false
			}
			seen[node.ID] = true
		})
		return n, valid && !truncated
	}
	before, beforeOK := inventory(old.Data)
	after, afterOK := inventory(next.Data)
	if !beforeOK || !afterOK || after <= before {
		return false
	}
	// Both trees are already bounded by MaxCommentTreeDepth and
	// MaxCommentTreeNodes. Existing children must remain a prefix.
	var preserves func(Comment, Comment) bool
	preserves = func(a, b Comment) bool {
		childrenA, childrenB := a.Replies.Data, b.Replies.Data
		a.Replies = b.Replies
		if !reflect.DeepEqual(a, b) || len(childrenA) > len(childrenB) {
			return false
		}
		for i := range childrenA {
			if !preserves(childrenA[i], childrenB[i]) {
				return false
			}
		}
		return true
	}
	for i := range old.Data {
		if !preserves(old.Data[i], next.Data[i]) {
			return false
		}
	}
	return true
}
