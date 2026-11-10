// Package server exposes the mail store as read-only MCP tools.
package server

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/kad-xyz/mailkite/internal/fts"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options configures the server.
type Options struct {
	DownloadDir string // where save_attachment writes; default ~/Downloads
}

// lazy opens a resource on first use and retries after a failure.
type lazy[T any] struct {
	mu   sync.Mutex
	open func() (T, error)
	val  T
	done bool
}

func (l *lazy[T]) get() (T, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return l.val, nil
	}
	v, err := l.open()
	if err != nil {
		var zero T
		return zero, err // not cached: access may be granted later
	}
	l.val, l.done = v, true
	return v, nil
}

func (l *lazy[T]) peek() (v T, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.val, l.done
}

type handlers struct {
	store *lazy[*maildata.Store]
	index *lazy[*fts.Index]
	opts  Options
	reads readCache
}

// Close releases whatever has been opened.
func (h *handlers) close() {
	if st, ok := h.store.peek(); ok {
		st.Close()
	}
	if ix, ok := h.index.peek(); ok {
		ix.Close()
	}
}

// New builds the server over an already opened store and index.
func New(st *maildata.Store, idx *fts.Index, opts Options) *mcp.Server {
	s, _ := build(&lazy[*maildata.Store]{open: func() (*maildata.Store, error) { return st, nil }},
		&lazy[*fts.Index]{open: func() (*fts.Index, error) { return idx, nil }}, opts)
	return s
}

// NewLazy builds the server; the store and index are opened on the first tool call
// (and again after a failure). The returned func closes whatever was opened.
func NewLazy(openStore func() (*maildata.Store, error), openIndex func() (*fts.Index, error), opts Options) (*mcp.Server, func()) {
	return build(&lazy[*maildata.Store]{open: openStore}, &lazy[*fts.Index]{open: openIndex}, opts)
}

func build(st *lazy[*maildata.Store], ix *lazy[*fts.Index], opts Options) (*mcp.Server, func()) {
	if opts.DownloadDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			opts.DownloadDir = filepath.Join(home, "Downloads")
		}
	}
	h := &handlers{store: st, index: ix, opts: opts}
	srv := mcp.NewServer(&mcp.Implementation{Name: "mailkite", Version: "dev"}, nil)
	h.addStatus(srv)
	h.addFind(srv)
	h.addRead(srv)
	h.addSaveAttachment(srv)
	h.addSenders(srv)
	return srv, h.close
}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true}

func textResult(s string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}, nil, nil
}

func errResult(err error) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
}
