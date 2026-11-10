package actions

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/message"
	"github.com/kad-xyz/mailkite/internal/safety"
	"github.com/kad-xyz/mailkite/internal/smtpx"
)

// Unsubscribe methods, in the order they are preferred.
const (
	MethodOneClick = "oneclick" // RFC 8058 POST to the https URL of List-Unsubscribe
	MethodMailto   = "mailto"   // a short email to the mailto address of List-Unsubscribe
	MethodManual   = "manual"   // a link for the user; never opened by mailkite
	MethodNone     = "none"
)

const (
	unsubPrefix    = "unsubscribe"
	oneClickBody   = "List-Unsubscribe=One-Click"
	httpTimeout    = 15 * time.Second
	maxRedirects   = 5
	maxUnsubscribe = 200 // senders in one plan
	defaultUnsub   = "unsubscribe"
)

// UnsubLine is one sender of an unsubscribe preview: the method and its URL
// (one-click URL, mailto URI or the manual link).
type UnsubLine struct {
	Sender, Method, URL string
}

// testNet relaxes the one-click network guard so tests can reach an httptest
// server on 127.0.0.1 with its own certificate. Production never sets it.
type testNet struct{ roots *x509.CertPool }

// unsubItem is one sender of an unsubscribe plan.
type unsubItem struct {
	UnsubLine
	Account string // Mail account UUID of the newest message (mailto From, log)
}

// unsubExtra is the Plan.Extra of an unsubscribe plan.
type unsubExtra struct {
	Items    []unsubItem
	Sources  map[string][]safety.Target // the newest message of each automatic sender
	Cleanup  map[string][]safety.Target // nil without alsoCleanup
	CleanupX planExtra
}

func isUnsubAction(a string) bool { return a == unsubPrefix || strings.HasPrefix(a, unsubPrefix+"+") }

var headerURLRe = regexp.MustCompile(`<([^<>]*)>`)

// headerURLs returns the URIs of a List-Unsubscribe header in order.
func headerURLs(h string) []string {
	var out []string
	for _, g := range headerURLRe.FindAllStringSubmatch(h, -1) {
		if u := strings.TrimSpace(g[1]); u != "" {
			out = append(out, u)
		}
	}
	return out
}

var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"100.64.0.0/10",  // CGNAT
		"192.0.0.0/24",   // IETF protocol assignments
		"198.18.0.0/15",  // benchmarking
		"240.0.0.0/4",    // reserved (and broadcast)
		"64:ff9b::/96",   // NAT64: may embed any IPv4, private ones too
		"64:ff9b:1::/48", // local-use NAT64
		"2002::/16",      // 6to4: may embed any IPv4
	} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// blockedIP reports addresses a one-click request must never reach: loopback,
// private, link-local, multicast, unspecified, CGNAT and reserved ranges, and the
// NAT64/6to4 prefixes that can carry any of them.
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// checkOneClickURL accepts only https URLs without userinfo whose host is a name
// or a public IP. allowPrivate (tests) permits private and loopback addresses.
func checkOneClickURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("bad URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errors.New("not https")
	}
	if u.User != nil {
		return errors.New("URL has user info")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return errors.New("URL has no host")
	}
	if allowPrivate {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("local host")
	}
	if ip := net.ParseIP(host); ip != nil && blockedIP(ip) {
		return errors.New("private address")
	}
	return nil
}

func isOneClickPost(v string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(v), ""), oneClickBody)
}

// manualLink is the first unsubscribe link of the message (body or header), for
// the user to open; any http(s) URL of List-Unsubscribe if none is classified.
func manualLink(m *message.Message) string {
	for _, l := range message.Extract(m, m.HTML()).Links {
		if l.Kind == "unsubscribe" {
			return l.URL
		}
	}
	for _, u := range headerURLs(m.ListUnsubscribe) {
		if l := strings.ToLower(u); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			return u
		}
	}
	return ""
}

