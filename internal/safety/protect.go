// Package safety guards every destructive mailkite action: it classifies mail that
// must never be removed automatically, keeps one-time confirmation plans, asks the
// user through a native macOS dialog, archives messages before they are erased and
// records what was done.
package safety

import (
	"net/mail"
	"regexp"
	"strings"
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

var (
	codeKeywords    = []string{"код", "code", "пароль", "password", "вход в аккаунт", "sign-in", "security", "безопасност"}
	receiptKeywords = []string{"чек", "receipt", "офд", "кассов", "invoice", "счёт", "квитанц"}
	bankDomains     = []string{"gosuslugi.ru", "nalog.ru", "nalog.gov.ru", "mos.ru", "sberbank.ru", "tinkoff.ru", "tbank.ru", "vtb.ru", "alfabank.ru", "raiffeisen.ru", "gazprombank.ru"}

	robotLocalRe = regexp.MustCompile(`(?i)^(no-?reply|noreply|mailer(-daemon)?|notifications?|news(letter)?|info|marketing|promo|hello|support)$`)
)

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
	hay := fold(m.Subject + "\n" + s.Subject + "\n" + firstRunes(m.Text, textWindow))

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
	if containsAny(hay, codeKeywords) || len(message.Extract(m, "").Codes) > 0 {
		return true, ReasonCode
	}
	if containsAny(hay, receiptKeywords) {
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
