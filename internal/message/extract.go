package message

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Link is a labeled link. Kind is confirm|login|unsubscribe|reset|other.
type Link struct{ Kind, Text, URL string }

// Extracted holds one-time codes and links found in a message.
type Extracted struct {
	Codes []string
	Links []Link
}

const (
	maxLinks = 15
	maxCodes = 5
	window   = 40 // runes between a keyword and a code
)

var (
	keywordRe   = regexp.MustCompile(`(?i)(код|code|пароль|passcode|password|otp|pin|verification)`)
	tokenRe     = regexp.MustCompile(`[A-Za-z0-9]+`)
	urlSpanRe   = regexp.MustCompile(`(?i)(https?://|www\.)\S+|\S+@\S+\.\S+`)
	phoneSpanRe = regexp.MustCompile(`\+?\d[ \d().-]{8,}\d`)
	angleRe     = regexp.MustCompile(`<([^<>]*)>`)
	textURLRe   = regexp.MustCompile(`https?://[^\s<>"'\]]+`)

	unsubRe   = regexp.MustCompile(`(?i)unsubscribe|отпис|optout|opt-out`)
	confirmRe = regexp.MustCompile(`(?i)confirm|verify|подтверд|activate|активир`)
	loginRe   = regexp.MustCompile(`(?i)login|sign in|(?:^|[^\p{L}])(?:войти|вход)(?:[^\p{L}]|$)`)
	resetRe   = regexp.MustCompile(`(?i)reset|сброс|восстанов`)
)

func inSpans(spans [][]int, s, e int) bool {
	for _, sp := range spans {
		if s < sp[1] && e > sp[0] {
			return true
		}
	}
	return false
}

func isCodeToken(tok string) bool {
	n := len(tok)
	digits, upper, lower := 0, 0, 0
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'A' && r <= 'Z':
			upper++
		default:
			lower++
		}
	}
	if lower > 0 {
		return false
	}
	if digits == n {
		return n >= 4 && n <= 8
	}
	return n >= 6 && n <= 8 && digits > 0
}

