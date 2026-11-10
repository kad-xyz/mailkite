package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/imapx"
	"github.com/kad-xyz/mailkite/internal/safety"
	"github.com/kad-xyz/mailkite/internal/secret"
)

// appender puts a raw message back into a server mailbox.
type appender interface {
	Append(ctx context.Context, mailbox string, raw []byte, seen bool) error
	Close() error
}

type restoreDeps struct {
	cfg  *config.Config
	dial func(ctx context.Context, a config.Account) (appender, error)
	open func(ctx context.Context, path string) error // hands a file to Mail (POP accounts)
	out  io.Writer
}

func realRestoreDeps(cfg *config.Config, out io.Writer) restoreDeps {
	return restoreDeps{
		cfg: cfg,
		dial: func(ctx context.Context, a config.Account) (appender, error) {
			pw, err := secret.Keychain{}.Get(a.Email)
			if err != nil {
				return nil, err
			}
			return imapx.Dial(ctx, a, pw)
		},
		open: func(ctx context.Context, path string) error {
			return exec.CommandContext(ctx, "/usr/bin/open", path).Run()
		},
		out: out,
	}
}

// restoreFiles lists the .eml files to restore: path itself, or the .eml files of a
// day directory in name order.
func restoreFiles(path string) ([]string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		if filepath.Ext(abs) != ".eml" {
			return nil, fmt.Errorf("%s: not an .eml file", path)
		}
		return []string{abs}, nil
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range ents {
		if e.Type().IsRegular() && filepath.Ext(e.Name()) == ".eml" && !strings.HasPrefix(e.Name(), ".") {
			files = append(files, filepath.Join(abs, e.Name()))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: no .eml files", path)
	}
	sort.Strings(files)
	return files, nil
}

// runRestore is `mailkite restore <path>`: archived messages go back to their
// original mailbox (IMAP) or are opened in Mail (POP). Archive files are kept.
func runRestore(ctx context.Context, d restoreDeps, path string) error {
	files, err := restoreFiles(path)
	if err != nil {
		return err
	}
	conns := map[string]appender{}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	var errs []error
	for _, f := range files {
		msg, err := restoreOne(ctx, d, conns, f)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			fmt.Fprintf(d.out, "failed   %s: %v\n", f, err)
			continue
		}
		fmt.Fprintln(d.out, msg)
	}
	return errors.Join(errs...)
}

func restoreOne(ctx context.Context, d restoreDeps, conns map[string]appender, f string) (string, error) {
	sc, err := safety.ReadSidecar(f)
	if err != nil {
		return "", err
	}
	acc, ok := d.cfg.ByMailUUID(sc.Account)
	if !ok {
		return "", fmt.Errorf("account %q is not in the mailkite config (run mailkite setup)", sc.Account)
	}
	switch acc.Kind {
	case "pop":
		if err := d.open(ctx, f); err != nil {
			return "", fmt.Errorf("open: %w", err)
		}
		return "opened   " + f + " in Mail", nil
	case "imap":
		if sc.Mailbox == "" {
			return "", errors.New("sidecar has no mailbox")
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		if len(raw) == 0 {
			return "", errors.New("empty file")
		}
		c, ok := conns[acc.MailUUID]
		if !ok {
			if c, err = d.dial(ctx, acc); err != nil {
				return "", err
			}
			conns[acc.MailUUID] = c
		}
		if err := c.Append(ctx, sc.Mailbox, raw, true); err != nil {
			return "", err
		}
		return fmt.Sprintf("restored %s -> %s", f, sc.Mailbox), nil
	}
	return "", fmt.Errorf("account kind %q", acc.Kind)
}
