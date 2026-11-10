package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type sendersInput struct {
	Account  string `json:"account,omitempty" jsonschema:"account name"`
	Mailbox  string `json:"mailbox,omitempty" jsonschema:"mailbox name or role"`
	Since    string `json:"since,omitempty" jsonschema:"YYYY-MM-DD"`
	MinCount int    `json:"min_count,omitempty" jsonschema:"default 2"`
	Limit    int    `json:"limit,omitempty" jsonschema:"default 50"`
}

func (h *handlers) addSenders(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "senders", Description: "Top senders with counts.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in sendersInput) (*mcp.CallToolResult, any, error) {
			st, err := h.store.get()
			if err != nil {
				return errResult(err)
			}
			since, _, err := dateRange(in.Since, "")
			if err != nil {
				return errResult(err)
			}
			f := maildata.Filter{Account: in.Account, Since: since}
			if in.Mailbox != "" {
				mailboxFilter(&f, in.Mailbox)
			}
			f.Skip = h.hiddenFilter(ctx, st, false)
			minCount, limit := in.MinCount, in.Limit
			if minCount <= 0 {
				minCount = 2
			}
			if limit <= 0 {
				limit = 50
			}
			groups, err := st.Senders(ctx, f, minCount)
			if err != nil {
				return errResult(err)
			}
			if len(groups) > limit {
				groups = groups[:limit]
			}
			if len(groups) == 0 {
				return textResult("no senders")
			}
			var b strings.Builder
			for i, g := range groups {
				if i > 0 {
					b.WriteByte('\n')
				}
				who := g.Address
				if n := oneLine(g.Name); n != "" {
					who = n + " " + g.Address
				}
				fmt.Fprintf(&b, "%d (%d unread) | %s | last %s", g.Count, g.Unread, who, g.Last.Local().Format("02.01"))
				if g.ListID {
					b.WriteString(" | list")
				}
				if g.Unsub > 0 {
					b.WriteString(" | unsub")
				}
			}
			return textResult(b.String())
		})
}
