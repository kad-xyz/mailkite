package server

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kad-xyz/mailkite/internal/fts"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type findInput struct {
	Query       string `json:"query,omitempty" jsonschema:"full-text search in subject and body"`
	Account     string `json:"account,omitempty" jsonschema:"account name"`
	Mailbox     string `json:"mailbox,omitempty" jsonschema:"mailbox name or inbox/sent/drafts/trash/junk/archive/all"`
	From        string `json:"from,omitempty" jsonschema:"sender name or address contains"`
	Subject     string `json:"subject,omitempty" jsonschema:"subject contains"`
	Since       string `json:"since,omitempty" jsonschema:"YYYY-MM-DD"`
	Until       string `json:"until,omitempty" jsonschema:"YYYY-MM-DD, inclusive"`
	Unread      bool   `json:"unread,omitempty" jsonschema:"only unread"`
	Flagged     bool   `json:"flagged,omitempty" jsonschema:"only flagged"`
	Attachments bool   `json:"attachments,omitempty" jsonschema:"only with attachments"`
	Limit       int    `json:"limit,omitempty" jsonschema:"default 20, max 200"`
	Offset      int    `json:"offset,omitempty" jsonschema:"skip this many"`
}

func truthy(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

func (h *handlers) addFind(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "find", Description: "Search messages; returns one line each.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in findInput) (*mcp.CallToolResult, any, error) {
			st, err := h.store.get()
			if err != nil {
				return errResult(err)
			}
			since, until, err := dateRange(in.Since, in.Until)
			if err != nil {
				return errResult(err)
			}
			f := maildata.Filter{
				Account: in.Account, From: in.From, Subject: in.Subject, Since: since, Until: until,
				Unread: truthy(in.Unread), Flagged: truthy(in.Flagged), HasAttachments: truthy(in.Attachments),
			}
			if in.Mailbox != "" {
				mailboxFilter(&f, in.Mailbox)
			}
			f.Skip = h.hiddenFilter(ctx, st, true)
			limit, offset := in.Limit, max(in.Offset, 0)
			if limit <= 0 {
				limit = 20
			}
			limit = min(limit, 200)

			var items []maildata.Summary
			more := false
			if strings.TrimSpace(in.Query) == "" {
				f.Limit, f.Offset = limit, offset
				if items, more, err = st.Find(ctx, f); err != nil {
					return errResult(err)
				}
			} else {
				ix, err := h.index.get()
				if err != nil {
					return errResult(err)
				}
				// another process holding the write lock is indexing: search what is there
				if _, _, err := ix.Sync(ctx, st); err != nil && !fts.IsBusy(err) {
					return errResult(err)
				}
				ids, err := ix.Search(ctx, in.Query, 500)
				if err != nil {
					return errResult(err)
				}
				if len(ids) > 0 {
					f.IDs = ids
					all, err := findAll(ctx, st, f)
					if err != nil {
						return errResult(err)
					}
					rank := make(map[int64]int, len(ids))
					for i, id := range ids {
						rank[id] = i
					}
					sort.SliceStable(all, func(i, j int) bool { return rank[all[i].ID] < rank[all[j].ID] })
					if offset < len(all) {
						end := min(offset+limit, len(all))
						items, more = all[offset:end], end < len(all)
					}
				}
			}
			if len(items) == 0 {
				return textResult("no messages")
			}
			var b strings.Builder
			for _, sm := range items {
				b.WriteString(summaryLine(sm))
				b.WriteByte('\n')
			}
			if more {
				fmt.Fprintf(&b, "more: offset=%d", offset+len(items))
			}
			return textResult(strings.TrimRight(b.String(), "\n"))
		})
}

// findAll pages through every match (the id list is at most 500 long).
func findAll(ctx context.Context, st *maildata.Store, f maildata.Filter) ([]maildata.Summary, error) {
	var out []maildata.Summary
	f.Limit = 200
	for f.Offset = 0; ; f.Offset += f.Limit {
		page, more, err := st.Find(ctx, f)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if !more {
			return out, nil
		}
	}
}
