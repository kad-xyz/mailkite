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
		{"promokod", "Shop <news@shop.example>", bulkHeaders, "Ваш промокод SALE20", "Скидка на всё", config.Rules{}, ""},
		{"promo code", "Shop <news@shop.example>", bulkHeaders, "Your Promo Code inside", "Big sale", config.Rules{}, ""},
		{"promocode", "Shop <news@shop.example>", bulkHeaders, "Promocode for you", "Big sale", config.Rules{}, ""},
		{"qr code", "Shop <news@shop.example>", bulkHeaders, "QR code inside", "Big sale", config.Rules{}, ""},
		{"qr-kod", "Shop <news@shop.example>", bulkHeaders, "Ваш QR-код на скидку", "Покажите на кассе", config.Rules{}, ""},
		{"shtrikh-kod", "Shop <news@shop.example>", bulkHeaders, "Штрих-код карты", "Покажите на кассе", config.Rules{}, ""},
		{"barcode", "Shop <news@shop.example>", bulkHeaders, "Your barcode", "Big sale", config.Rules{}, ""},
		{"checklist", "Shop <news@shop.example>", bulkHeaders, "Чеклист путешественника", "Что взять с собой", config.Rules{}, ""},
		{"raschet", "Shop <news@shop.example>", bulkHeaders, "Расчет выгоды", "Большая распродажа", config.Rules{}, ""},
		{"pin inside word", "Shop <news@shop.example>", bulkHeaders, "Shopping spinner", "Big sale", config.Rules{}, ""},
		{"border", "Shop <news@shop.example>", bulkHeaders, "Border collie news", "Big sale", config.Rules{}, ""},
		{"predoplata", "Shop <news@shop.example>", bulkHeaders, "Без предоплаты", "Большая распродажа", config.Rules{}, ""},
		{"kod podtverzhdeniya", "Shop <news@shop.example>", bulkHeaders, "Код подтверждения: 123456", "Никому", config.Rules{}, "code"},
		{"qr code plus real code", "Shop <news@shop.example>", bulkHeaders, "QR code and your code", "Big sale", config.Rules{}, "code"},
		{"pin word", "Shop <news@shop.example>", bulkHeaders, "Your PIN changed", "Big sale", config.Rules{}, "code"},
		{"vash chek", "Shop <news@shop.example>", bulkHeaders, "Ваш чек", "Спасибо", config.Rules{}, "receipt"},
		{"schyot na oplatu", "Shop <news@shop.example>", bulkHeaders, "Счёт на оплату", "Спасибо", config.Rules{}, "receipt"},
		{"zakaz", "Shop <news@shop.example>", bulkHeaders, "Ваш заказ отправлен", "Спасибо", config.Rules{}, "receipt"},
		{"oplaty", "Shop <news@shop.example>", bulkHeaders, "Статус оплаты", "Спасибо", config.Rules{}, "receipt"},
		{"bronirovaniya", "Shop <news@shop.example>", bulkHeaders, "Подтверждение бронирования", "Спасибо", config.Rules{}, "receipt"},
		{"vypiska", "Shop <news@shop.example>", bulkHeaders, "Выписка по карте", "Спасибо", config.Rules{}, "receipt"},
		{"bilet", "Shop <news@shop.example>", bulkHeaders, "Ваш билет", "Спасибо", config.Rules{}, "receipt"},
		{"order", "Shop <news@shop.example>", bulkHeaders, "Your Order shipped", "Thanks", config.Rules{}, "receipt"},
		{"payment", "Shop <news@shop.example>", bulkHeaders, "Payments received", "Thanks", config.Rules{}, "receipt"},
		{"billing statement", "Shop <news@shop.example>", bulkHeaders, "Monthly statement", "Billing details", config.Rules{}, "receipt"},
		{"booking ticket", "Shop <news@shop.example>", bulkHeaders, "Booking confirmed", "Your tickets", config.Rules{}, "receipt"},
		{"sber.ru", "Сбер <news@sber.ru>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, "bank"},
		{"sberbank.com", "Sber <news@online.sberbank.com>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, "bank"},
		{"alfa-bank.ru", "Альфа <news@alfa-bank.ru>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, "bank"},
		{"pochtabank.ru", "ПБ <news@pochtabank.ru>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, "bank"},
		{"cbr.ru", "ЦБ <news@cbr.ru>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, "bank"},
		{"pfr.gov.ru", "ПФР <x@mail.pfr.gov.ru>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, "bank"},
		{"sfr.gov.ru", "СФР <x@sfr.gov.ru>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, "bank"},
		{"notcbr.ru", "X <news@notcbr.ru>", bulkHeaders, "Акция", "Большая распродажа", config.Rules{}, ""},
		{"koda", "Shop <news@shop.example>", bulkHeaders, "Введите кода нет", "Большая распродажа", config.Rules{}, "code"},
		{"kodom", "Shop <news@shop.example>", bulkHeaders, "Вход по кодом", "Большая распродажа", config.Rules{}, "code"},
		{"kodov", "Shop <news@shop.example>", bulkHeaders, "Список кодов", "Большая распродажа", config.Rules{}, "code"},
		{"cheki", "Shop <news@shop.example>", bulkHeaders, "Чеки за октябрь", "Спасибо", config.Rules{}, "receipt"},
		{"chekov", "Shop <news@shop.example>", bulkHeaders, "Архив чеков", "Спасибо", config.Rules{}, "receipt"},
		{"scheta", "Shop <news@shop.example>", bulkHeaders, "Новости", "Пришли счета за месяц", config.Rules{}, "receipt"},
		{"schyotom", "Shop <news@shop.example>", bulkHeaders, "Новости", "Что делать со счётом", config.Rules{}, "receipt"},
		{"codes", "Shop <news@shop.example>", bulkHeaders, "Your codes", "Big sale", config.Rules{}, "code"},
		{"pins", "Shop <news@shop.example>", bulkHeaders, "New pins", "Big sale", config.Rules{}, "code"},
		{"kodeks", "Shop <news@shop.example>", bulkHeaders, "Трудовой кодекс", "Большая распродажа", config.Rules{}, ""},
		{"chekap", "Shop <news@shop.example>", bulkHeaders, "Чекап здоровья", "Большая распродажа", config.Rules{}, ""},
		{"kodirovka", "Shop <news@shop.example>", bulkHeaders, "Кодировка письма", "Большая распродажа", config.Rules{}, ""},
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

func TestClassifyCodeOnlyInHTML(t *testing.T) {
	raw := "From: Shop <news@shop.example>\r\n" + bulkHeaders + "Subject: Welcome\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlease open the HTML version of this letter, thanks.\r\n" +
		"--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Your verification: <b>482913</b></p>\r\n--b--\r\n"
	m, err := message.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.Text, "482913") {
		t.Fatalf("test setup: text came from html: %q", m.Text)
	}
	p, r := Classify(m, maildata.Summary{From: "news@shop.example", ListID: true}, config.Rules{})
	if !p || r != "code" {
		t.Fatalf("got (%v, %q)", p, r)
	}
}
