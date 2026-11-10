package safety

import (
	"strings"
	"testing"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/message"
)

const bulkHeaders = "List-Id: <news.shop.example>\r\nList-Unsubscribe: <https://shop.example/u>\r\nPrecedence: bulk\r\n"

func mk(t *testing.T, from, headers, subject, text string) (*message.Message, maildata.Summary) {
	t.Helper()
	raw := "From: " + from + "\r\n" + headers + "Subject: " + subject + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + text
	m, err := message.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	addr := from
	if i := strings.LastIndex(from, "<"); i >= 0 {
		addr = strings.Trim(from[i:], "<>")
	}
	return m, maildata.Summary{From: addr, Subject: subject, ListID: strings.Contains(headers, "List-Id")}
}

func TestClassify(t *testing.T) {
	filler := strings.Repeat("а ", 1100) // 2200 runes without keywords
	cases := []struct {
		name, from, headers, subject, text string
		rules                              config.Rules
		want                               string // "" = not protected
	}{
		{"newsletter", "Shop <news@shop.example>", bulkHeaders, "Скидки недели", "Большая распродажа обуви и сумок", config.Rules{}, ""},
		{"code keyword ru", "Shop <news@shop.example>", bulkHeaders, "Ваш КОД подтверждения", "Никому не сообщайте", config.Rules{}, "code"},
		{"security en upper", "Shop <news@shop.example>", bulkHeaders, "SECURITY alert", "New device", config.Rules{}, "code"},
		{"sign-in text", "Shop <news@shop.example>", bulkHeaders, "Hello", "Someone tried a Sign-In from Paris", config.Rules{}, "code"},
		{"vkhod v akkaunt", "Shop <news@shop.example>", bulkHeaders, "Уведомление", "Выполнен Вход в аккаунт с нового устройства", config.Rules{}, "code"},
		{"extract code", "Shop <news@shop.example>", bulkHeaders, "Welcome", "Your verification: 482913 enjoy", config.Rules{}, "code"},
		{"receipt cyrillic upper", "Shop <news@shop.example>", bulkHeaders, "Ваш ЧЕК от магазина", "Спасибо за покупку", config.Rules{}, "receipt"},
		{"receipt ofd", "Shop <news@shop.example>", bulkHeaders, "Документ", "Данные переданы в ОФД", config.Rules{}, "receipt"},
		{"schet with e", "Shop <news@shop.example>", bulkHeaders, "Счет на оплату", "Оплатите до пятницы", config.Rules{}, "receipt"},
		{"invoice", "Shop <news@shop.example>", bulkHeaders, "Your Invoice", "Thanks", config.Rules{}, "receipt"},
		{"bank subdomain", "Т-Банк <news@mail.tbank.ru>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, "bank"},
		{"gov exact", "Госуслуги <no-reply@gosuslugi.ru>", bulkHeaders, "Новости", "Большая новость", config.Rules{}, "bank"},
		{"bank lookalike", "Promo <promo@gosuslugi.ru.evil.example>", bulkHeaders, "Новости", "Большая новость", config.Rules{}, ""},
		{"bank suffix lookalike", "Promo <promo@notsberbank.ru>", bulkHeaders, "Новости", "Большая новость", config.Rules{}, ""},
		{"person", "Иван Петров <ivan@example.org>", "", "Привет", "Как дела? Встретимся в субботу", config.Rules{}, "person"},
		{"noreply not person", "Shop <no-reply@shop.example>", "", "Скидки", "Большая распродажа", config.Rules{}, ""},
		{"Newsletter local part", "Shop <NewsLetter@shop.example>", "", "Скидки", "Большая распродажа", config.Rules{}, ""},
		{"mailer-daemon", "MAILER-DAEMON@mx.example", "", "Undelivered", "Delivery failed", config.Rules{}, ""},
		{"precedence bulk only", "Deals <deals@shop.example>", "Precedence: Bulk\r\n", "Скидки", "Большая распродажа", config.Rules{}, ""},
		{"precedence list only", "Deals <deals@shop.example>", "Precedence: list\r\n", "Скидки", "Большая распродажа", config.Rules{}, ""},
		{"list-unsubscribe only", "Deals <deals@shop.example>", "List-Unsubscribe: <mailto:u@shop.example>\r\n", "Скидки", "Большая распродажа", config.Rules{}, ""},
		{"raw list-id only", "Deals <deals@shop.example>", "List-Id: <x.shop.example>\r\n", "Скидки", "Большая распродажа", config.Rules{}, ""},
		{"keyword beyond 2000", "Shop <news@shop.example>", bulkHeaders, "Скидки", filler + " пароль", config.Rules{}, ""},
		{"rule address", "Boss <boss@corp.example>", bulkHeaders, "Скидки", "Большая распродажа", config.Rules{ProtectedAddresses: []string{"Boss@Corp.Example"}}, "rule"},
		{"rule domain sub", "Boss <x@mail.corp.example>", bulkHeaders, "Скидки", "Большая распродажа", config.Rules{ProtectedDomains: []string{"Corp.Example"}}, "rule"},
		{"rule domain lookalike", "Boss <x@corp.example.evil>", bulkHeaders, "Скидки", "Большая распродажа", config.Rules{ProtectedDomains: []string{"corp.example"}}, ""},
		{"rule keyword", "Shop <news@shop.example>", bulkHeaders, "Скидки", "Про ПРОЕКТ Альфа", config.Rules{ProtectedKeywords: []string{"проект альфа"}}, "rule"},
		{"empty rule ignored", "Shop <news@shop.example>", bulkHeaders, "Скидки", "Большая распродажа", config.Rules{ProtectedKeywords: []string{""}, ProtectedDomains: []string{""}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, s := mk(t, c.from, c.headers, c.subject, c.text)
			p, reason := Classify(m, s, c.rules)
			if p != (c.want != "") || reason != c.want {
				t.Fatalf("got (%v, %q), want %q", p, reason, c.want)
			}
		})
	}
}

func TestClassifyNilMessageProtected(t *testing.T) {
	if p, _ := Classify(nil, maildata.Summary{From: "news@shop.example", ListID: true}, config.Rules{}); !p {
		t.Fatal("unreadable message must be protected")
	}
}

func TestClassifySummaryListIDBlocksPerson(t *testing.T) {
	m, s := mk(t, "Deals <deals@shop.example>", "", "Скидки", "Большая распродажа")
	s.ListID = true
	if p, r := Classify(m, s, config.Rules{}); p {
		t.Fatalf("got %q", r)
	}
}
