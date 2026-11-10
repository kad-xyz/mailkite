package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/message"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultChars = 4000
	maxChars     = 20000
	notDownload  = "body: not downloaded by Mail (open it in Mail to fetch)"
)

type readInput struct {
	ID       int64 `json:"id" jsonschema:"message id from find"`
	Offset   int   `json:"offset,omitempty" jsonschema:"start char"`
	MaxChars int   `json:"max_chars,omitempty" jsonschema:"default 4000, max 20000"`
	Headers  bool  `json:"headers,omitempty" jsonschema:"all raw headers"`
}

// loaded is a message found in the index, with its raw bytes when downloaded.
type loaded struct {
	sm      maildata.Summary
	raw     []byte // nil when the body is not on disk
	partial bool
	emlx    string // path of the .emlx file
}

func (h *handlers) load(ctx context.Context, id int64) (*loaded, error) {
	st, err := h.store.get()
	if err != nil {
		return nil, err
	}
	sm, mb, err := st.Get(ctx, id)
	if errors.Is(err, maildata.ErrNotFound) {
		return nil, fmt.Errorf("message %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	l := &loaded{sm: sm}
	if p, partial, ok := st.EmlxPath(id, mb); ok {
		l.partial, l.emlx = partial, p
		raw, err := message.ReadEmlx(p)
		if err != nil {
			return nil, fmt.Errorf("cannot read message %d: %w", id, err)
		}
		l.raw = raw
	}
	return l, nil
}

func (h *handlers) addRead(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "read", Description: "Read one message: headers, codes, links, attachments, text.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
			l, err := h.load(ctx, in.ID)
			if err != nil {
				return errResult(err)
			}
			var m *message.Message
			if l.raw != nil {
				if m, err = message.Parse(l.raw); err != nil {
					return errResult(fmt.Errorf("cannot parse message %d: %w", in.ID, err))
				}
			}
			return textResult(renderMessage(l, m, in))
		})
}

func renderMessage(l *loaded, m *message.Message, in readInput) string {
	sm := l.sm
	var b strings.Builder
	line := func(k, v string) {
		if v = oneLine(v); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, clipRunes(v, 300))
		}
	}
	from, to, cc, subj := displayFrom(sm), "", "", sm.Subject
	date := sm.Date
	msgID := sm.MessageID
	if m != nil {
		if m.From != "" {
			from = m.From
		}
		to, cc = m.To, m.Cc
		if m.Subject != "" {
			subj = m.Subject
		}
		if !m.Date.IsZero() {
			date = m.Date
		}
		if m.MessageID != "" {
			msgID = m.MessageID
		}
	}
	line("From", from)
	line("To", to)
	line("Cc", cc)
	if !date.IsZero() {
		line("Date", date.Local().Format("2006-01-02 15:04"))
	}
	line("Subject", subj)
	if msgID != "" {
		fmt.Fprintf(&b, "open: %s\n", messageLink(msgID))
	}
	if in.Headers && l.raw != nil {
		hdr := l.raw
		if i := bytes.Index(hdr, []byte("\r\n\r\n")); i >= 0 {
			hdr = hdr[:i]
		} else if i := bytes.Index(hdr, []byte("\n\n")); i >= 0 {
			hdr = hdr[:i]
		}
		b.WriteString("headers:\n" + strings.ReplaceAll(string(hdr), "\r\n", "\n") + "\n")
	}
	if m == nil {
		b.WriteString(notDownload)
		return b.String()
	}
	if in.Offset <= 0 {
		ex := message.Extract(m, m.HTML())
		if len(ex.Codes) > 0 {
			b.WriteString("codes: " + strings.Join(ex.Codes, ", ") + "\n")
		}
		if len(ex.Links) > 0 {
			b.WriteString("links:\n")
			for _, ln := range ex.Links {
				text := oneLine(ln.Text)
				if ln.Kind != "other" {
					fmt.Fprintf(&b, "- [%s] %s → %s\n", ln.Kind, text, ln.URL)
				} else if d := domainOf(ln.URL); d != "" {
					fmt.Fprintf(&b, "- %s (%s)\n", text, d)
				} else {
					fmt.Fprintf(&b, "- %s\n", text)
				}
			}
		}
		if len(m.Parts) > 0 {
			b.WriteString("attachments:\n")
			for _, p := range m.Parts {
				fmt.Fprintf(&b, "%d. %s (%s, %s)\n", p.Index, oneLine(p.Name), p.ContentType, sizeKB(p.Size))
			}
		}
	}
	text := []rune(m.Text)
	if len(text) == 0 && len(m.Parts) == 0 && l.partial {
		b.WriteString(notDownload)
		return b.String()
	}
	n := in.MaxChars
	if n <= 0 {
		n = defaultChars
	}
	n = min(n, maxChars)
	off := min(max(in.Offset, 0), len(text))
	end := min(off+n, len(text))
	b.WriteString("\n")
	b.WriteString(string(text[off:end]))
	if end < len(text) {
		fmt.Fprintf(&b, "\n… more: offset=%d", end)
	}
	return b.String()
}

func displayFrom(sm maildata.Summary) string {
	if n := oneLine(sm.FromName); n != "" {
		return n + " <" + sm.From + ">"
	}
	return sm.From
}
