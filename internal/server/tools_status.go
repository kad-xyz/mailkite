package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type noInput struct{}

func (h *handlers) addStatus(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "status", Description: "Mail accounts, counts and index state.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
			st, err := h.store.get()
			if err != nil {
				return errResult(err)
			}
			accs, err := st.Accounts(ctx)
			if err != nil {
				return errResult(err)
			}
			var b strings.Builder
			for _, a := range accs {
				fmt.Fprintf(&b, "account %s (%s): %d msgs, %d unread\n", a.Name, a.Kind, a.Messages, a.Unread)
			}
			if t := st.IndexModTime(); !t.IsZero() {
				fmt.Fprintf(&b, "mail index updated %s\n", t.Local().Format("2006-01-02 15:04"))
			}
			if n, err := h.indexCount(ctx); err != nil {
				fmt.Fprintf(&b, "search index: unavailable (%s)", shortErr(err))
			} else {
				fmt.Fprintf(&b, "search index: %d docs", n)
			}
			return textResult(b.String())
		})
}

func (h *handlers) indexCount(ctx context.Context) (int, error) {
	ix, err := h.index.get()
	if err != nil {
		return 0, err
	}
	return ix.Count(ctx)
}

// shortErr is the innermost cause of err (after the last ": "), first line only, at most 80 bytes.
func shortErr(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	if len(s) > 80 {
		s = strings.ToValidUTF8(s[:80], "") + "..."
	}
	return s
}
