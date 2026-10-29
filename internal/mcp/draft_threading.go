package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
)

// applyReplyContext makes compose a reply to an archived message. The parent's
// raw headers decide In-Reply-To, References, the reply subject, and, when the
// caller gives no recipients, who the reply goes to. It uses the same reply
// builder as draft_reply, so both tool families thread identically.
func (h *handlers) applyReplyContext(ctx context.Context, args map[string]any, account, sender string, compose *gmail.DraftCompose) error {
	parentID, err := getIDArg(args, "reply_to_message_id")
	if err != nil {
		return err
	}
	for _, key := range []string{"in_reply_to", "references", "thread_id"} {
		if v, _ := args[key].(string); v != "" {
			return fmt.Errorf("reply_to_message_id sets %s from the archived message; omit %s", key, key)
		}
	}
	if sender == "" {
		return errors.New("reply_to_message_id needs a sender address; pass from")
	}
	parent, err := h.engine.GetMessage(ctx, parentID)
	if err != nil {
		return fmt.Errorf("read message %d: %w", parentID, err)
	}
	if parent == nil {
		return fmt.Errorf("message %d not found", parentID)
	}
	raw, err := h.engine.GetMessageRaw(ctx, parentID)
	if err != nil {
		return fmt.Errorf("read raw message %d: %w", parentID, err)
	}
	replyAll, _ := args["reply_all"].(bool)
	selfAddress := sender
	if address, err := gmail.ParseSender(sender); err == nil {
		selfAddress = address.Address
	}
	reply, err := imaplib.BuildReplyWithOptions(raw, sender, "", imaplib.ReplyOptions{
		ReplyAll: replyAll, SelfAddresses: []string{selfAddress},
	}, time.Now(), "")
	if err != nil {
		return fmt.Errorf("build reply to message %d: %w", parentID, err)
	}
	built, err := mail.ReadMessage(bytes.NewReader(reply.Raw))
	if err != nil {
		return fmt.Errorf("read reply headers: %w", err)
	}
	compose.InReplyTo = built.Header.Get("In-Reply-To")
	if compose.InReplyTo == "" {
		return fmt.Errorf("message %d has no Message-ID, so a reply cannot thread under it", parentID)
	}
	compose.References = built.Header.Get("References")
	if compose.Subject == "" {
		subject, err := new(mime.WordDecoder).DecodeHeader(built.Header.Get("Subject"))
		if err != nil {
			subject = built.Header.Get("Subject")
		}
		compose.Subject = subject
	}
	if len(compose.To) == 0 && len(compose.Cc) == 0 {
		if compose.To, err = headerAddresses(built.Header, "To"); err != nil {
			return err
		}
		if compose.Cc, err = headerAddresses(built.Header, "Cc"); err != nil {
			return err
		}
	}
	// Gmail also needs the provider thread ID, which only applies when the
	// parent lives in the account that will hold the draft.
	if parent.SourceConversationID != "" {
		accounts, err := h.engine.ListAccounts(ctx)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		for _, a := range accounts {
			if a.ID == parent.SourceID && a.Identifier == account && a.SourceType == "gmail" {
				compose.ThreadID = parent.SourceConversationID
			}
		}
	}
	return nil
}

func headerAddresses(header mail.Header, name string) ([]string, error) {
	if strings.TrimSpace(header.Get(name)) == "" {
		return nil, nil
	}
	list, err := header.AddressList(name)
	if err != nil {
		return nil, fmt.Errorf("read reply %s: %w", name, err)
	}
	out := make([]string, 0, len(list))
	for _, address := range list {
		out = append(out, address.String())
	}
	return out, nil
}

// draftSender returns the visible sender a new draft will use.
func draftSender(client gmail.LiveAPI, args map[string]any) string {
	if v, _ := args["from"].(string); v != "" {
		return v
	}
	if s, ok := client.(interface{ Sender() string }); ok {
		return s.Sender()
	}
	return ""
}
