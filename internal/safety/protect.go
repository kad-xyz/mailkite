// Package safety guards every destructive mailkite action: it classifies mail that
// must never be removed automatically, keeps one-time confirmation plans, asks the
// user through a native macOS dialog, archives messages before they are erased and
// records what was done.
package safety

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/message"
)

// Reasons returned by Classify.
const (
	ReasonRule       = "rule"
	ReasonCode       = "code"
	ReasonReceipt    = "receipt"
	ReasonBank       = "bank"
	ReasonPerson     = "person"
	ReasonUnreadable = "unreadable"
)

// textWindow is how many runes of the body are searched for keywords.
const textWindow = 2000

// keywords is one protection category: whole words (letter or digit on either
// side = no match), word starts (stems: no letter or digit before) and plain
// substrings. All are matched on fold()ed text.
type keywords struct{ words, stems, subs []string }

func (k keywords) match(hay string) bool {
	for _, w := range k.words {
		if hasWord(hay, w, true) {
			return true
		}
	}
	for _, w := range k.stems {
		if hasWord(hay, w, false) {
			return true
		}
	}
	return containsAny(hay, k.subs)
}

var (
	codeKeywords = keywords{
		words: []string{"код", "code", "pin"},
		subs:  []string{"пароль", "password", "вход в аккаунт", "sign-in", "security", "безопасност"},
	}
	receiptKeywords = keywords{
		words: []string{"чек", "счет"}, // fold turns счёт into счет
		stems: []string{"order", "заказ", "payment", "оплат", "billing", "statement", "выписк", "booking", "бронирован", "билет", "ticket"},
		subs:  []string{"receipt", "офд", "кассов", "invoice", "квитанц"},
	}
	// notCode phrases never count as a code or a receipt; they are blanked out of the
	// text before the code and receipt keywords are matched.
	notCode = []string{"промокод", "promo code", "promocode", "qr code", "qr-код", "barcode", "штрих-код"}

	bankDomains = []string{"gosuslugi.ru", "nalog.ru", "nalog.gov.ru", "mos.ru", "sberbank.ru", "tinkoff.ru", "tbank.ru", "vtb.ru", "alfabank.ru", "raiffeisen.ru", "gazprombank.ru",
		"sber.ru", "sberbank.com", "alfa-bank.ru", "pochtabank.ru", "cbr.ru", "pfr.gov.ru", "sfr.gov.ru"}

	robotLocalRe = regexp.MustCompile(`(?i)^(no-?reply|noreply|mailer(-daemon)?|notifications?|news(letter)?|info|marketing|promo|hello|support)$`)
)

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// hasWord reports whether w occurs in hay with no letter or digit right before it
// and, when whole, right after it. Unicode-aware (Go's \b is ASCII only).
func hasWord(hay, w string, whole bool) bool {
	if w == "" {
		return false
	}
	for i := 0; i < len(hay); {
		j := strings.Index(hay[i:], w)
		if j < 0 {
			return false
		}
		s, e := i+j, i+j+len(w)
		before, _ := utf8.DecodeLastRuneInString(hay[:s])
		after, _ := utf8.DecodeRuneInString(hay[e:])
		if (s == 0 || !isWordRune(before)) && (!whole || e == len(hay) || !isWordRune(after)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(hay[s:])
		i = s + size
	}
	return false
}

// blank replaces every occurrence of the phrases with a space.
func blank(hay string, phrases []string) string {
	for _, p := range phrases {
		hay = strings.ReplaceAll(hay, fold(p), " ")
	}
	return hay
}

// fold lower-cases s (Unicode-aware), treats ё as е and collapses whitespace runs to
// one space, so keyword matching ignores case, ё/е spelling and line breaks.
func fold(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "ё", "е")
	return strings.Join(strings.Fields(s), " ")
}

