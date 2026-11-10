// Package testfixture builds a synthetic Apple Mail data directory
// (Envelope Index, Accounts4.sqlite and .emlx files) for tests.
package testfixture

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const storeDir = "00000000-0000-0000-0000-000000000000"

const emlxTail = `<?xml version="1.0"?><plist version="1.0"><dict/></plist>`

const indexSchema = `
CREATE TABLE messages(ROWID INTEGER PRIMARY KEY AUTOINCREMENT, message_id INTEGER, global_message_id INTEGER, remote_id INTEGER, sender INTEGER, subject_prefix TEXT, subject INTEGER, summary INTEGER, date_sent INTEGER, date_received INTEGER, mailbox INTEGER, flags INTEGER, read INTEGER, flagged INTEGER, deleted INTEGER, size INTEGER, conversation_id INTEGER, list_id_hash INTEGER, unsubscribe_type INTEGER);
CREATE TABLE subjects(ROWID INTEGER PRIMARY KEY AUTOINCREMENT, subject TEXT);
CREATE TABLE addresses(ROWID INTEGER PRIMARY KEY AUTOINCREMENT, address TEXT, comment TEXT);
CREATE TABLE mailboxes(ROWID INTEGER PRIMARY KEY AUTOINCREMENT, url TEXT, total_count INTEGER, unread_count INTEGER);
CREATE TABLE message_global_data(ROWID INTEGER PRIMARY KEY AUTOINCREMENT, message_id INTEGER, message_id_header TEXT);
CREATE TABLE recipients(ROWID INTEGER PRIMARY KEY AUTOINCREMENT, message INTEGER, address INTEGER, type INTEGER, position INTEGER);
CREATE TABLE attachments(ROWID INTEGER PRIMARY KEY AUTOINCREMENT, message INTEGER, attachment_id TEXT, name TEXT);
CREATE TABLE summaries(ROWID INTEGER PRIMARY KEY AUTOINCREMENT, summary TEXT);
`

const accountsSchema = `
CREATE TABLE ZACCOUNTTYPE(Z_PK INTEGER PRIMARY KEY, ZACCOUNTTYPEDESCRIPTION TEXT);
CREATE TABLE ZACCOUNT(Z_PK INTEGER PRIMARY KEY, ZIDENTIFIER TEXT, ZACCOUNTDESCRIPTION TEXT, ZUSERNAME TEXT, ZACCOUNTTYPE INTEGER, ZPARENTACCOUNT INTEGER);
`

// FixtureMsg describes one synthetic message.
type FixtureMsg struct {
	MailboxURL, From, FromName, Subject, MessageID string
	Received                                       time.Time
	Read, Flagged, Deleted                         bool
	ListID                                         bool
	UnsubType                                      int
	To                                             []string
	Attachments                                    []string
	Raw                                            []byte
	Partial                                        bool
}

// Fixture is a temporary analogue of ~/Library/Mail.
type Fixture struct {
	Root       string // analogue of ~/Library/Mail
	AccountsDB string // path to Accounts4.sqlite

	t     *testing.T
	mu    sync.Mutex
	idx   *sql.DB
	acc   *sql.DB
	seq   atomic.Int64
	types map[string]int64
}

// New creates the fixture in a temp dir that is removed when the test ends.
func New(t *testing.T) *Fixture {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "V10", "MailData")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &Fixture{Root: root, AccountsDB: filepath.Join(root, "Accounts4.sqlite"), t: t, types: map[string]int64{}}
	f.idx = f.open(filepath.Join(dataDir, "Envelope Index"), indexSchema)
	f.acc = f.open(f.AccountsDB, accountsSchema)
	t.Cleanup(func() { f.idx.Close(); f.acc.Close() })
	return f
}

func (f *Fixture) open(path, schema string) *sql.DB {
	f.t.Helper()
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		f.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		f.t.Fatal(err)
	}
	return db
}

// AddAccount registers an account with the given UUID, description and type name.
func (f *Fixture) AddAccount(uuid, desc, typ string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	tid, ok := f.types[typ]
	if !ok {
		res, err := f.acc.Exec("INSERT INTO ZACCOUNTTYPE(ZACCOUNTTYPEDESCRIPTION) VALUES(?)", typ)
		if err != nil {
			f.t.Fatal(err)
		}
		tid, _ = res.LastInsertId()
		f.types[typ] = tid
	}
	if _, err := f.acc.Exec("INSERT INTO ZACCOUNT(ZIDENTIFIER, ZACCOUNTDESCRIPTION, ZUSERNAME, ZACCOUNTTYPE) VALUES(?,?,?,?)",
		uuid, desc, uuid+"@example.com", tid); err != nil {
		f.t.Fatal(err)
	}
}