// pickMethod chooses how to unsubscribe from m: one-click when List-Unsubscribe
// has a safe https URL and List-Unsubscribe-Post is "List-Unsubscribe=One-Click";
// else mailto when it has a usable mailto; else a manual link; else none.
// Automatic methods use only the List-Unsubscribe header, never the body.
func pickMethod(m *message.Message, allowPrivate bool) UnsubLine {
	urls := headerURLs(m.ListUnsubscribe)
	if isOneClickPost(m.ListUnsubscribePost) {
		for _, u := range urls {
			if checkOneClickURL(u, allowPrivate) == nil {
				return UnsubLine{Method: MethodOneClick, URL: u}
			}
		}
	}
	for _, u := range urls {
		if _, _, err := parseMailto(u); err == nil {
			return UnsubLine{Method: MethodMailto, URL: u}
		}
	}
	if l := manualLink(m); l != "" {
		return UnsubLine{Method: MethodManual, URL: l}
	}
	return UnsubLine{Method: MethodNone}
}

func cleanLine(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "")))
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return s
}

// mailtoBody is the whole body of every mailto unsubscribe: the body hfield of the
// URI is never used, so nothing the user did not see is ever sent.
const mailtoBody = defaultUnsub

// parseMailto reads an RFC 6068 mailto URI. The recipient is exactly one address
// from the URI path; the to, cc, bcc, body and all other hfields are ignored, so
// no other recipient or text is ever added. The subject is one line of at most
// fragRunes runes (default "unsubscribe") and is shown in the dialog.
func parseMailto(raw string) (to, subject string, err error) {
	if len(raw) < 7 || !strings.EqualFold(raw[:7], "mailto:") {
		return "", "", errors.New("not a mailto URI")
	}
	path, query, _ := strings.Cut(raw[7:], "?")
	var addrs []string
	for _, a := range strings.Split(path, ",") {
		d, err := url.PathUnescape(a)
		if err != nil {
			return "", "", errors.New("bad mailto address")
		}
		if d = strings.TrimSpace(d); d != "" {
			addrs = append(addrs, d)
		}
	}
	if len(addrs) != 1 || !smtpx.ValidAddress(addrs[0]) {
		return "", "", errors.New("mailto needs exactly one plain address")
	}
	subject = defaultUnsub
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		k, err1 := url.PathUnescape(k)
		v, err2 := url.PathUnescape(v)
		if err1 != nil || err2 != nil {
			continue
		}
		if strings.EqualFold(k, "subject") {
			if s := frag(v); s != "" {
				subject = s
			}
		}
	}
	return addrs[0], subject, nil
}

// newestFrom finds the newest message (with a Message-ID, not erased by mailkite)
// whose sender matches.
func (e *Engine) newestFrom(ctx context.Context, needle string, match func(string) bool, hidden map[safety.HiddenKey]struct{}) (maildata.Summary, maildata.Mailbox, bool, error) {
	f := maildata.Filter{From: needle, Limit: pageSize, Skip: skipHidden(hidden)}
	for off := 0; off < maxCleanup; off += pageSize {
		f.Offset = off
		items, more, err := e.Store.Find(ctx, f)
		if err != nil {
			return maildata.Summary{}, maildata.Mailbox{}, false, err
		}
		for _, sm := range items {
			if !match(sm.From) || strings.TrimSpace(sm.MessageID) == "" {
				continue
			}
			got, mb, err := e.Store.Get(ctx, sm.ID)
			if errors.Is(err, maildata.ErrNotFound) {
				continue
			}
			if err != nil {
				return maildata.Summary{}, maildata.Mailbox{}, false, err
			}
			if isHidden(hidden, mb.AccountUUID, got.ID) {
				continue
			}
			return got, mb, true, nil
		}
		if !more {
			break
		}
	}
	return maildata.Summary{}, maildata.Mailbox{}, false, nil
}

var addrRe = regexp.MustCompile(`[^\s<>,;:"()]+@[^\s<>,;:"()]+`)

