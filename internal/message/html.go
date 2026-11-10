package message

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	hardHiddenRe = regexp.MustCompile(`(?i)(display\s*:\s*none|visibility\s*:\s*hidden)`)
	// Layout tricks: only treated as hidden on elements without element children.
	softHiddenRe = regexp.MustCompile(`(?i)((max-height|font-size)\s*:\s*0(px|pt|em|rem|%)?\s*(;|!|$)|opacity\s*:\s*0(\.0+)?\s*(;|!|$))`)
)

func hasElementChild(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return true
		}
	}
	return false
}

func isHidden(n *html.Node) bool {
	for _, a := range n.Attr {
		switch a.Key {
		case "hidden":
			return true
		case "style":
			if mayContainFold(a.Val, "display", "visibility") && hardHiddenRe.MatchString(a.Val) {
				return true
			}
			if mayContainFold(a.Val, "max-height", "font-size", "opacity") && softHiddenRe.MatchString(a.Val) && !hasElementChild(n) {
				return true
			}
		}
	}
	return false
}

// mayContainFold is a cheap prefilter for a case-insensitive regexp whose every match
// contains one of lits (given in lower case, ASCII or Cyrillic): false means the regexp
// cannot match s. Runes that case-fold to these letters without being their upper case
// (U+017F long s, U+1C80..U+1C88 Cyrillic variants) make it answer true.
func mayContainFold(s string, lits ...string) bool {
	if strings.ContainsFunc(s, func(r rune) bool { return r == 0x17f || (r >= 0x1c80 && r <= 0x1c88) }) {
		return true
	}
	low := strings.ToLower(s)
	for _, l := range lits {
		if strings.Contains(low, l) {
			return true
		}
	}
	return false
}

func skipTag(a atom.Atom) bool {
	switch a {
	case atom.Head, atom.Title, atom.Style, atom.Script, atom.Noscript, atom.Template:
		return true
	}
	return false
}

type textBuilder struct{ sb strings.Builder }

// nl makes sure the output ends with at least n newlines.
func (t *textBuilder) nl(n int) {
	s := t.sb.String()
	if s == "" {
		return
	}
	have := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
		have++
	}
	for ; have < n; have++ {
		t.sb.WriteByte('\n')
	}
}

func (t *textBuilder) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		t.sb.WriteString(collapseBytes(n.Data, isHTMLSpace))
		return
	case html.ElementNode:
		if skipTag(n.DataAtom) || isHidden(n) {
			return
		}
		switch n.DataAtom {
		case atom.Br:
			t.sb.WriteByte('\n')
			return
		case atom.Img:
			return
		case atom.P, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Table, atom.Ul, atom.Ol, atom.Blockquote:
			t.nl(2)
			t.children(n)
			t.nl(2)
			return
		case atom.Div, atom.Tr, atom.Li, atom.Section, atom.Article, atom.Header, atom.Footer, atom.Hr, atom.Pre:
			t.nl(1)
			t.children(n)
			t.nl(1)
			return
		case atom.Td, atom.Th:
			t.sb.WriteByte(' ')
			t.children(n)
			t.sb.WriteByte(' ')
			return
		}
	case html.CommentNode, html.DoctypeNode:
		return
	}
	t.children(n)
}

func (t *textBuilder) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		t.walk(c)
	}
}

// HTMLToText converts HTML to compact plain text: block tags become newlines,
// links keep their text, hidden elements, styles, scripts and the head vanish.
func HTMLToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return normalize(src)
	}
	return docText(doc)
}

func docText(doc *html.Node) string {
	var t textBuilder
	t.walk(doc)
	return normalize(t.sb.String())
}
