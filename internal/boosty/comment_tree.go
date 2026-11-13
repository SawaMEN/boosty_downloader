package boosty

// Comment trees returned by an undocumented API may be unexpectedly deep or
// wide. The archive keeps the raw response, but rendering and counting must
// use the same bounded walk so a partial preview is never reported complete.
const (
	MaxCommentTreeDepth = 64
	MaxCommentTreeNodes = 10000
)

// WalkCommentTree visits comments in preorder, including deleted stubs so
// their live replies retain their position. Depth starts at zero. It returns
// true if some nodes were omitted by the explicit depth/node safety limits.
// A stack of slice cursors avoids recursion and is bounded by max depth.
func WalkCommentTree(comments []Comment, visit func(*Comment, int)) (truncated bool) {
	type frame struct {
		items []Comment
		index int
		depth int
	}
	stack := []frame{{items: comments}}
	visited := 0
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.index >= len(top.items) {
			stack = stack[:len(stack)-1]
			continue
		}
		if visited >= MaxCommentTreeNodes {
			return true
		}
		node := &top.items[top.index]
		depth := top.depth
		top.index++
		visited++
		visit(node, depth)
		if len(node.Replies.Data) != 0 {
			if depth+1 >= MaxCommentTreeDepth {
				truncated = true
			} else {
				stack = append(stack, frame{items: node.Replies.Data, depth: depth + 1})
			}
		}
	}
	return truncated
}
