package safety

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kad-xyz/mailkite/internal/maildata"
)

// ArchiveDays is how long archived messages are kept.
const ArchiveDays = 30

const (
	dayLayout       = "2006-01-02"
	maxSubjectRunes = 60
	maxNameTries    = 1000
)

// DefaultRoot returns ~/Library/Application Support/mailkite, the directory for the
// archive, state.sqlite and actions.log.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "mailkite"), nil
}

// Archive keeps raw copies of messages before they are erased, one directory per day.
type Archive struct{ Dir string }

// Sidecar is the .json file stored next to every archived .eml.
type Sidecar struct {
	Account   string `json:"account"` // Mail account UUID
	Mailbox   string `json:"mailbox"`
	MessageID string `json:"message_id"`
	Date      string `json:"date"` // message date, RFC 3339
}

// mkdirPrivate creates dir (and parents) and forces 0700 on dir itself.
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// safeName turns a subject into a file-name fragment: no path separators, no
// control characters, no leading dots, at most 60 runes.
func safeName(subject string) string {
	r := []rune(strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == ':' || unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r):
			return '_'
		}
		return r
	}, strings.ToValidUTF8(subject, "_")))
	if len(r) > maxSubjectRunes {
		r = r[:maxSubjectRunes]
	}
	s := strings.TrimRight(strings.TrimLeft(string(r), ". _"), ". ")
	if s == "" {
		return "message"
	}
	return s
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// Save writes raw to Dir/YYYY-MM-DD/<id>-<subject>.eml (day of date, the archiving
// time) with a .json sidecar, both 0600 and fsynced. Existing files are never
// overwritten: a -2, -3, ... suffix is added instead. mailbox is the mailbox the
// message is restored to. It returns the .eml path.
func (a Archive) Save(date time.Time, s maildata.Summary, mailbox string, raw []byte) (string, error) {
	if a.Dir == "" {
		return "", errors.New("archive: empty directory")
	}
	if len(raw) == 0 {
		return "", errors.New("archive: empty message")
	}
	day := filepath.Join(a.Dir, date.Format(dayLayout))
	if err := mkdirPrivate(a.Dir); err != nil {
		return "", err
	}
	if err := mkdirPrivate(day); err != nil {
		return "", err
	}
	msgDate := s.Date
	if msgDate.IsZero() {
		msgDate = date
	}
	meta, err := json.Marshal(Sidecar{Account: s.Account, Mailbox: mailbox, MessageID: s.MessageID, Date: msgDate.Format(time.RFC3339)})
	if err != nil {
		return "", err
	}
	base := strconv.FormatInt(s.ID, 10) + "-" + safeName(s.Subject)
	for i := 1; i <= maxNameTries; i++ {
		name := base
		if i > 1 {
			name += "-" + strconv.Itoa(i)
		}
		eml := filepath.Join(day, name+".eml")
		side := filepath.Join(day, name+".json")
		if _, err := os.Lstat(side); err == nil {
			continue // orphan sidecar: keep it, take the next name
		}
		if err := writeNew(eml, raw); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", fmt.Errorf("archive: %w", err)
		}
		if err := writeNew(side, append(meta, '\n')); err != nil {
			os.Remove(eml)
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", fmt.Errorf("archive: %w", err)
		}
		return eml, nil
	}
	return "", fmt.Errorf("archive: no free name for %q", base)
}

// ReadSidecar reads the .json next to an archived .eml.
func ReadSidecar(emlPath string) (Sidecar, error) {
	var sc Sidecar
	b, err := os.ReadFile(strings.TrimSuffix(emlPath, filepath.Ext(emlPath)) + ".json")
	if err != nil {
		return sc, err
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		return sc, fmt.Errorf("archive sidecar: %w", err)
	}
	return sc, nil
}

// Prune removes day directories older than ArchiveDays days before now. Entries
// whose name is not a YYYY-MM-DD date, files and symlinks are left alone.
func (a Archive) Prune(now time.Time) error {
	if a.Dir == "" {
		return errors.New("archive: empty directory")
	}
	ents, err := os.ReadDir(a.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	y, m, d := now.Date()
	cutoff := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -ArchiveDays)
	var errs []error
	for _, e := range ents {
		if !e.IsDir() { // DirEntry type comes from lstat: symlinks are not dirs
			continue
		}
		day, err := time.ParseInLocation(dayLayout, e.Name(), now.Location())
		if err != nil || day.Format(dayLayout) != e.Name() || !day.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(a.Dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