// deliveredTo reports whether email is among the message's recipients (To, Cc,
// Delivered-To, X-Original-To), case-insensitively. Mail sent to an alias is not:
// answering it from the main address would reveal that address.
func (e *Engine) deliveredTo(id int64, mb maildata.Mailbox, email string) bool {
	path, _, ok := e.Store.EmlxPath(id, mb)
	if !ok {
		return false
	}
	raw, err := message.ReadEmlx(path)
	if err != nil {
		return false
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	email = strings.TrimSpace(email)
	for _, k := range []string{"To", "Cc", "Delivered-To", "X-Original-To"} {
		for _, v := range msg.Header[k] {
			var addrs []string
			if list, err := mail.ParseAddressList(v); err == nil {
				for _, a := range list {
					addrs = append(addrs, a.Address)
				}
			} else {
				addrs = addrRe.FindAllString(v, -1)
			}
			for _, a := range addrs {
				if strings.EqualFold(strings.TrimSpace(a), email) {
					return true
				}
			}
		}
	}
	return false
}

// mergeTargets joins the targets of both parts of a plan, so its hash covers both.
func mergeTargets(parts ...map[string][]safety.Target) map[string][]safety.Target {
	out := map[string][]safety.Target{}
	for _, p := range parts {
		for acc, ts := range p {
			out[acc] = append(out[acc], ts...)
		}
	}
	return out
}

// BuildUnsubscribe previews unsubscribing from senders (addresses or @domains):
// for each, the newest message's List-Unsubscribe decides the method. With
// alsoCleanup (erase|trash) the senders' messages are cleaned up the same way
// BuildCleanup does it, in the same plan and the same dialog. It changes nothing
// and touches no network.
func (e *Engine) BuildUnsubscribe(ctx context.Context, senders []string, alsoCleanup string) (Preview, error) {
	if alsoCleanup != "" && alsoCleanup != "erase" && alsoCleanup != "trash" {
		return Preview{}, fmt.Errorf("also_cleanup must be erase or trash, not %q", alsoCleanup)
	}
	var cleanup func(specs []string) (Preview, *safety.Plan, error)
	if alsoCleanup != "" {
		cleanup = func(specs []string) (Preview, *safety.Plan, error) {
			return e.takeCleanup(ctx, CleanupRequest{Senders: specs, Action: alsoCleanup})
		}
	}
	return e.buildUnsub(ctx, senders, alsoCleanup, cleanup)
}

// cleanupAndUnsubscribe is BuildCleanup with Unsubscribe: the cleanup selection
// (filter, senders, allowed protected messages) is built first, then its senders
// are unsubscribed in the same plan and dialog. Only erase and trash combine.
func (e *Engine) cleanupAndUnsubscribe(ctx context.Context, r CleanupRequest) (Preview, error) {
	if r.Action != "erase" && r.Action != "trash" {
		return Preview{}, errors.New("unsubscribe needs action erase or trash")
	}
	r.Unsubscribe = false
	cp, cplan, err := e.takeCleanup(ctx, r)
	if err != nil || cplan == nil {
		return cp, err
	}
	senders := make([]string, 0, len(cp.BySender))
	for _, s := range cp.BySender {
		senders = append(senders, s.Sender)
	}
	return e.buildUnsub(ctx, senders, r.Action, func([]string) (Preview, *safety.Plan, error) { return cp, cplan, nil })
}

// takeCleanup builds a cleanup plan and takes it out of the plan store at once:
// it only ever runs as part of an unsubscribe plan. The plan is nil when there is
// nothing to clean up.
func (e *Engine) takeCleanup(ctx context.Context, r CleanupRequest) (Preview, *safety.Plan, error) {
	cp, err := e.BuildCleanup(ctx, r)
	if err != nil || cp.PlanID == "" {
		return cp, nil, err
	}
	cplan, err := e.Plans.Take(cp.PlanID, e.now())
	if err != nil {
		return Preview{}, nil, err
	}
	cp.PlanID = ""
	return cp, &cplan, nil
}

// buildUnsub previews unsubscribing from senders; cleanup (nil: none) supplies the
// cleanup part of the same plan.
func (e *Engine) buildUnsub(ctx context.Context, senders []string, alsoCleanup string, cleanup func(specs []string) (Preview, *safety.Plan, error)) (Preview, error) {
	var specs []string
	seen := map[string]bool{}
	for _, s := range senders {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		specs = append(specs, s)
	}
	if len(specs) == 0 {
		return Preview{}, errors.New("unsubscribe needs senders (addresses or @domains)")
	}
	if len(specs) > maxUnsubscribe {
		return Preview{}, fmt.Errorf("at most %d senders at once", maxUnsubscribe)
	}
	hidden, err := e.hidden(ctx)
	if err != nil {
		return Preview{}, err
	}
	var p Preview
	var items []unsubItem
	sources := map[string][]safety.Target{}
	auto := 0
	for _, spec := range specs {
		it := unsubItem{UnsubLine: UnsubLine{Sender: spec, Method: MethodNone}}
		needle, match := senderMatcher(spec)
		if match != nil {
			sm, mb, ok, err := e.newestFrom(ctx, needle, match, hidden)
			if err != nil {
				return Preview{}, err
			}
			var m *message.Message
			if ok {
				m = e.parseLocal(sm.ID, mb)
			}
			if m != nil {
				l := pickMethod(m, e.testNet != nil)
				it.Method, it.URL, it.Account = l.Method, l.URL, mb.AccountUUID
				if l.Method == MethodMailto {
					if acc, ok := e.Cfg.ByMailUUID(mb.AccountUUID); !ok || acc.SMTPHost == "" || acc.Email == "" {
						if ml := manualLink(m); ml != "" {
							it.URL = ml
						}
						it.Method = MethodManual
						p.Skipped = append(p.Skipped, fmt.Sprintf("%s: mailto needs SMTP settings of account %s (run mailkite setup)", spec, sm.Account))
					} else if !e.deliveredTo(sm.ID, mb, acc.Email) {
						// Keep the mailto as the manual link; the user decides.
						it.Method = MethodManual
						p.Skipped = append(p.Skipped, spec+": sent to an alias; mailto would reveal your main address")
					}
				}
				if it.Method == MethodOneClick || it.Method == MethodMailto {
					uid, err := e.Store.RemoteID(ctx, sm.ID)
					if err != nil && !errors.Is(err, maildata.ErrNotFound) {
						return Preview{}, err
					}
					sources[mb.AccountUUID] = append(sources[mb.AccountUUID], safety.Target{ID: sm.ID, MessageID: sm.MessageID,
						Mailbox: mb.Name, UID: uid, From: sm.From, Subject: sm.Subject})
					auto++
				}
			}
		}
		items = append(items, it)
		p.Unsub = append(p.Unsub, it.UnsubLine)
	}

	if auto == 0 {
		p.Skipped = append(p.Skipped, "no sender can be unsubscribed automatically; nothing to confirm (use cleanup to remove their mail)")
		return p, nil
	}
	x := unsubExtra{Items: items, Sources: sources}
	cleanupSummary := ""
	if cleanup != nil {
		cp, cplan, err := cleanup(specs)
		if err != nil {
			return Preview{}, err
		}
		p.Count, p.BySender, p.Protected = cp.Count, cp.BySender, cp.Protected
		p.Skipped = append(p.Skipped, cp.Skipped...)
		if cplan != nil {
			// The cleanup plan becomes part of this one: one plan, one dialog.
			cx, ok := cplan.Extra.(planExtra)
			if !ok {
				return Preview{}, errors.New("internal: unexpected cleanup plan")
			}
			x.Cleanup, x.CleanupX, cleanupSummary = cplan.Account, cx, cplan.Summary
		}
	}
	action := unsubPrefix
	p.Button = fmt.Sprintf("Отписаться: %d", auto)
	if x.Cleanup != nil {
		action += "+" + alsoCleanup
		if alsoCleanup == "erase" {
			p.Button += fmt.Sprintf(" и стереть %d", p.Count)
		} else {
			p.Button += fmt.Sprintf(" и в корзину %d", p.Count)
		}
	}
	plan, err := safety.NewPlanFor(action, mergeTargets(x.Sources, x.Cleanup), e.unsubSummary(x, p.Count, cleanupSummary), p.Button, x, e.now())
	if err != nil {
		return Preview{}, err
	}
	p.PlanID = e.Plans.New(plan)
	return p, nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}

// unsubSummary is the dialog body: the first sentence (who and, with a cleanup,
// how many messages), the protected warning, then who is unsubscribed how and the
// cleanup's own summary. Every address, host and subject is one line of at most
// fragRunes runes.
func (e *Engine) unsubSummary(x unsubExtra, count int, cleanup string) string {
	var b strings.Builder
	var auto, manual []unsubItem
	for _, it := range x.Items {
		switch it.Method {
		case MethodOneClick, MethodMailto:
			auto = append(auto, it)
		case MethodManual:
			manual = append(manual, it)
		}
	}
	fmt.Fprintf(&b, "Отписаться от %d %s", len(auto), plural(len(auto), "отправителя", "отправителей", "отправителей"))
	if cleanup != "" {
		switch x.CleanupX.Op {
		case "erase":
			fmt.Fprintf(&b, " и стереть насовсем %s", letters(count))
		case "trash":
			fmt.Fprintf(&b, " и переместить в Корзину %s", letters(count))
		}
	}
	b.WriteString(".")
	if cleanup != "" && x.CleanupX.Included > 0 {
		fmt.Fprintf(&b, "\nВнимание: в том числе защищённых: %d.", x.CleanupX.Included)
	}
	for i, it := range auto {
		if i == dialogSenders {
			fmt.Fprintf(&b, "\n…и ещё %d", len(auto)-i)
			break
		}
		if it.Method == MethodOneClick {
			fmt.Fprintf(&b, "\n%s — запрос на %s", frag(it.Sender), frag(hostOf(it.URL)))
		} else {
			to, subject, _ := parseMailto(it.URL)
			from := ""
			if acc, ok := e.Cfg.ByMailUUID(it.Account); ok {
				from = acc.Email
			}
			fmt.Fprintf(&b, "\n%s — письмо на %s с адреса %s, тема: %s", frag(it.Sender), frag(to), frag(from), subject)
		}
	}
	if len(manual) > 0 {
		fmt.Fprintf(&b, "\n\nВручную (mailkite не открывает ссылки): %d", len(manual))
	}
	if cleanup != "" {
		b.WriteString("\n\nЗатем:\n" + cleanup)
	}
	return b.String()
}

// unsubClient is a fresh HTTP client for one Execute: 15 s timeout, no cookies, no
// proxy, no keep-alive, redirects only to https and at most 5, and never to a
// private or loopback address (checked on the resolved IP at connect time).
func (e *Engine) unsubClient() (*http.Client, *http.Transport) {
	allow := e.testNet != nil
	d := &net.Dialer{Timeout: httpTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		if allow {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
			return errors.New("refusing a private or local address")
		}
		return nil
	}}
	tcfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if e.testNet != nil {
		tcfg.RootCAs = e.testNet.roots
	}
	tr := &http.Transport{
		DialContext:            d.DialContext,
		TLSClientConfig:        tcfg,
		TLSHandshakeTimeout:    httpTimeout,
		ResponseHeaderTimeout:  httpTimeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	c := &http.Client{Transport: tr, Timeout: httpTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if req.Method != http.MethodPost {
			return http.ErrUseLastResponse // 301/302/303 would turn the POST into a GET
		}
		if err := checkOneClickURL(req.URL.String(), allow); err != nil {
			return fmt.Errorf("redirect refused: %v", err)
		}
		return nil
	}}
	return c, tr
}

