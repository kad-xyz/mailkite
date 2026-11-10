package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
			dir := h.opts.DownloadDir
			if in.Dir != "" {
				if dir, err = resolveDir(in.Dir); err != nil {
					return errResult(err)
				}
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return errResult(err)
			}
			var paths []string
			for _, i := range idxs {
				name, data, err := message.PartData(l.raw, i)
				if err != nil {
					return errResult(fmt.Errorf("attachment %d: %w", i, err))
				}
				if len(data) == 0 && l.partial {
					if data, err = externalAttachment(l, m, i, name); err != nil {
						return errResult(err)
					}
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

// externalAttachment reads an attachment that Mail stores next to a .partial.emlx,
// at <Data dir>/Attachments/<rowid>/<part number>/<file name>.
func externalAttachment(l *loaded, m *message.Message, index int, name string) ([]byte, error) {
	notFound := errors.New("attachment not downloaded by Mail (open the message in Mail to fetch it)")
	// ordinal of this part among the parts sharing its name
	ord := 0
	for _, p := range m.Parts {
		if p.Index == index {
			break
		}
		if p.Name == name || sanitizeName(p.Name, p.Index) == sanitizeName(name, index) {
			ord++
		}
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(l.emlx)), "Attachments", strconv.FormatInt(l.sm.ID, 10))
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, notFound
	}
	type cand struct {
		num  int
		path string
	}
	var cands []cand
	want := map[string]bool{name: true, sanitizeName(name, index): true}
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.IsDir() && want[f.Name()] {
				cands = append(cands, cand{n, filepath.Join(root, e.Name(), f.Name())})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].num < cands[j].num })
	if ord >= len(cands) {
		return nil, notFound
	}
	data, err := os.ReadFile(cands[ord].path)
	if err != nil {
		return nil, notFound
	}
	return data, nil
}
