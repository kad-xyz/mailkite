package message

import (
	"reflect"
	"testing"
)

func codes(text string) []string {
	return Extract(&Message{Text: text}, "").Codes
}

func TestExtractCodes(t *testing.T) {
	if got := codes("Ваш код подтверждения: 482913"); !reflect.DeepEqual(got, []string{"482913"}) {
		t.Fatalf("%v", got)
	}
	if got := codes("© 2026 Example Inc."); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := codes("Звоните: +7 812 555 12 34"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := codes("Your verification code is A1B2C3"); !reflect.DeepEqual(got, []string{"A1B2C3"}) {
		t.Fatalf("%v", got)
	}
	if got := codes("Password 2024 reset"); !reflect.DeepEqual(got, []string{"2024"}) {
		t.Fatalf("%v", got)
	}
	if got := codes("code: see https://example.com/c/123456 now"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := codes("Order 123456 shipped"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := codes("PIN 1234, код 1234"); !reflect.DeepEqual(got, []string{"1234"}) {
		t.Fatalf("dedupe %v", got)
	}
}

func TestExtractLinks(t *testing.T) {
	html := `<a href="https://example.com/news">Новости</a>
<a href="https://example.com/a?x=1">Войти</a>
<a href="https://example.com/c">Подтвердить email</a>
<a href="https://example.com/u">Отписаться</a>
<a href="https://example.com/news">Новости again</a>
<a href="mailto:a@example.com">mail</a>`
	got := Extract(&Message{}, html).Links
	var kinds []string
	for _, l := range got {
		kinds = append(kinds, l.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"unsubscribe", "confirm", "login", "other"}) {
		t.Fatalf("%+v", got)
	}
	if got[0].Text != "Отписаться" || got[0].URL != "https://example.com/u" {
		t.Fatalf("%+v", got[0])
	}
}

func TestExtractLinksFromTextAndKindByURL(t *testing.T) {
	m := &Message{Text: "Reset here: https://example.com/password/reset?t=1, thanks"}
	got := Extract(m, "").Links
	if len(got) != 1 || got[0].Kind != "reset" || got[0].URL != "https://example.com/password/reset?t=1" {
		t.Fatalf("%+v", got)
	}
}

func TestExtractLinksLimit(t *testing.T) {
	html := ""
	for i := 0; i < 30; i++ {
		html += `<a href="https://example.com/p` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `">x</a>`
	}
	html += `<a href="https://example.com/unsub">Unsubscribe</a>`
	got := Extract(&Message{}, html).Links
	if len(got) != 15 || got[0].Kind != "unsubscribe" {
		t.Fatalf("%d %+v", len(got), got[0])
	}
}

func TestExtractCodesFromParsedFixture(t *testing.T) {
	m, err := Parse(load(t, "code.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Extract(m, "").Codes; !reflect.DeepEqual(got, []string{"482913"}) {
		t.Fatalf("%v", got)
	}
}

func TestExtractCodeKeywordForms(t *testing.T) {
	for _, s := range []string{"Your codes: 123456", "passcode 123456"} {
		if got := codes(s); !reflect.DeepEqual(got, []string{"123456"}) {
			t.Fatalf("%q: %v", s, got)
		}
	}
}

func TestExtractLoginWordBoundary(t *testing.T) {
	m := &Message{}
	got := Extract(m, `<a href="https://example.com/mail">Входящие</a><a href="https://example.com/x">Вход</a>`).Links
	if len(got) != 2 || got[0].Kind != "login" || got[0].Text != "Вход" || got[1].Kind != "other" {
		t.Fatalf("%+v", got)
	}
}

func TestListUnsubscribeCommaInURL(t *testing.T) {
	m := &Message{ListUnsubscribe: "<mailto:u@example.com>, <https://example.com/u?a=1,2>"}
	got := Extract(m, "").Links
	if len(got) != 1 || got[0].URL != "https://example.com/u?a=1,2" {
		t.Fatalf("%+v", got)
	}
}
