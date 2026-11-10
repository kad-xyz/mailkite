package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/secret"
)

// verifyFunc checks that the account can log in with the password.
type verifyFunc func(ctx context.Context, a config.Account, password string) error

// runSetup is the interactive `mailkite setup` flow.
func runSetup(ctx context.Context, in io.Reader, out io.Writer,
	list func(context.Context) ([]maildata.Account, error),
	st secret.Store, verify verifyFunc, cfgPath string) error {
	sc := bufio.NewScanner(in)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", err
			}
			return "", io.ErrUnexpectedEOF
		}
		return strings.TrimSpace(sc.Text()), nil
	}

	accs, err := list(ctx)
	if err != nil {
		return err
	}
	if len(accs) == 0 {
		return errors.New("no Mail accounts found")
	}
	for i, a := range accs {
		fmt.Fprintf(out, "%d) %s [%s]\n", i+1, a.Name, a.Kind)
	}
	s, err := ask("Account number: ")
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > len(accs) {
		return fmt.Errorf("invalid account number %q", s)
	}
	ma := accs[n-1]

	email, err := ask("Email: ")
	if err != nil {
		return err
	}
	if !strings.Contains(email, "@") {
		return fmt.Errorf("invalid email %q", email)
	}
	preset, err := ask("Preset (icloud/gmail/manual): ")
	if err != nil {
		return err
	}
	preset = strings.ToLower(preset)

	var acc config.Account
	if preset == "manual" {
		acc.SMTPTLS = "starttls"
		if ma.Kind != "pop" {
			if acc.IMAPHost, err = askHost(ask, "IMAP host: "); err != nil {
				return err
			}
			if acc.IMAPPort, err = askPort(ask, "IMAP port: "); err != nil {
				return err
			}
		}
		if acc.SMTPHost, err = askHost(ask, "SMTP host: "); err != nil {
			return err
		}
		if acc.SMTPPort, err = askPort(ask, "SMTP port: "); err != nil {
			return err
		}
		if t, err := ask("SMTP security (tls/starttls): "); err != nil {
			return err
		} else if t != "tls" && t != "starttls" {
			return fmt.Errorf("invalid SMTP security %q", t)
		} else {
			acc.SMTPTLS = t
		}
	} else {
		p, ok := config.Presets[preset]
		if !ok {
			return fmt.Errorf("unknown preset %q", preset)
		}
		acc = p
	}
	acc.MailUUID, acc.Email, acc.Kind = ma.UUID, email, ma.Kind
	if ma.Kind == "pop" {
		acc.IMAPHost, acc.IMAPPort = "", 0
	}

	fmt.Fprintln(out, "Enter the app password when security asks for it (it is not echoed or stored anywhere else).")
	if err := st.Set(email); err != nil {
		return err
	}
	if acc.Kind != "pop" && verify != nil {
		pw, err := st.Get(email)
		if err != nil {
			return err
		}
		if err := verify(ctx, acc, pw); err != nil {
			fmt.Fprintf(out, "login failed: %v. The new password was saved to the Keychain; run `mailkite setup` again to replace it, or remove it with: security delete-generic-password -s mailkite -a %s\n", err, email)
			return fmt.Errorf("login failed: %w", err)
		}
		fmt.Fprintln(out, "IMAP login OK")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	replaced := false
	for i := range cfg.Accounts {
		if cfg.Accounts[i].MailUUID == acc.MailUUID {
			cfg.Accounts[i], replaced = acc, true
		}
	}
	if !replaced {
		cfg.Accounts = append(cfg.Accounts, acc)
	}
	if err := cfg.Save(cfgPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved %s to %s\n", email, cfgPath)
	return nil
}

func askPort(ask func(string) (string, error), prompt string) (int, error) {
	s, err := ask(prompt)
	if err != nil {
		return 0, err
	}
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

func askHost(ask func(string) (string, error), prompt string) (string, error) {
	h, err := ask(prompt)
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", errors.New("host must not be empty")
	}
	return h, nil
}
