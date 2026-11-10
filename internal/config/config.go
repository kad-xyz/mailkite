// Package config holds mailkite's account settings and rules (never passwords).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultMarkLimit is used when Config.MarkLimit is zero.
const DefaultMarkLimit = 20

// Account links a Mail.app account to its IMAP/SMTP endpoints.
type Account struct {
	MailUUID string
	Email    string
	Kind     string // imap|pop
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
	SMTPTLS  string // "tls"|"starttls"
	Gmail    bool
}

// Rules lists what must never be removed automatically.
type Rules struct {
	ProtectedDomains   []string
	ProtectedAddresses []string
	ProtectedKeywords  []string
}

// Config is the contents of config.json.
type Config struct {
	Accounts  []Account
	Rules     Rules
	MarkLimit int
}

// Presets are ready host settings for known providers.
var Presets = map[string]Account{
	"icloud": {IMAPHost: "imap.mail.me.com", IMAPPort: 993, SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPTLS: "starttls"},
	"gmail":  {IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLS: "tls", Gmail: true},
}

// Path returns ~/.config/mailkite/config.json.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "mailkite", "config.json"), nil
}

// Load reads path; a missing file yields an empty Config with defaults.
func Load(path string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, err
		}
		for i, a := range c.Accounts {
			if err := a.validate(); err != nil {
				return nil, fmt.Errorf("config %s: account %d: %w", path, i+1, err)
			}
		}
	}
	if c.MarkLimit == 0 {
		c.MarkLimit = DefaultMarkLimit
	}
	return c, nil
}

// Save writes the config atomically (temp file + rename), file 0600, directory 0700.
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// ByMailUUID finds the account linked to a Mail.app account UUID.
func (c *Config) ByMailUUID(uuid string) (Account, bool) {
	for _, a := range c.Accounts {
		if a.MailUUID == uuid {
			return a, true
		}
	}
	return Account{}, false
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

func (a Account) validate() error {
	if a.Email == "" {
		return errors.New("empty email")
	}
	switch a.Kind {
	case "imap":
		if a.IMAPHost == "" {
			return fmt.Errorf("%s: empty IMAPHost", a.Email)
		}
		if !validPort(a.IMAPPort) {
			return fmt.Errorf("%s: invalid IMAPPort %d", a.Email, a.IMAPPort)
		}
	case "pop":
	default:
		return fmt.Errorf("%s: Kind must be imap or pop, got %q", a.Email, a.Kind)
	}
	switch a.SMTPTLS {
	case "", "tls", "starttls":
	default:
		return fmt.Errorf("%s: SMTPTLS must be tls or starttls, got %q", a.Email, a.SMTPTLS)
	}
	if a.SMTPHost != "" && !validPort(a.SMTPPort) {
		return fmt.Errorf("%s: invalid SMTPPort %d", a.Email, a.SMTPPort)
	}
	return nil
}
