package boosty

import (
	"strconv"
	"testing"
)

func nestedCommentChain(depth int) []Comment {
	var children []Comment
	for i := depth - 1; i >= 0; i-- {
		node := Comment{ID: strconv.Itoa(i)}
		node.Replies.Data = children
		if len(children) != 0 {
			node.ReplyCount = 1
		}
		children = []Comment{node}
	}
	return children
}

func TestCommentTreeDeepWalkIsBoundedAndMarksIncomplete(t *testing.T) {
	comments := nestedCommentChain(MaxCommentTreeDepth + 50)
	seen := make([]string, 0)
	truncated := WalkCommentTree(comments, func(c *Comment, depth int) {
		seen = append(seen, c.ID)
		if depth != len(seen)-1 {
			t.Fatalf("unexpected comment depth %d at node %d", depth, len(seen))
		}
	})
	n, incomplete := countComments(comments)
	if !truncated || !incomplete || len(seen) != MaxCommentTreeDepth || n != MaxCommentTreeDepth {
		t.Fatalf("deep discussion silently counted as complete: seen=%d, count=%d, partial=%v", len(seen), n, incomplete)
	}
	if seen[0] != "0" || seen[len(seen)-1] != strconv.Itoa(MaxCommentTreeDepth-1) {
		t.Fatal("deep comment order changed")
	}
}

func TestCommentTreeWideWalkIsBoundedAndMarksIncomplete(t *testing.T) {
	comments := make([]Comment, MaxCommentTreeNodes+1)
	for i := range comments {
		comments[i].ID = strconv.Itoa(i)
	}
	seen := 0
	truncated := WalkCommentTree(comments, func(c *Comment, depth int) {
		if depth != 0 || c.ID != strconv.Itoa(seen) {
			t.Fatal("wide comment order changed")
		}
		seen++
	})
	n, partial := countComments(comments)
	if !truncated || !partial || seen != MaxCommentTreeNodes || n != MaxCommentTreeNodes {
		t.Fatalf("wide page was not bounded: %d %d %v", seen, n, partial)
	}
}

func TestCommentTreeDeletedStubsKeepRepliesAndPreorder(t *testing.T) {
	parent := Comment{ID: "deleted-parent", IsDeleted: true}
	parent.Replies.Data = []Comment{{ID: "live-one"}, {ID: "live-two"}}
	parent.ReplyCount = 2
	comments := []Comment{parent, {ID: "last"}}
	var visited []string
	truncated := WalkCommentTree(comments, func(c *Comment, depth int) {
		visited = append(visited, c.ID+"@"+strconv.Itoa(depth))
	})
	n, partial := countComments(comments)
	if truncated || partial || n != 3 || len(visited) != 4 {
		t.Fatalf("deleted reply container lost nodes: count=%d partial=%v path=%v", n, partial, visited)
	}
	want := []string{"deleted-parent@0", "live-one@1", "live-two@1", "last@0"}
	for i := range want {
		if visited[i] != want[i] {
			t.Fatalf("preorder changed: got %v, want %v", visited, want)
		}
	}
}

func TestCommentTreePreciselyAtLimitIsComplete(t *testing.T) {
	comments := make([]Comment, MaxCommentTreeNodes)
	n, partial := countComments(comments)
	if partial || n != MaxCommentTreeNodes {
		t.Fatalf("tree exactly at the limit should be complete: %d %v", n, partial)
	}
}
