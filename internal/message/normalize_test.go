package message

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// The hand-written whitespace helpers must behave exactly like the regexps they replace.
func TestWhitespaceHelpersMatchRegexps(t *testing.T) {
	invisibleRe := regexp.MustCompile(`[\x{200b}\x{200c}\x{200d}\x{2060}\x{feff}\x{034f}\x{00ad}]`)
	spacesRe := regexp.MustCompile(`[ \t\f\v]+`)
	htmlSpaceRe := regexp.MustCompile(`[ \t\r\n\f]+`)
	manyNLRe := regexp.MustCompile(`\n{3,}`)
	alphabet := []string{" ", "  ", "\t", "\f", "\v", "\r", "\n", "\n\n\n", "a", "я", "\u200b", "\u00ad", "\ufeff", "\u034f", "\xff", "\xe2\x80", "\u00a0", "x y"}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		var sb strings.Builder
		for n := rng.Intn(12); n > 0; n-- {
			sb.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		s := sb.String()
		if got, want := dropInvisible(s), invisibleRe.ReplaceAllString(s, ""); got != want {
			t.Fatalf("dropInvisible(%q) = %q, want %q", s, got, want)
		}
		if got, want := collapseBytes(s, isHSpace), spacesRe.ReplaceAllString(s, " "); got != want {
			t.Fatalf("collapse hspace(%q) = %q, want %q", s, got, want)
		}
		if got, want := collapseBytes(s, isHTMLSpace), htmlSpaceRe.ReplaceAllString(s, " "); got != want {
			t.Fatalf("collapse html(%q) = %q, want %q", s, got, want)
		}
		if got, want := squeezeNewlines(s), manyNLRe.ReplaceAllString(s, "\n\n"); got != want {
			t.Fatalf("squeezeNewlines(%q) = %q, want %q", s, got, want)
		}
	}
}

// mayContainFold must never reject a string the case-insensitive regexp matches.
func TestMayContainFoldIsPrefilter(t *testing.T) {
	cases := []struct {
		re   *regexp.Regexp
		lits []string
	}{
		{unsubRe, []string{"unsubscribe", "отпис", "optout", "opt-out"}},
		{confirmRe, []string{"confirm", "verify", "подтверд", "activate", "активир"}},
		{loginRe, []string{"login", "sign in", "войти", "вход"}},
		{resetRe, []string{"reset", "сброс", "восстанов"}},
		{hardHiddenRe, []string{"display", "visibility"}},
		{softHiddenRe, []string{"max-height", "font-size", "opacity"}},
		{keywordRe, codeKeywords},
	}
	inputs := []string{"UNSUBSCRIBE", "Отписаться", "ОТПИС", "OptOut", "Opt-Out", "CONFIRM", "Verify", "ПОДТВЕРДИТЬ", "Активировать",
		"LogIn", "Sign In", "ВОЙТИ", "Вход", "RESET", "Сброс", "Восстановить", "un\u017fubscribe", "\u1c80ход", "Display: None", "VISIBILITY:hidden",
		"Font-Size:0", "MAX-HEIGHT:0px;", "Opacity:0", "КОД", "Code", "ПАРОЛЬ", "PassCode", "OTP", "PIN", "Verification", "nothing here"}
	for _, c := range cases {
		for _, s := range inputs {
			if c.re.MatchString(s) && !mayContainFold(s, c.lits...) {
				t.Errorf("%s: prefilter rejects %q", c.re, s)
			}
		}
	}
}
