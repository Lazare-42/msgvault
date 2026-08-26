package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

type fakeTriageFolderMover struct {
	confirmed map[string]bool
	errors    map[string]error
}

func (f *fakeTriageFolderMover) MoveMessageToFolder(
	_ context.Context,
	messageID, _, _ string,
) (bool, error) {
	return f.confirmed[messageID], f.errors[messageID]
}

func TestParseTriageSkipIDs(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    map[int64]bool
		wantErr string
	}{
		{name: "empty", raw: "", want: map[int64]bool{}},
		{name: "single", raw: "42", want: map[int64]bool{42: true}},
		{
			name: "multiple with spaces",
			raw:  " 1, 2 ,3 ",
			want: map[int64]bool{1: true, 2: true, 3: true},
		},
		{
			name: "trailing comma ignored",
			raw:  "7,",
			want: map[int64]bool{7: true},
		},
		{name: "non-numeric", raw: "1,abc", wantErr: "invalid --skip-ids entry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTriageSkipIDs(tt.raw)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFilterTriageSourceMailbox(t *testing.T) {
	msgs := []query.MessageSummary{
		{ID: 1, SourceMessageID: "INBOX|10"},
		{ID: 2, SourceMessageID: "inbox|11"},
		{ID: 3, SourceMessageID: "INBOX/2026/SITE-A|12"},
		{ID: 4, SourceMessageID: "0 A traiter|13"},
		{ID: 5, SourceMessageID: "INBOX|not-a-uid"},
		{ID: 6, SourceMessageID: "gmail-id"},
	}

	got, skipped := filterTriageSourceMailbox(msgs, "INBOX")

	assert.Equal(t, 4, skipped)
	assert.Equal(t, []query.MessageSummary{msgs[0], msgs[1]}, got)
}

func TestExecuteTriageMovesReportsConfirmedMovesOnly(t *testing.T) {
	client := &fakeTriageFolderMover{
		confirmed: map[string]bool{
			"INBOX|1": true,
			"INBOX|2": false,
			"INBOX|4": true,
		},
		errors: map[string]error{
			"INBOX|3": errors.New("server rejected move"),
		},
	}
	msgs := []query.MessageSummary{
		{ID: 101, SourceMessageID: "INBOX|1"},
		{ID: 102, SourceMessageID: "INBOX|2"},
		{ID: 103, SourceMessageID: "INBOX|3"},
		{ID: 104, SourceMessageID: "INBOX|4"},
	}

	moved, noops, moveErrors := executeTriageMoves(
		context.Background(), client, msgs, "INBOX", "Queue")

	assert.Equal(t, []int64{101, 104}, moved)
	assert.Equal(t, 1, noops)
	assert.Equal(t, 1, moveErrors)
}