func keywordBoundaryOK(text string, s, e int) bool {
	if r, _ := utf8.DecodeLastRuneInString(text[:s]); s > 0 && unicode.IsLetter(r) {
		return false
	}
	kw := text[s:e]
	ascii := kw[0] < utf8.RuneSelf
	if ascii && e < len(text) {
		if text[e] == 's' || text[e] == 'S' {
			e++
		}
	}
	if ascii && e < len(text) {
		if r, _ := utf8.DecodeRuneInString(text[e:]); unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// codeKeywords are the literals one of which every keywordRe match contains.
var codeKeywords = []string{"код", "code", "пароль", "passcode", "password", "otp", "pin", "verification"}

func findCodes(text string) []string {
	if !mayContainFold(text, codeKeywords...) {
		return nil
	}
	kws := keywordRe.FindAllStringIndex(text, -1)
	if len(kws) == 0 {
		return nil
	}
	var good [][]int
	for _, k := range kws {
		if keywordBoundaryOK(text, k[0], k[1]) {
			good = append(good, k)
		}
	}
	if len(good) == 0 {
		return nil
	}
	urls := urlSpanRe.FindAllStringIndex(text, -1)
	var phones [][]int
	for _, p := range phoneSpanRe.FindAllStringIndex(text, -1) {
		d := 0
		for _, r := range text[p[0]:p[1]] {
			if r >= '0' && r <= '9' {
				d++
			}
		}
		if d >= 10 {
			phones = append(phones, p)
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range tokenRe.FindAllStringIndex(text, -1) {
		s, e := t[0], t[1]
		tok := text[s:e]
		if !isCodeToken(tok) || seen[tok] || inSpans(urls, s, e) || inSpans(phones, s, e) {
			continue
		}
		if s > 0 && strings.IndexByte("/._=?&#@+%", text[s-1]) >= 0 {
			continue
		}
		if e < len(text) {
			c := text[e]
			if strings.IndexByte("/_=?&#@+%", c) >= 0 {
				continue
			}
			if (c == '.' || c == '-') && e+1 < len(text) && tokenRe.Match([]byte{text[e+1]}) {
				continue
			}
		}
		near := false
		for _, k := range good {
			var gap int
			switch {
			case s >= k[1]:
				gap = utf8.RuneCountInString(text[k[1]:s])
			case e <= k[0]:
				gap = utf8.RuneCountInString(text[e:k[0]])
			default:
				continue
			}
			if gap <= window {
				near = true
				break
			}
		}
		if near {
			seen[tok] = true
			out = append(out, tok)
			if len(out) == maxCodes {
				break
			}
		}
	}
	return out
}

func classify(text, u string) string {
	s := text + " " + u
	switch {
	case mayContainFold(s, "unsubscribe", "отпис", "optout", "opt-out") && unsubRe.MatchString(s):
		return "unsubscribe"
	case mayContainFold(s, "confirm", "verify", "подтверд", "activate", "активир") && confirmRe.MatchString(s):
		return "confirm"
	case mayContainFold(s, "login", "sign in", "войти", "вход") && loginRe.MatchString(s):
		return "login"
	case mayContainFold(s, "reset", "сброс", "восстанов") && resetRe.MatchString(s):
		return "reset"
	}
	return "other"
}

var kindRank = map[string]int{"unsubscribe": 0, "confirm": 1, "login": 2, "reset": 3, "other": 4}

func nodeText(n *html.Node, sb *strings.Builder) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
		return
	}
	if n.Type == html.ElementNode {
		if skipTag(n.DataAtom) || isHidden(n) {
			return
		}
		if n.DataAtom == atom.Img {
			for _, a := range n.Attr {
				if a.Key == "alt" {
					sb.WriteString(a.Val)
				}
			}
			return
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		nodeText(c, sb)
	}
}

func htmlLinks(doc *html.Node) []Link {
	if doc == nil {
		return nil
	}
	var out []Link
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.A {
			href := ""
			for _, a := range n.Attr {
				if a.Key == "href" {
					href = strings.TrimSpace(a.Val)
				}
			}
			var sb strings.Builder
			nodeText(n, &sb)
			out = append(out, Link{Text: clip(normalizeInline(sb.String())), URL: href})
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(doc)
	return out
}

func normalizeInline(s string) string {
	return strings.TrimSpace(collapseBytes(dropInvisible(s), isHTMLSpace))
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= 80 {
		return s
	}
	return string([]rune(s)[:80]) + "…"
}

func validURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// Extract finds one-time codes in the message text and links in the HTML body
// (htmlBody), the text and the List-Unsubscribe header.
func Extract(m *Message, htmlBody string) Extracted {
	var ex Extracted
	ex.Codes = findCodes(m.Text)

	var doc *html.Node
	switch {
	case htmlBody == "":
	case htmlBody == m.html && m.doc != nil:
		doc, m.doc = m.doc, nil // parsed for the text; not needed after this
	default:
		doc, _ = html.Parse(strings.NewReader(htmlBody))
	}
	cands := htmlLinks(doc)
	for _, u := range textURLRe.FindAllString(m.Text, -1) {
		cands = append(cands, Link{URL: strings.TrimRight(u, ".,;:!?)")})
	}
	for _, g := range angleRe.FindAllStringSubmatch(m.ListUnsubscribe, -1) {
		part := strings.TrimSpace(g[1])
		if strings.HasPrefix(strings.ToLower(part), "http") {
			cands = append(cands, Link{Text: "List-Unsubscribe", URL: part})
		}
	}
	seen := map[string]int{}
	var links []Link
	for _, l := range cands {
		if !validURL(l.URL) {
			continue
		}
		if i, ok := seen[l.URL]; ok {
			if links[i].Text == "" {
				links[i].Text = l.Text
			}
			continue
		}
		seen[l.URL] = len(links)
		links = append(links, l)
	}
	for i := range links {
		links[i].Kind = classify(links[i].Text, links[i].URL)
	}
	sort.SliceStable(links, func(i, j int) bool { return kindRank[links[i].Kind] < kindRank[links[j].Kind] })
	if len(links) > maxLinks {
		links = links[:maxLinks]
	}
	ex.Links = links
	return ex
}