func containsAny(hay string, kws []string) bool {
	for _, k := range kws {
		if k = fold(k); k != "" && strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

// firstRunes returns at most n runes of s.
func firstRunes(s string, n int) string {
	if len(s) <= n { // byte length bounds rune count
		return s
	}
	i := 0
	for c := 0; c < n && i < len(s); c++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// address extracts a lower-case bare address from "Name <a@b>" or "a@b".
func address(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if a, err := mail.ParseAddress(s); err == nil {
		return strings.ToLower(a.Address)
	}
	if i := strings.LastIndexByte(s, '<'); i >= 0 {
		s = s[i+1:]
		if j := strings.IndexByte(s, '>'); j >= 0 {
			s = s[:j]
		}
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func splitAddr(a string) (local, domain string) {
	i := strings.LastIndexByte(a, '@')
	if i < 0 {
		return a, ""
	}
	return a[:i], strings.TrimSuffix(a[i+1:], ".")
}

// domainMatch reports whether domain equals want or is a subdomain of it.
func domainMatch(domain, want string) bool {
	want = strings.TrimSuffix(strings.TrimLeft(strings.ToLower(strings.TrimSpace(want)), "@."), ".")
	if want == "" || domain == "" {
		return false
	}
	return domain == want || strings.HasSuffix(domain, "."+want)
}

func anyDomain(domains []string, wants []string) bool {
	for _, d := range domains {
		for _, w := range wants {
			if domainMatch(d, w) {
				return true
			}
		}
	}
	return false
}

// Classify reports whether a message must never be removed automatically, with a
// short reason (rule, code, receipt, bank, person). Any match protects. A nil
// message cannot be checked and is protected as "unreadable".
func Classify(m *message.Message, s maildata.Summary, r config.Rules) (protected bool, reason string) {
	if m == nil {
		return true, ReasonUnreadable
	}
	// Both the index's sender and the header's are checked; a match on either protects.
	var addrs, domains []string
	for _, raw := range []string{s.From, m.From} {
		if a := address(raw); a != "" {
			addrs = append(addrs, a)
			_, d := splitAddr(a)
			domains = append(domains, d)
		}
	}
	// The HTML part is checked too: Text may come from a stub plain part.
	var htmlText string
	if h := m.HTML(); h != "" {
		htmlText = message.HTMLToText(h)
	}
	hay := fold(m.Subject + "\n" + s.Subject + "\n" + firstRunes(m.Text, textWindow) + "\n" + firstRunes(htmlText, textWindow))

	for _, a := range addrs {
		for _, w := range r.ProtectedAddresses {
			if w = strings.ToLower(strings.TrimSpace(w)); w != "" && a == w {
				return true, ReasonRule
			}
		}
	}
	if anyDomain(domains, r.ProtectedDomains) || containsAny(hay, r.ProtectedKeywords) {
		return true, ReasonRule
	}
	kwHay := blank(hay, notCode)
	if codeKeywords.match(kwHay) || hasCode(m, htmlText) {
		return true, ReasonCode
	}
	if receiptKeywords.match(kwHay) {
		return true, ReasonReceipt
	}
	if anyDomain(domains, bankDomains) {
		return true, ReasonBank
	}
	if isPerson(m, s, addrs) {
		return true, ReasonPerson
	}
	return false, ""
}

// hasCode reports a one-time code in the text or in the HTML part's text.
// Extract reads codes from Message.Text only, so the HTML text is checked through a
// stand-in message.
func hasCode(m *message.Message, htmlText string) bool {
	if len(message.Extract(m, "").Codes) > 0 {
		return true
	}
	return htmlText != "" && len(message.Extract(&message.Message{Text: htmlText}, "").Codes) > 0
}

// isPerson: no list markers at all and a sender that does not look like a robot.
func isPerson(m *message.Message, s maildata.Summary, addrs []string) bool {
	if s.ListID || m.ListID != "" || m.ListUnsubscribe != "" {
		return false
	}
	switch strings.ToLower(m.Precedence) {
	case "bulk", "list", "junk":
		return false
	}
	for _, a := range addrs {
		if local, _ := splitAddr(a); robotLocalRe.MatchString(local) {
			return false
		}
	}
	return true
}
