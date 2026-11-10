package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kad-xyz/mailkite/internal/message"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type saveInput struct {
	ID    int64  `json:"id" jsonschema:"message id"`
	Index *int   `json:"index,omitempty" jsonschema:"attachment number; omit for all"`
	Dir   string `json:"dir,omitempty" jsonschema:"default ~/Downloads"`
}

func (h *handlers) addSaveAttachment(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "save_attachment", Description: "Save attachments to disk without overwriting.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in saveInput) (*mcp.CallToolResult, any, error) {
			l, err := h.load(ctx, in.ID)
			if err != nil {
				return errResult(err)
			}
			if l.raw == nil {
				return errResult(errors.New("message body is not downloaded by Mail (open it in Mail to fetch)"))
			}
			m, err := message.Parse(l.raw)
			if err != nil {
				return errResult(err)
			}
			var idxs []int
			if in.Index != nil {
				idxs = []int{*in.Index}
			} else {
				for _, p := range m.Parts {
					idxs = append(idxs, p.Index)
				}
			}
			if len(idxs) == 0 {
				return errResult(errors.New("message has no attachments"))
			}
			dir := in.Dir
			if dir == "" {
				dir = h.opts.DownloadDir
			}
			if strings.HasPrefix(dir, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					dir = filepath.Join(home, dir[2:])
				}
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return errResult(err)
			}
			var paths []string
			for _, i := range idxs {
				name, data, err := message.PartData(l.raw, i)
				if err != nil {
					return errResult(fmt.Errorf("attachment %d: %w", i, err))
				}
				p, err := writeUnique(dir, sanitizeName(name, i), data)
				if err != nil {
					return errResult(err)
				}
				paths = append(paths, p)
			}
			return textResult(strings.Join(paths, "\n"))
		})
}

// writeUnique creates dir/name (or "name (2).ext", ...) exclusively with mode 0600.
func writeUnique(dir, name string, data []byte) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 1; ; n++ {
		cand := name
		if n > 1 {
			cand = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		p := filepath.Join(dir, cand)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(p)
			return "", err
		}
		return p, f.Close()
	}
}
