// Package triage implements the pure classification core of the
// triage-queue command: given the messages scanned from a mail pool
// (e.g. INBOX), it buckets each one as "move to the queue folder",
// "already queued", "treated" (carries a treated label or was excluded
// by ID), or "replied" (carries the ANSWERED label while replied
// reporting is enabled). It performs no I/O; callers fetch messages
// and execute moves.
package triage

import (
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/query"
)

// RepliedLabel is the archive label carried by messages whose IMAP
// \Answered flag was set (see internal/imap label mapping).
const RepliedLabel = "ANSWERED"

// Options configures classification.
type Options struct {
	// QueueFolder is the destination folder. Messages already carrying
	// this label (exact, case-insensitive match) are never re-moved.
	QueueFolder string

	// TreatedLabels lists labels that mark a message as handled.
	// Matching is exact and case-insensitive per label: a treated label
	// "traite" does NOT match a folder label "0 A traiter".
	TreatedLabels []string

	// ReportReplied routes messages carrying RepliedLabel into the
	// Replied bucket instead of the Move bucket.
	ReportReplied bool

	// SkipIDs holds archive message IDs judged treated externally
	// (e.g. by a caller reviewing a prior replied report). They are
	// counted separately as SkippedByID.
	SkipIDs map[int64]bool
}

// Result holds the classification buckets.
type Result struct {
	// Move lists messages that should be moved to the queue folder.
	Move []query.MessageSummary
	// Replied lists ANSWERED messages withheld from moving for the
	// caller to report (only populated when Options.ReportReplied).
	Replied []query.MessageSummary
	// SkippedTreated counts messages excluded by a treated label.
	SkippedTreated int
	// SkippedByID counts messages excluded by Options.SkipIDs.
	SkippedByID int
	// SkippedAlreadyQueued counts messages already carrying the queue
	// folder label.
	SkippedAlreadyQueued int
}

// Classify buckets msgs according to opts. Precedence per message:
// already-queued, then treated label, then skip ID, then replied
// (when enabled), then move.
func Classify(msgs []query.MessageSummary, opts Options) Result {
	var res Result
	for _, msg := range msgs {
		switch {
		case hasLabel(msg.Labels, opts.QueueFolder):
			res.SkippedAlreadyQueued++
		case hasAnyLabel(msg.Labels, opts.TreatedLabels):
			res.SkippedTreated++
		case opts.SkipIDs[msg.ID]:
			res.SkippedByID++
		case opts.ReportReplied && hasLabel(msg.Labels, RepliedLabel):
			res.Replied = append(res.Replied, msg)
		default:
			res.Move = append(res.Move, msg)
		}
	}
	return res
}

// hasLabel reports whether labels contains want using exact,
// case-insensitive equality. An empty want never matches.
func hasLabel(labels []string, want string) bool {
	if want == "" {
		return false
	}
	for _, label := range labels {
		if strings.EqualFold(label, want) {
			return true
		}
	}
	return false
}

// hasAnyLabel reports whether labels contains any of wants (exact,
// case-insensitive equality per label).
func hasAnyLabel(labels, wants []string) bool {
	for _, want := range wants {
		if hasLabel(labels, want) {
			return true
		}
	}
	return false
}

// CutoffWorkdays returns the staleness cutoff such that a message is
// stale when n full working days (Mon-Fri) have passed since the day
// it arrived; weekends do not count. The cutoff is a midnight boundary
// in now's location: messages sent before it are stale (strictly-less
// comparison). Walking back day by day, each weekday consumes one of
// the n working days and weekend days are skipped. For example, run on
// a Monday with n=2 the cutoff is the preceding Thursday: Friday mail
// has seen only one full working day and is not yet stale.
func CutoffWorkdays(now time.Time, n int) time.Time {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for i := 0; i < n; i++ {
		day = day.AddDate(0, 0, -1)
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, -1)
		}
	}
	return day
}

// TruncateText returns s truncated to at most max runes. Truncation is
// rune-safe so multi-byte characters are never split.
func TruncateText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
