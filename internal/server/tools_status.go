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
			ix, err := h.index.get()
			if err != nil {
				return errResult(err)
			}
			n, err := ix.Count(ctx)
			if err != nil {
				return errResult(err)
			}
			fmt.Fprintf(&b, "search index: %d docs", n)
			return textResult(b.String())
		})
}