func (e *Engine) userAgent() string {
	v := e.Version
	if v == "" {
		v = "dev"
	}
	return "mailkite/" + v
}

func shortErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "timeout"
		}
		err = ue.Err
	}
	return cleanLine(err.Error(), 100)
}

// postOneClick sends the RFC 8058 POST; success is a 2xx answer.
func (e *Engine) postOneClick(ctx context.Context, c *http.Client, u string) error {
	if err := checkOneClickURL(u, e.testNet != nil); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(oneClickBody))
	if err != nil {
		return errors.New("bad URL")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", e.userAgent())
	resp, err := c.Do(req)
	if err != nil {
		return errors.New(shortErr(err))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return fmt.Errorf("HTTP %d (redirect not followed)", resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// sendMailto sends the unsubscribe email from the account that got the message.
func (e *Engine) sendMailto(ctx context.Context, it unsubItem) error {
	to, subject, err := parseMailto(it.URL)
	if err != nil {
		return err
	}
	acc, ok := e.Cfg.ByMailUUID(it.Account)
	if !ok || acc.SMTPHost == "" {
		return errors.New("account has no SMTP settings")
	}
	if e.Secrets == nil {
		return errors.New("no password store")
	}
	pw, err := e.Secrets.Get(acc.Email)
	if err != nil {
		return errors.New("no password in Keychain (run mailkite setup)")
	}
	send := e.SendMail
	if send == nil {
		send = smtpx.Send
	}
	if err := send(ctx, acc, pw, acc.Email, []string{to}, smtpx.Build(acc.Email, to, subject, mailtoBody)); err != nil {
		return errors.New(cleanLine(err.Error(), 100))
	}
	return nil
}

// executeUnsub runs a verified unsubscribe plan: one dialog, then each sender,
// then the cleanup part (if any) exactly as a cleanup plan runs.
func (e *Engine) executeUnsub(ctx context.Context, plan safety.Plan) (Report, error) {
	x, ok := plan.Extra.(unsubExtra)
	if !ok {
		return Report{}, fmt.Errorf("plan action %q is not supported", plan.Action)
	}
	if err := plan.Verify(plan.Action, mergeTargets(x.Sources, x.Cleanup)); err != nil {
		return Report{}, err
	}
	if len(x.Cleanup) > 0 && x.CleanupX.Op == "erase" && (e.Archive == nil || e.Archive.Dir == "") {
		return Report{}, errors.New("no archive configured; refusing to erase")
	}
	if e.Confirm == nil {
		return Report{}, errors.New("no confirmation dialog available; nothing was changed")
	}
	yes, err := e.Confirm.Confirm(ctx, "mailkite", plan.Summary, plan.Button)
	if err != nil {
		e.logAll(plan, "dialog_failed")
		return Report{}, fmt.Errorf("confirmation dialog failed, nothing was changed: %w", err)
	}
	if !yes {
		e.logAll(plan, "cancelled")
		return Report{Cancelled: true}, nil
	}
	rep := Report{Unsubscribe: e.runUnsub(ctx, x)}
	if len(x.Cleanup) > 0 {
		cr := e.run(ctx, safety.Plan{Action: cleanupPrefix + x.CleanupX.Op, Account: x.Cleanup}, x.CleanupX)
		rep.Done, rep.NotFound, rep.Failed, rep.PerAccount = cr.Done, cr.NotFound, cr.Failed, cr.PerAccount
	}
	return rep, nil
}

// runUnsub unsubscribes sender by sender and logs counts per account (no
// addresses or URLs in actions.log).
func (e *Engine) runUnsub(ctx context.Context, x unsubExtra) []string {
	client, tr := e.unsubClient()
	defer tr.CloseIdleConnections()
	type counts struct{ okClick, badClick, okMail, badMail int }
	per := map[string]*counts{}
	var lines []string
	for _, it := range x.Items {
		var err error
		switch it.Method {
		case MethodOneClick:
			err = e.postOneClick(ctx, client, it.URL)
		case MethodMailto:
			err = e.sendMailto(ctx, it)
		case MethodManual:
			lines = append(lines, it.Sender+" | manual: "+it.URL)
			continue
		default:
			lines = append(lines, it.Sender+" | none")
			continue
		}
		c := per[it.Account]
		if c == nil {
			c = &counts{}
			per[it.Account] = c
		}
		switch {
		case err != nil && it.Method == MethodOneClick:
			c.badClick++
		case err != nil:
			c.badMail++
		case it.Method == MethodOneClick:
			c.okClick++
		default:
			c.okMail++
		}
		if err != nil {
			lines = append(lines, it.Sender+" | failed: "+err.Error())
		} else {
			lines = append(lines, it.Sender+" | done")
		}
	}
	if e.State != nil {
		for _, uuid := range sortedKeys(per) {
			c := per[uuid]
			_ = e.State.Log(safety.ActionLine{TS: e.now(), Action: unsubPrefix, Account: uuid, Count: c.okClick + c.okMail,
				Result: fmt.Sprintf("oneclick done=%d failed=%d mailto done=%d failed=%d", c.okClick, c.badClick, c.okMail, c.badMail)})
		}
	}
	return lines
}
