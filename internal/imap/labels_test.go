package imap

import (
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
)

func TestClassifyLabelType(t *testing.T) {
	tests := []struct {
		name    string
		mailbox string
		attrs   []imap.MailboxAttr
		want    string
	}{
		// Name-based detection
		{name: "INBOX", mailbox: "INBOX", want: "system"},
		{name: "inbox lowercase", mailbox: "inbox", want: "system"},
		{name: "Sent", mailbox: "Sent", want: "system"},
		{name: "Sent Items", mailbox: "Sent Items", want: "system"},
		{name: "Sent Messages", mailbox: "Sent Messages", want: "system"},
		{name: "Drafts", mailbox: "Drafts", want: "system"},
		{name: "Draft", mailbox: "Draft", want: "system"},
		{name: "Trash", mailbox: "Trash", want: "system"},
		{name: "Deleted Items", mailbox: "Deleted Items", want: "system"},
		{name: "Deleted Messages", mailbox: "Deleted Messages", want: "system"},
		{name: "Junk", mailbox: "Junk", want: "system"},
		{name: "Bulk Mail", mailbox: "Bulk Mail", want: "system"},
		{name: "Spam", mailbox: "Spam", want: "system"},
		{name: "Archive", mailbox: "Archive", want: "system"},
		{name: "All Mail", mailbox: "All Mail", want: "system"},
		{name: "Gmail All Mail", mailbox: "[Gmail]/All Mail", want: "system"},

		// Attribute-based detection
		{
			name:    "attr Sent",
			mailbox: "custom-sent",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrSent},
			want:    "system",
		},
		{
			name:    "attr Drafts",
			mailbox: "custom-drafts",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrDrafts},
			want:    "system",
		},
		{
			name:    "attr Trash",
			mailbox: "custom-trash",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrTrash},
			want:    "system",
		},
		{
			name:    "attr Junk",
			mailbox: "custom-junk",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrJunk},
			want:    "system",
		},
		{
			name:    "attr All",
			mailbox: "custom-all",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrAll},
			want:    "system",
		},
		{
			name:    "attr Archive",
			mailbox: "custom-archive",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrArchive},
			want:    "system",
		},
		{
			name:    "attr Flagged",
			mailbox: "custom-flagged",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrFlagged},
			want:    "system",
		},

		// Custom folder → user
		{name: "custom folder", mailbox: "Projects/Work", want: "user"},
		{name: "custom with attrs", mailbox: "MyFolder",
			attrs: []imap.MailboxAttr{imap.MailboxAttrHasChildren},
			want:  "user",
		},

		// Attribute takes priority over name
		{
			name:    "attr overrides unknown name",
			mailbox: "Papierkorb",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrTrash},
			want:    "system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLabelType(tt.mailbox, tt.attrs)
			assert.Equal(t, tt.want, got, "classifyLabelType(%q, %v)", tt.mailbox, tt.attrs)
		})
	}
}

func TestLabelsForFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags []imap.Flag
		want  []string
	}{
		{
			name:  "no flags means unread",
			flags: nil,
			want:  []string{"UNREAD"},
		},
		{
			name:  "seen message has no flag labels",
			flags: []imap.Flag{imap.FlagSeen},
			want:  []string{},
		},
		{
			name:  "flagged becomes starred",
			flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged},
			want:  []string{"STARRED"},
		},
		{
			name:  "answered plus custom keyword",
			flags: []imap.Flag{imap.FlagAnswered, "Traite"},
			want:  []string{"ANSWERED", "Traite", "UNREAD"},
		},
		{
			name: "system flags and dollar keywords are skipped",
			flags: []imap.Flag{
				imap.FlagSeen,
				imap.FlagDraft,
				imap.FlagDeleted,
				`\Recent`,
				imap.FlagForwarded,
				imap.FlagMDNSent,
				"$Junk",
			},
			want: []string{},
		},
		{
			name:  "system flag atoms match case-insensitively",
			flags: []imap.Flag{`\seen`, `\FLAGGED`, `\answered`},
			want:  []string{"STARRED", "ANSWERED"},
		},
		{
			name:  "bare spam-training keywords are skipped",
			flags: []imap.Flag{imap.FlagSeen, "NonJunk", "Junk", "NotJunk"},
			want:  []string{},
		},
		{
			name:  "spam-training keywords are skipped case-insensitively",
			flags: []imap.Flag{imap.FlagSeen, "nonjunk", "JUNK", "notjunk", "Traite"},
			want:  []string{"Traite"},
		},
		{
			name:  "duplicate flags map to one label",
			flags: []imap.Flag{"Traite", "Traite", imap.FlagFlagged, "STARRED"},
			want:  []string{"Traite", "STARRED", "UNREAD"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, labelsForFlags(tt.flags))
		})
	}
}

func TestMergeFlagLabels(t *testing.T) {
	tests := []struct {
		name       string
		labels     []string
		flagLabels []string
		want       []string
	}{
		{
			name:       "appends missing flag labels",
			labels:     []string{"INBOX"},
			flagLabels: []string{"UNREAD", "STARRED"},
			want:       []string{"INBOX", "UNREAD", "STARRED"},
		},
		{
			name:       "skips duplicates",
			labels:     []string{"INBOX", "Traite"},
			flagLabels: []string{"Traite", "UNREAD"},
			want:       []string{"INBOX", "Traite", "UNREAD"},
		},
		{
			name:       "no flag labels keeps mailbox labels",
			labels:     []string{"INBOX"},
			flagLabels: nil,
			want:       []string{"INBOX"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mergeFlagLabels(tt.labels, tt.flagLabels))
		})
	}
}
