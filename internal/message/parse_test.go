package message

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadEmlx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "1.emlx")
	write(t, p, "14\nHello: x\r\n\r\nhi<?xml version=\"1.0\"?><plist/>")
	raw, err := ReadEmlx(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "Hello: x\r\n\r\nhi" {
		t.Fatalf("got %q", raw)
	}
}

func TestReadEmlxTrailingSpacesAndShort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "1.emlx")
	write(t, p, "5  \nabcdefg")
	raw, _ := ReadEmlx(p)
	if string(raw) != "abcde" {
		t.Fatalf("got %q", raw)
	}
	write(t, p, "50\nabc")
	raw, err := ReadEmlx(p)
	if err != nil || string(raw) != "abc" {
		t.Fatalf("got %q %v", raw, err)
	}
	write(t, p, "garbage\nabc")
	if _, err := ReadEmlx(p); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseCharsets(t *testing.T) {
	m, err := Parse(load(t, "plain_cp1251.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Text, "Привет") {
		t.Fatalf("text %q", m.Text)
	}
	if m.From != "Ivan Petrov <ivan@example.ru>" || m.To != "anna@example.com" || m.MessageID != "m1@example.ru" {
		t.Fatalf("headers %q %q %q", m.From, m.To, m.MessageID)
	}
	if m.Date.IsZero() {
		t.Fatal("no date")
	}
	m, err = Parse(load(t, "koi8_subject_rfc2047.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Subject, "Выписка") {
		t.Fatalf("subject %q", m.Subject)
	}
}

func TestParseUnknownCharsetTolerated(t *testing.T) {
	raw := []byte("Subject: x\r\nContent-Type: text/plain; charset=x-nonexistent\r\n\r\nHello there, this is a long enough body text.\r\n")
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Text, "Hello there") {
		t.Fatalf("text %q", m.Text)
	}
}

func TestParseHTMLOnly(t *testing.T) {
	m, err := Parse(load(t, "html_only_qp.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Text, "Подтвердить") {
		t.Fatalf("text %q", m.Text)
	}
	for _, bad := range []string{"color:red", "Preheader", "<a", "href"} {
		if strings.Contains(m.Text, bad) {
			t.Fatalf("text contains %q: %q", bad, m.Text)
		}
	}
	if !strings.Contains(m.HTML(), `href="https://t.co/x"`) {
		t.Fatalf("html %q", m.HTML())
	}
}

func TestParseAttachments(t *testing.T) {
	raw := load(t, "multipart_attach.eml")
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Parts) != 1 {
		t.Fatalf("parts %+v", m.Parts)
	}
	p := m.Parts[0]
	if p.Name != "Счёт.pdf" || p.ContentType != "application/pdf" || p.Size <= 0 {
		t.Fatalf("part %+v", p)
	}
	if !strings.Contains(m.Text, "Счёт во вложении") {
		t.Fatalf("text %q", m.Text)
	}
	name, data, err := PartData(raw, p.Index)
	if err != nil || name != "Счёт.pdf" || !bytes.HasPrefix(data, []byte("%PDF")) {
		t.Fatalf("%q %q %v", name, data, err)
	}
	if int64(len(data)) != p.Size {
		t.Fatalf("size %d vs %d", len(data), p.Size)
	}
	if _, _, err := PartData(raw, 5); err == nil {
		t.Fatal("expected error")
	}
}

func TestAlternativePrefersPlainUnlessTrivial(t *testing.T) {
	mk := func(plain string) []byte {
		return []byte("Content-Type: multipart/alternative; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\n" + plain +
			"\r\n--B\r\nContent-Type: text/html\r\n\r\n<p>The real html content is long enough</p>\r\n--B--\r\n")
	}
	m, _ := Parse(mk("This is the plain text variant, fine."))
	if !strings.Contains(m.Text, "plain text variant") {
		t.Fatalf("%q", m.Text)
	}
	m, _ = Parse(mk("Open in browser"))
	if !strings.Contains(m.Text, "real html content") {
		t.Fatalf("%q", m.Text)
	}
}

func TestListUnsubscribeHeaders(t *testing.T) {
	m, _ := Parse([]byte("List-Unsubscribe: <https://example.com/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nbody"))
	if m.ListUnsubscribe != "<https://example.com/u>" || m.ListUnsubscribePost != "List-Unsubscribe=One-Click" {
		t.Fatalf("%q %q", m.ListUnsubscribe, m.ListUnsubscribePost)
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<html><head><title>x</title></head><body><!-- c --><script>var a=1</script>
<div hidden>H1</div><span style="visibility: hidden">H2</span><p style="font-size:0">H3</p>
<h1>Title</h1><p>One</p><p>Two<br>three</p><table><tr><td>a</td><td>b</td></tr></table>
<ul><li>x</li><li>y</li></ul><img alt="logo" src="a.png"><a href="https://e.com">link</a>   many    spaces</body></html>`
	got := HTMLToText(in)
	for _, bad := range []string{"H1", "H2", "H3", "var a", "logo"} {
		if strings.Contains(got, bad) {
			t.Fatalf("contains %q: %q", bad, got)
		}
	}
	for _, want := range []string{"Title\n", "One", "Two\nthree", "a b", "link many spaces", "x\ny"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("3 newlines: %q", got)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHTMLToTextLayoutTricks(t *testing.T) {
	got := HTMLToText(`<div style="font-size:0"><div style="font-size:14px">Важный текст</div></div><span style="font-size:0;opacity:0">preheader</span>`)
	if !strings.Contains(got, "Важный текст") || strings.Contains(got, "preheader") {
		t.Fatalf("%q", got)
	}
}

func TestPartDataReportsCorruptBody(t *testing.T) {
	raw := "From: a@x.com\r\nSubject: s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nQUJD!!!!REVG\r\n--B--\r\n"
	if _, err := Parse([]byte(raw)); err != nil {
		t.Fatalf("Parse must stay tolerant: %v", err)
	}
	if _, data, err := PartData([]byte(raw), 0); err == nil {
		t.Errorf("corrupt part returned without error: %q", data)
	}
}

func TestPrecedenceAndListIDHeaders(t *testing.T) {
	m, _ := Parse([]byte("Precedence:  Bulk \r\nList-Id: Shop news <news.shop.example>\r\n\r\nbody"))
	if m.Precedence != "Bulk" || m.ListID != "Shop news <news.shop.example>" {
		t.Fatalf("%q %q", m.Precedence, m.ListID)
	}
}
