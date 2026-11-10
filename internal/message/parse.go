// Package message parses Apple Mail .emlx messages into clean text,
// attachment lists, one-time codes and labeled links.
package message

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
)

// Part describes an attachment. Index is the position of the part among all
// leaf parts of the message and is what PartData accepts.
type Part struct {
	Index       int
	Name        string
	ContentType string
	Size        int64
	Inline      bool
}

// Message is a parsed message.
type Message struct {
	MessageID, From, To, Cc, Subject string
	Date                             time.Time
	Text                             string
	Parts                            []Part
	ListUnsubscribe                  string
	ListUnsubscribePost              string

	html string
	doc  *html.Node // parsed html, kept when the text came from it
}

// HTML returns the raw HTML body, if the message has one.
func (m *Message) HTML() string { return m.html }

// ReadEmlx reads an .emlx file and returns exactly the RFC 822 bytes declared
// by its first line; the trailing plist is dropped.
func ReadEmlx(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 {
		return nil, errors.New("emlx: missing length line")
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b[:nl])))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("emlx: bad length line %q", strings.TrimSpace(string(b[:nl])))
	}
	body := b[nl+1:]
	if n > len(body) {
		n = len(body)
	}
	return body[:n], nil
}

type leaf struct {
	index int
	e     *message.Entity
	ct    string // lower-case media type
	disp  string // lower-case disposition, "" if none
	name  string
}

func (l leaf) isAttachment() bool {
	if l.disp == "attachment" {
		return true
	}
	return l.name != "" && !strings.HasPrefix(l.ct, "text/") && !strings.HasPrefix(l.ct, "multipart/")
}

func usable(e *message.Entity, err error) bool {
	if e == nil {
		return false
	}
	return err == nil || message.IsUnknownCharset(err) || message.IsUnknownEncoding(err)
}

func decodeWord(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	d := mime.WordDecoder{CharsetReader: charset.Reader}
	if out, err := d.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

func partName(h message.Header) (disp, name string) {
	d, params, err := h.ContentDisposition()
	if err == nil {
		disp = strings.ToLower(d)
		name = params["filename"]
	}
	if name == "" {
		if _, p, err := h.ContentType(); err == nil {
			name = p["name"]
		}
	}
	return disp, decodeWord(name)
}

// walk visits leaf parts depth-first. A leaf is anything that is not multipart.
func walk(e *message.Entity, idx *int, visit func(leaf) bool) bool {
	ct, _, _ := e.Header.ContentType()
	ct = strings.ToLower(ct)
	if mr := e.MultipartReader(); mr != nil {
		for {
			p, err := mr.NextPart()
			if p == nil {
				return true
			}
			if err != nil && !usable(p, err) {
				continue
			}
			if !walk(p, idx, visit) {
				return false
			}
		}
	}
	disp, name := partName(e.Header)
	l := leaf{index: *idx, e: e, ct: ct, disp: disp, name: name}
	*idx++
	return visit(l)
}

func readBody(e *message.Entity) []byte {
	b, _ := io.ReadAll(e.Body)
	return b
}

func utf8Text(b []byte) string { return strings.ToValidUTF8(string(b), "�") }

func fmtAddrs(h message.Header, key string) string {
	raw := h.Get(key)
	if raw == "" {
		return ""
	}
	mh := mail.Header{Header: h}
	list, err := mh.AddressList(key)
	if err != nil || len(list) == 0 {
		return decodeWord(raw)
	}
	out := make([]string, len(list))
	for i, a := range list {
		switch {
		case a.Name == "":
			out[i] = a.Address
		default:
			out[i] = a.Name + " <" + a.Address + ">"
		}
	}
	return strings.Join(out, ", ")
}

type body struct{ plain, html string }

func joinText(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n\n" + b
}

// Parse parses RFC 822 bytes. Bad charsets never fail the parse.
func Parse(raw []byte) (*Message, error) {
	e, err := message.Read(bytes.NewReader(raw))
	if !usable(e, err) {
		if err == nil {
			err = errors.New("message: unreadable")
		}
		return nil, err
	}
	h := e.Header
	m := &Message{
		MessageID:           strings.Trim(strings.TrimSpace(h.Get("Message-Id")), "<>"),
		From:                fmtAddrs(h, "From"),
		To:                  fmtAddrs(h, "To"),
		Cc:                  fmtAddrs(h, "Cc"),
		ListUnsubscribe:     strings.TrimSpace(h.Get("List-Unsubscribe")),
		ListUnsubscribePost: strings.TrimSpace(h.Get("List-Unsubscribe-Post")),
	}
	mh := mail.Header{Header: h}
	if s, err := mh.Subject(); err == nil {
		m.Subject = s
	}
	if m.Subject == "" {
		m.Subject = decodeWord(h.Get("Subject"))
	}
	if d, err := mh.Date(); err == nil {
		m.Date = d
	}

	var b body
	idx := 0
	walk(e, &idx, func(l leaf) bool {
		if l.isAttachment() {
			size := int64(len(readBody(l.e)))
			name := l.name
			if name == "" {
				name = "part" + strconv.Itoa(l.index)
			}
			m.Parts = append(m.Parts, Part{Index: l.index, Name: name, ContentType: l.ct, Size: size, Inline: l.disp == "inline"})
			return true
		}
		switch l.ct {
		case "text/html":
			b.html = joinText(b.html, utf8Text(readBody(l.e)))
		case "text/plain", "":
			b.plain = joinText(b.plain, utf8Text(readBody(l.e)))
		}
		return true
	})
	m.html = b.html
	plain := normalize(b.plain)
	if countNonSpace(plain) >= 20 || b.html == "" {
		m.Text = plain
	} else {
		if doc, err := html.Parse(strings.NewReader(b.html)); err != nil {
			m.Text = normalize(b.html)
		} else {
			m.doc = doc
			m.Text = docText(doc)
		}
		if m.Text == "" {
			m.Text = plain
		}
	}
	return m, nil
}

func countNonSpace(s string) int {
	n := 0
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			n++
		}
	}
	return n
}

