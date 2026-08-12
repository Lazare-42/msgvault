package imap

import (
	"slices"
	"strings"

	imap "github.com/emersion/go-imap/v2"
)

// systemAttrs maps RFC 6154 special-use attributes to system labels.
var systemAttrs = map[imap.MailboxAttr]bool{
	imap.MailboxAttrSent:    true,
	imap.MailboxAttrDrafts:  true,
	imap.MailboxAttrTrash:   true,
	imap.MailboxAttrJunk:    true,
	imap.MailboxAttrAll:     true,
	imap.MailboxAttrArchive: true,
	imap.MailboxAttrFlagged: true,
}

// systemNames lists folder names (lowercase) that are system labels
// across common IMAP providers.
var systemNames = map[string]bool{
	"inbox":            true,
	"sent":             true,
	"sent items":       true,
	"sent messages":    true,
	"drafts":           true,
	"draft":            true,
	"trash":            true,
	"deleted items":    true,
	"deleted messages": true,
	"junk":             true,
	"bulk mail":        true,
	"spam":             true,
	"archive":          true,
	"all mail":         true,
	"[gmail]/all mail": true,
}

// labelTypeSystem is the label_type value for standard IMAP folders.
const labelTypeSystem = "system"

// classifyLabelType returns "system" for standard IMAP folders
// (detected via RFC 6154 attributes or well-known folder names)
// and "user" for everything else.
func classifyLabelType(
	mailbox string,
	attrs []imap.MailboxAttr,
) string {
	for _, a := range attrs {
		if systemAttrs[a] {
			return labelTypeSystem
		}
	}
	if systemNames[strings.ToLower(mailbox)] {
		return labelTypeSystem
	}
	return "user"
}

// Labels derived from per-message IMAP flags. UNREAD and STARRED match the
// Gmail system label names so searches behave the same across source types.
const (
	flagLabelUnread   = "UNREAD"
	flagLabelStarred  = "STARRED"
	flagLabelAnswered = "ANSWERED"
)

// labelsForFlags maps per-message IMAP flags to archive labels:
//
//   - absence of \Seen        → UNREAD
//   - \Flagged                → STARRED
//   - \Answered               → ANSWERED
//   - custom keywords         → verbatim (e.g. Outlook category "Traite")
//
// Other system flags (\Draft, \Deleted, \Recent, ...) and $-prefixed
// system keywords ($Forwarded, $MDNSent, ...) are skipped. Flag atoms are
// case-insensitive per RFC 3501, so system flags match in any case.
func labelsForFlags(flags []imap.Flag) []string {
	labels := make([]string, 0, len(flags)+1)
	added := make(map[string]bool, len(flags)+1)
	add := func(label string) {
		if !added[label] {
			added[label] = true
			labels = append(labels, label)
		}
	}

	seen := false
	for _, flag := range flags {
		name := string(flag)
		switch {
		case strings.EqualFold(name, string(imap.FlagSeen)):
			seen = true
		case strings.EqualFold(name, string(imap.FlagFlagged)):
			add(flagLabelStarred)
		case strings.EqualFold(name, string(imap.FlagAnswered)):
			add(flagLabelAnswered)
		case strings.HasPrefix(name, `\`), strings.HasPrefix(name, "$"):
			// Other system flags and $-prefixed system keywords.
		default:
			add(name)
		}
	}
	if !seen {
		add(flagLabelUnread)
	}
	return labels
}

// mergeFlagLabels appends the flag-derived labels missing from labels,
// preserving order and avoiding duplicates that would violate the
// message_labels primary key.
func mergeFlagLabels(labels, flagLabels []string) []string {
	for _, flagLabel := range flagLabels {
		if !slices.Contains(labels, flagLabel) {
			labels = append(labels, flagLabel)
		}
	}
	return labels
}
