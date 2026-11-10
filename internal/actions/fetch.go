package actions

import (
	"context"
	"errors"

	"github.com/kad-xyz/mailkite/internal/imapx"
	"github.com/kad-xyz/mailkite/internal/maildata"
)

// FetchBody reads one message the local Mail copy lacks from the account's IMAP
// server with BODY.PEEK[] (the message is not marked as read). The connection is
// opened for this call only.
func (e *Engine) FetchBody(ctx context.Context, sm maildata.Summary, mb maildata.Mailbox) ([]byte, error) {
	acc, ok := e.Cfg.ByMailUUID(mb.AccountUUID)
	if !ok {
		return nil, errors.New(notSetUp)
	}
	if acc.Kind == "pop" || e.Secrets == nil || sm.MessageID == "" {
		return nil, errors.New("cannot fetch this message")
	}
	pw, err := e.Secrets.Get(acc.Email)
	if err != nil {
		return nil, errors.New("no password in Keychain")
	}
	dial := e.Dial
	if dial == nil {
		dial = DialIMAP
	}
	conn, err := dial(ctx, acc, pw)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var hint uint32
	if h, err := e.Store.RemoteID(ctx, sm.ID); err == nil {
		hint = h
	}
	found, err := conn.LocateMany(ctx, mb.Name, []imapx.LocateItem{{MessageID: sm.MessageID, UIDHint: hint}})
	if err != nil {
		return nil, err
	}
	uid, ok := found[sm.MessageID]
	if !ok {
		return nil, errors.New("message not on the server")
	}
	raw, err := conn.FetchRaw(ctx, mb.Name, uid)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("empty message")
	}
	return raw, nil
}