// PartData returns the decoded content of the leaf part with the given index.
func PartData(raw []byte, index int) (string, []byte, error) {
	e, err := message.Read(bytes.NewReader(raw))
	if !usable(e, err) {
		if err == nil {
			err = errors.New("message: unreadable")
		}
		return "", nil, err
	}
	var name string
	var data []byte
	found := false
	idx := 0
	walk(e, &idx, func(l leaf) bool {
		if l.index != index {
			return true
		}
		found = true
		name, data = l.name, readBody(l.e)
		if name == "" {
			name = "part" + strconv.Itoa(l.index)
		}
		return false
	})
	if !found {
		return "", nil, fmt.Errorf("part %d not found", index)
	}
	return name, data, nil
}

// isInvisible reports zero-width and soft-hyphen runes that are dropped from text.
func isInvisible(r rune) bool {
	switch r {
	case 0x200b, 0x200c, 0x200d, 0x2060, 0xfeff, 0x034f, 0x00ad:
		return true
	}
	return false
}

// dropInvisible removes invisible runes; every other byte, valid UTF-8 or not, is kept.
func dropInvisible(s string) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if isInvisible(r) {
			if b.Cap() == 0 {
				b.Grow(len(s))
			}
			b.WriteString(s[start:i])
			start = i + size
		}
		i += size
	}
	if start == 0 {
		return s
	}
	b.WriteString(s[start:])
	return b.String()
}

// collapseBytes replaces every run of bytes for which in returns true with one space.
// in must only accept ASCII bytes, so multi-byte sequences are never split.
func collapseBytes(s string, in func(byte) bool) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(s); {
		if !in(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && in(s[j]) {
			j++
		}
		if j-i == 1 && s[i] == ' ' {
			i = j
			continue // already a single space
		}
		if b.Cap() == 0 {
			b.Grow(len(s))
		}
		b.WriteString(s[start:i])
		b.WriteByte(' ')
		start, i = j, j
	}
	if start == 0 {
		return s
	}
	b.WriteString(s[start:])
	return b.String()
}

func isHSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\f' || c == '\v' }

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f'
}

// squeezeNewlines turns every run of 3+ newlines into exactly two.
func squeezeNewlines(s string) string {
	if !strings.Contains(s, "\n\n\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '\n' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '\n' {
			j++
		}
		if j-i >= 3 {
			b.WriteString("\n\n")
		} else {
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

// normalize collapses whitespace: runs of spaces to one, lines trimmed,
// 3+ newlines to 2.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = dropInvisible(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = collapseBytes(s, isHSpace)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = squeezeNewlines(s)
	return strings.TrimSpace(s)
}
