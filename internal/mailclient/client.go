// Package mailclient applies provider-independent sender policy to live mail clients.
package mailclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/gmail"
)

// Client keeps account selection separate from the visible sender and reply routing.
// The provider must also authorize alternate senders; this local allowlist does
// not grant Exchange Send As or configure a Gmail send-as identity.
type Client struct {
	gmail.LiveAPI

	account string
	allowed map[string]bool
}

// New allows the account itself and explicitly configured additional senders.
func New(backend gmail.LiveAPI, account string, allowedFrom []string) (*Client, error) {
	c := &Client{LiveAPI: backend, account: account, allowed: make(map[string]bool)}
	for _, value := range append([]string{account}, allowedFrom...) {
		address, err := gmail.ParseSender(value)
		if err != nil {
			return nil, fmt.Errorf("mail sender configuration: %w", err)
		}
		c.allowed[strings.ToLower(address.Address)] = true
	}
	return c, nil
}

func (c *Client) checkFrom(value string) error {
	address, err := gmail.ParseSender(value)
	if err != nil {
		return err
	}
	if !c.allowed[strings.ToLower(address.Address)] {
		return errors.New("from address is not allowed for this account; configure mail.accounts allowed_from and provider send-as permission")
	}
	return nil
}

func (c *Client) prepare(compose *gmail.DraftCompose) (*gmail.DraftCompose, error) {
	if compose == nil {
		return nil, errors.New("draft compose is nil")
	}
	prepared := *compose
	if prepared.From == "" {
		prepared.From = c.account
	}
	if err := c.checkFrom(prepared.From); err != nil {
		return nil, err
	}
	return &prepared, nil
}

func (c *Client) CreateDraft(ctx context.Context, compose *gmail.DraftCompose) (*gmail.ComposedDraft, error) {
	prepared, err := c.prepare(compose)
	if err != nil {
		return nil, err
	}
	return c.LiveAPI.CreateDraft(ctx, prepared)
}

// UpdateDraft preserves routing fields omitted by a body/content replacement.
// A non-nil empty ReplyTo slice explicitly removes the reply destination.
func (c *Client) UpdateDraft(ctx context.Context, id string, compose *gmail.DraftCompose) (*gmail.ComposedDraft, error) {
	if compose == nil {
		return nil, errors.New("draft compose is nil")
	}
	prepared := *compose
	if prepared.From == "" || prepared.ReplyTo == nil {
		previous, err := c.GetDraft(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("read draft routing before update: %w", err)
		}
		if prepared.From == "" {
			prepared.From = previous.Message.From
		}
		if prepared.ReplyTo == nil {
			prepared.ReplyTo = slices.Clone(previous.Message.ReplyTo)
		}
	}
	replacement, err := c.prepare(&prepared)
	if err != nil {
		return nil, err
	}
	return c.LiveAPI.UpdateDraft(ctx, id, replacement)
}

// SendDraft checks the stored sender again: drafts can be edited outside msgvault.
func (c *Client) SendDraft(ctx context.Context, id string) (*gmail.SentMessage, error) {
	draft, err := c.GetDraft(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read draft sender before send: %w", err)
	}
	from := draft.Message.From
	if from == "" {
		from = c.account
	} // Legacy drafts may omit From.
	if err := c.checkFrom(from); err != nil {
		return nil, err
	}
	return c.LiveAPI.SendDraft(ctx, id)
}

var _ gmail.LiveAPI = (*Client)(nil)
