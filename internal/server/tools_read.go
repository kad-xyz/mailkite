package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

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
	mb      maildata.Mailbox
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

// parsed is what read needs from one .emlx file.
type parsed struct {
	m   *message.Message
	ex  func() message.Extracted // codes and links, computed on the first page only
	hdr string                   // raw header block, CRLF turned into LF
}

// readCache keeps the last parsed message, so paging through it does not parse it again.
// It is keyed by the file and its size and mtime, so a changed file is parsed afresh.
type readCache struct {
	mu    sync.Mutex
	path  string
	size  int64
	mtime time.Time
	val   *parsed
}

func (c *readCache) get(path string, fi os.FileInfo) *parsed {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.val != nil && c.path == path && c.size == fi.Size() && c.mtime.Equal(fi.ModTime()) {
		return c.val
	}
	return nil
}

func (c *readCache) put(path string, fi os.FileInfo, v *parsed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.size, c.mtime, c.val = path, fi.Size(), fi.ModTime(), v
}

func headerBlock(raw []byte) string {
	hdr := raw
	if i := bytes.Index(hdr, []byte("\r\n\r\n")); i >= 0 {
		hdr = hdr[:i]
	} else if i := bytes.Index(hdr, []byte("\n\n")); i >= 0 {
		hdr = hdr[:i]
	}
	return strings.ReplaceAll(string(hdr), "\r\n", "\n")
}

// readMessage returns the parsed message for read; nil when the body is not on disk.
func (h *handlers) readMessage(ctx context.Context, id int64) (*loaded, *parsed, error) {
	st, err := h.store.get()
	if err != nil {
		return nil, nil, err
	}
	sm, mb, err := st.Get(ctx, id)
	if errors.Is(err, maildata.ErrNotFound) {
		return nil, nil, fmt.Errorf("message %d not found", id)
	}
	if err != nil {
		return nil, nil, err
	}
	if skip := h.hiddenFilter(ctx, st, false); skip != nil && skip(sm) {
		return nil, nil, fmt.Errorf("message %d not found", id)
	}
	l := &loaded{sm: sm, mb: mb}
	p, partial, ok := st.EmlxPath(id, mb)
	if !ok {
		return l, h.fetchOverIMAP(ctx, l), nil
	}
	l.partial, l.emlx = partial, p
	v, err := h.parseEmlx(p, id)
	if err != nil {
		return nil, nil, err
	}
	if l.partial && len(v.m.Text) == 0 && len(v.m.Parts) == 0 {
		if fv := h.fetchOverIMAP(ctx, l); fv != nil {
			return l, fv, nil
		}
	}
	return l, v, nil
}

// parseEmlx parses the .emlx file at p, through the one-message cache.
func (h *handlers) parseEmlx(p string, id int64) (*parsed, error) {
	fi, statErr := os.Stat(p)
	if statErr == nil {
		if v := h.reads.get(p, fi); v != nil {
			return v, nil
		}
	}
	raw, err := message.ReadEmlx(p)
	if err != nil {
		return nil, fmt.Errorf("cannot read message %d: %w", id, err)
	}
	m, err := message.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot parse message %d: %w", id, err)
	}
	v := newParsed(m, raw)
	if statErr == nil {
		h.reads.put(p, fi, v)
	}
	return v, nil
}

func newParsed(m *message.Message, raw []byte) *parsed {
	return &parsed{m: m, ex: sync.OnceValue(func() message.Extracted { return message.Extract(m, m.HTML()) }), hdr: headerBlock(raw)}
}

// fetchOverIMAP reads a body Mail has not downloaded from the account's IMAP server
// (BODY.PEEK[], so it stays unread). Any failure returns nil: read then says
// "not downloaded". The result is not cached on disk or in memory.
func (h *handlers) fetchOverIMAP(ctx context.Context, l *loaded) *parsed {
	eng, err := h.engine()
	if err != nil || len(eng.Cfg.Accounts) == 0 {
		return nil
	}
	raw, err := eng.FetchBody(ctx, l.sm, l.mb)
	if err != nil {
		return nil
	}
	m, err := message.Parse(raw)
	if err != nil {
		return nil
	}
	return newParsed(m, raw)
}

func (h *handlers) addRead(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "read", Description: "Read one message: headers, codes, links, attachments, text.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
			l, v, err := h.readMessage(ctx, in.ID)
			if err != nil {
				return errResult(err)
			}
			return textResult(renderMessage(l, v, in))
		})
}

func renderMessage(l *loaded, v *parsed, in readInput) string {
	sm := l.sm
	var m *message.Message
	if v != nil {
		m = v.m
	}
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
	if in.Headers && v != nil {
		b.WriteString("headers:\n" + v.hdr + "\n")
	}
	if m == nil {
		b.WriteString(notDownload)
		return b.String()
	}
	if in.Offset <= 0 {
		ex := v.ex()
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
