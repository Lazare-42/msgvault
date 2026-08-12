package triage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func msg(id int64, labels ...string) query.MessageSummary {
	return query.MessageSummary{ID: id, Labels: labels}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name                     string
		msgs                     []query.MessageSummary
		opts                     Options
		wantMoveIDs              []int64
		wantRepliedIDs           []int64
		wantSkippedTreated       int
		wantSkippedByID          int
		wantSkippedAlreadyQueued int
	}{
		{
			name:        "plain stale message moves",
			msgs:        []query.MessageSummary{msg(1, "INBOX", "UNREAD")},
			opts:        Options{QueueFolder: "0 A traiter"},
			wantMoveIDs: []int64{1},
		},
		{
			name: "treated label excluded case-insensitively",
			msgs: []query.MessageSummary{
				msg(1, "INBOX", "TRAITE"),
				msg(2, "INBOX", "Traite"),
				msg(3, "INBOX"),
			},
			opts:               Options{QueueFolder: "Queue", TreatedLabels: []string{"traite"}},
			wantMoveIDs:        []int64{3},
			wantSkippedTreated: 2,
		},
		{
			name: "treated matching is exact not substring",
			msgs: []query.MessageSummary{
				// A folder label "0 A traiter" must NOT match a
				// treated label "traite" substring-wise.
				msg(1, "INBOX", "0 A traiter suivi"),
				msg(2, "INBOX", "traite"),
			},
			opts:               Options{QueueFolder: "Queue", TreatedLabels: []string{"traite"}},
			wantMoveIDs:        []int64{1},
			wantSkippedTreated: 1,
		},
		{
			name: "any of several treated labels excludes",
			msgs: []query.MessageSummary{
				msg(1, "INBOX", "done"),
				msg(2, "INBOX", "Handled"),
				msg(3, "INBOX", "pending"),
			},
			opts:               Options{QueueFolder: "Queue", TreatedLabels: []string{"DONE", "handled"}},
			wantMoveIDs:        []int64{3},
			wantSkippedTreated: 2,
		},
		{
			name: "queue folder label always excluded",
			msgs: []query.MessageSummary{
				msg(1, "INBOX", "0 a TRAITER"),
				msg(2, "INBOX"),
			},
			opts:                     Options{QueueFolder: "0 A Traiter"},
			wantMoveIDs:              []int64{2},
			wantSkippedAlreadyQueued: 1,
		},
		{
			name: "queued wins over treated",
			msgs: []query.MessageSummary{
				msg(1, "Queue", "done"),
			},
			opts:                     Options{QueueFolder: "Queue", TreatedLabels: []string{"done"}},
			wantSkippedAlreadyQueued: 1,
		},
		{
			name: "answered routes to replied when reporting",
			msgs: []query.MessageSummary{
				msg(1, "INBOX", "ANSWERED"),
				msg(2, "INBOX", "answered"),
				msg(3, "INBOX"),
			},
			opts:           Options{QueueFolder: "Queue", ReportReplied: true},
			wantMoveIDs:    []int64{3},
			wantRepliedIDs: []int64{1, 2},
		},
		{
			name: "answered moves when not reporting",
			msgs: []query.MessageSummary{
				msg(1, "INBOX", "ANSWERED"),
			},
			opts:        Options{QueueFolder: "Queue"},
			wantMoveIDs: []int64{1},
		},
		{
			name: "treated wins over replied",
			msgs: []query.MessageSummary{
				msg(1, "INBOX", "ANSWERED", "done"),
			},
			opts: Options{
				QueueFolder:   "Queue",
				TreatedLabels: []string{"done"},
				ReportReplied: true,
			},
			wantSkippedTreated: 1,
		},
		{
			name: "skip ids counted separately from treated labels",
			msgs: []query.MessageSummary{
				msg(1, "INBOX"),
				msg(2, "INBOX"),
				msg(3, "INBOX", "ANSWERED"),
				msg(4, "INBOX", "done"),
			},
			opts: Options{
				QueueFolder:   "Queue",
				TreatedLabels: []string{"done"},
				ReportReplied: true,
				SkipIDs:       map[int64]bool{1: true, 3: true},
			},
			wantMoveIDs:        []int64{2},
			wantSkippedTreated: 1,
			wantSkippedByID:    2,
		},
		{
			name: "empty input",
			opts: Options{QueueFolder: "Queue"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Classify(tt.msgs, tt.opts)

			moveIDs := make([]int64, 0, len(res.Move))
			for _, m := range res.Move {
				moveIDs = append(moveIDs, m.ID)
			}
			repliedIDs := make([]int64, 0, len(res.Replied))
			for _, m := range res.Replied {
				repliedIDs = append(repliedIDs, m.ID)
			}

			assert.Equal(t, tt.wantMoveIDs, nilIfEmpty(moveIDs))
			assert.Equal(t, tt.wantRepliedIDs, nilIfEmpty(repliedIDs))
			assert.Equal(t, tt.wantSkippedTreated, res.SkippedTreated)
			assert.Equal(t, tt.wantSkippedByID, res.SkippedByID)
			assert.Equal(t, tt.wantSkippedAlreadyQueued, res.SkippedAlreadyQueued)
		})
	}
}