// AddMessage writes the message rows and its .emlx file and returns the message ROWID.
// It is safe for concurrent use.
func (f *Fixture) AddMessage(m FixtureMsg) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()

	if m.MessageID == "" {
		m.MessageID = fmt.Sprintf("<fixture-%d@example.com>", f.seq.Add(1))
	}
	if m.Received.IsZero() {
		m.Received = time.Unix(1700000000, 0)
	}
	if m.Raw == nil {
		m.Raw = synthesize(m)
	}
	ts := m.Received.Unix()

	tx, err := f.idx.Begin()
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	must := func(err error) {
		if err != nil {
			f.t.Fatal(err)
		}
	}
	lookup := func(sel, ins string, args ...any) int64 {
		var id int64
		err := tx.QueryRow(sel, args...).Scan(&id)
		if err == nil {
			return id
		}
		if err != sql.ErrNoRows {
			f.t.Fatal(err)
		}
		res, err := tx.Exec(ins, args...)
		must(err)
		id, _ = res.LastInsertId()
		return id
	}

	mbox := lookup("SELECT ROWID FROM mailboxes WHERE url=?", "INSERT INTO mailboxes(url,total_count,unread_count) VALUES(?,0,0)", m.MailboxURL)
	subj := lookup("SELECT ROWID FROM subjects WHERE subject=?", "INSERT INTO subjects(subject) VALUES(?)", m.Subject)
	var sender int64
	if m.From != "" {
		var cid int64
		err := tx.QueryRow("SELECT ROWID FROM addresses WHERE address=? AND comment=?", m.From, m.FromName).Scan(&cid)
		if err == sql.ErrNoRows {
			res, err := tx.Exec("INSERT INTO addresses(address,comment) VALUES(?,?)", m.From, m.FromName)
			must(err)
			cid, _ = res.LastInsertId()
		} else {
			must(err)
		}
		sender = cid
	}
	res, err := tx.Exec("INSERT INTO summaries(summary) VALUES(?)", "Fixture body for "+m.Subject)
	must(err)
	sum, _ := res.LastInsertId()
	res, err = tx.Exec("INSERT INTO message_global_data(message_id, message_id_header) VALUES(0,?)", m.MessageID)
	must(err)
	gid, _ := res.LastInsertId()

	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	var listHash int64
	if m.ListID {
		listHash = 1234567
	}
	res, err = tx.Exec(`INSERT INTO messages(message_id, global_message_id, remote_id, sender, subject_prefix, subject, summary,
		date_sent, date_received, mailbox, flags, read, flagged, deleted, size, conversation_id, list_id_hash, unsubscribe_type)
		VALUES(?,?,?,?,?,?,?,?,?,?,0,?,?,?,?,0,?,?)`,
		gid, gid, 0, sender, "", subj, sum, ts, ts, mbox, b2i(m.Read), b2i(m.Flagged), b2i(m.Deleted), len(m.Raw), listHash, m.UnsubType)
	must(err)
	rowid, _ := res.LastInsertId()
	_, err = tx.Exec("UPDATE message_global_data SET message_id=? WHERE ROWID=?", rowid, gid)
	must(err)

	for i, to := range m.To {
		aid := lookup("SELECT ROWID FROM addresses WHERE address=? AND comment=''", "INSERT INTO addresses(address,comment) VALUES(?,'')", to)
		_, err = tx.Exec("INSERT INTO recipients(message,address,type,position) VALUES(?,?,0,?)", rowid, aid, i)
		must(err)
	}
	for i, name := range m.Attachments {
		_, err = tx.Exec("INSERT INTO attachments(message,attachment_id,name) VALUES(?,?,?)", rowid, strconv.Itoa(i+1), name)
		must(err)
	}
	_, err = tx.Exec("UPDATE mailboxes SET total_count=total_count+1, unread_count=unread_count+? WHERE ROWID=?", 1-b2i(m.Read), mbox)
	must(err)

	p := f.emlxPath(m.MailboxURL, rowid, m.Partial)
	must(os.MkdirAll(filepath.Dir(p), 0o755))
	body := strconv.Itoa(len(m.Raw)) + "\n" + string(m.Raw) + emlxTail
	must(os.WriteFile(p, []byte(body), 0o644))
	must(tx.Commit())
	return rowid
}

// emlxPath applies the Mail storage layout rule for a message file.
func (f *Fixture) emlxPath(mailboxURL string, rowid int64, partial bool) string {
	u, err := url.Parse(mailboxURL)
	if err != nil {
		f.t.Fatal(err)
	}
	parts := []string{"V10", u.Host}
	var segs []string
	for _, s := range strings.Split(u.EscapedPath(), "/") {
		if s == "" {
			continue
		}
		d, err := url.PathUnescape(s)
		if err != nil {
			f.t.Fatal(err)
		}
		segs = append(segs, d+".mbox")
	}
	if len(segs) == 0 {
		segs = []string{"INBOX.mbox"}
	}
	parts = append(parts, segs...)
	parts = append(parts, storeDir, "Data")
	if k := rowid / 1000; k > 0 {
		digits := strconv.FormatInt(k, 10)
		for i := len(digits) - 1; i >= 0; i-- {
			parts = append(parts, string(digits[i]))
		}
	}
	name := strconv.FormatInt(rowid, 10) + ".emlx"
	if partial {
		name = strconv.FormatInt(rowid, 10) + ".partial.emlx"
	}
	parts = append(parts, "Messages", name)
	return filepath.Join(append([]string{f.Root}, parts...)...)
}

func synthesize(m FixtureMsg) []byte {
	from := m.From
	if m.FromName != "" {
		from = fmt.Sprintf("%q <%s>", m.FromName, m.From)
	}
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(m.To, ", ") + "\r\n")
	b.WriteString("Subject: " + m.Subject + "\r\n")
	b.WriteString("Message-ID: " + m.MessageID + "\r\n")
	b.WriteString("Date: " + m.Received.UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString("Fixture body for " + m.Subject + "\r\n")
	return []byte(b.String())
}