func nilIfEmpty(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	return ids
}

func TestCutoffWorkdays(t *testing.T) {
	// 2024-01-08 is a Monday; the surrounding days give one fixed
	// reference week (Mon Jan 8 .. Sat Jan 13) plus the prior week.
	day := func(d int) time.Time {
		return time.Date(2024, time.January, d, 0, 0, 0, 0, time.UTC)
	}
	require.Equal(t, time.Monday, day(8).Weekday(), "fixture sanity: Jan 8 2024 is a Monday")

	tests := []struct {
		name string
		now  time.Time
		n    int
		want time.Time
	}{
		{
			name: "monday n=2 reaches back to thursday",
			now:  day(8).Add(9 * time.Hour), // Mon 09:00
			n:    2,
			want: day(4), // Thursday
		},
		{
			name: "monday n=1 reaches back to friday",
			now:  day(8),
			n:    1,
			want: day(5), // Friday
		},
		{
			name: "tuesday n=1 reaches back to monday",
			now:  day(9),
			n:    1,
			want: day(8),
		},
		{
			name: "wednesday n=3 crosses the weekend",
			now:  day(10),
			n:    3,
			want: day(5), // Friday
		},
		{
			name: "friday n=1 reaches back to thursday",
			now:  day(12),
			n:    1,
			want: day(11),
		},
		{
			name: "friday n=5 reaches back a full week",
			now:  day(12),
			n:    5,
			want: day(5),
		},
		{
			name: "saturday n=1 reaches back to friday",
			now:  day(13),
			n:    1,
			want: day(12),
		},
		{
			name: "saturday n=2 reaches back to thursday",
			now:  day(13),
			n:    2,
			want: day(11),
		},
		{
			name: "n=0 is midnight today",
			now:  day(10).Add(15 * time.Hour),
			n:    0,
			want: day(10),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, CutoffWorkdays(tt.now, tt.n))
		})
	}
}

func TestCutoffWorkdaysFridayMailNotStaleOnMonday(t *testing.T) {
	monday := time.Date(2024, time.January, 8, 10, 0, 0, 0, time.UTC)
	require.Equal(t, time.Monday, monday.Weekday())

	cutoff := CutoffWorkdays(monday, 2)
	fridayMail := time.Date(2024, time.January, 5, 14, 30, 0, 0, time.UTC)
	wednesdayMail := time.Date(2024, time.January, 3, 14, 30, 0, 0, time.UTC)

	assert.False(t, fridayMail.Before(cutoff), "Friday mail must NOT be stale on Monday with n=2")
	assert.True(t, wednesdayMail.Before(cutoff), "Wednesday mail must be stale on Monday with n=2")
}

func TestCutoffWorkdaysPreservesLocation(t *testing.T) {
	loc := time.FixedZone("UTC+2", 2*60*60)
	now := time.Date(2024, time.January, 9, 18, 45, 12, 0, loc) // Tuesday evening

	cutoff := CutoffWorkdays(now, 1)

	assert.Equal(t, time.Date(2024, time.January, 8, 0, 0, 0, 0, loc), cutoff)
	assert.Equal(t, loc, cutoff.Location())
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "short string unchanged", in: "hello", max: 10, want: "hello"},
		{name: "exact length unchanged", in: "hello", max: 5, want: "hello"},
		{name: "long string truncated", in: "hello world", max: 5, want: "hello"},
		{name: "multi-byte runes not split", in: "héllo wörld", max: 7, want: "héllo w"},
		{name: "zero max", in: "hello", max: 0, want: ""},
		{name: "empty string", in: "", max: 600, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TruncateText(tt.in, tt.max))
		})
	}
}
