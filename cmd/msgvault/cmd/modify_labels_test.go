package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArchiveIDList(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []int64
		wantErr string
	}{
		{name: "empty", raw: ""},
		{name: "single", raw: "42", want: []int64{42}},
		{
			name: "multiple with spaces",
			raw:  " 1, 2 ,3 ",
			want: []int64{1, 2, 3},
		},
		{
			name: "trailing comma ignored",
			raw:  "7,",
			want: []int64{7},
		},
		{name: "non-numeric", raw: "1,abc", wantErr: "invalid --ids entry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseArchiveIDList(tt.raw)
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

func TestParseModifyLabelsRequest(t *testing.T) {
	tests := []struct {
		name      string
		ids       string
		sourceIDs string
		add       []string
		remove    []string
		want      modifyLabelsRequest
		wantErr   string
	}{
		{
			name: "archive ids with add",
			ids:  "12,34",
			add:  []string{"keyword:Handled"},
			want: modifyLabelsRequest{
				archiveIDs: []int64{12, 34},
				addLabels:  []string{"keyword:Handled"},
			},
		},
		{
			name:      "source ids with remove",
			sourceIDs: "INBOX|41, INBOX|57",
			remove:    []string{"UNREAD"},
			want: modifyLabelsRequest{
				sourceIDs:    []string{"INBOX|41", "INBOX|57"},
				removeLabels: []string{"UNREAD"},
			},
		},
		{
			name:      "both id kinds combine",
			ids:       "12",
			sourceIDs: "INBOX|41",
			add:       []string{"STARRED"},
			remove:    []string{"UNREAD"},
			want: modifyLabelsRequest{
				archiveIDs:   []int64{12},
				sourceIDs:    []string{"INBOX|41"},
				addLabels:    []string{"STARRED"},
				removeLabels: []string{"UNREAD"},
			},
		},
		{
			name: "label entries are trimmed and empties dropped",
			ids:  "12",
			add:  []string{" STARRED ", "", "  "},
			want: modifyLabelsRequest{
				archiveIDs: []int64{12},
				addLabels:  []string{"STARRED"},
			},
		},
		{
			name:    "no labels rejected",
			ids:     "12",
			wantErr: "at least one of --add or --remove",
		},
		{
			name:    "whitespace-only labels rejected",
			ids:     "12",
			add:     []string{"  "},
			wantErr: "at least one of --add or --remove",
		},
		{
			name:    "no ids rejected",
			add:     []string{"STARRED"},
			wantErr: "at least one of --ids or --source-ids",
		},
		{
			name:    "bad archive id rejected",
			ids:     "12,x",
			add:     []string{"STARRED"},
			wantErr: "invalid --ids entry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseModifyLabelsRequest(tt.ids, tt.sourceIDs, tt.add, tt.remove)
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

func TestDedupeStrings(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "empty", in: nil, want: []string{}},
		{name: "no duplicates", in: []string{"a", "b"}, want: []string{"a", "b"}},
		{
			name: "duplicates keep first-seen order",
			in:   []string{"b", "a", "b", "a"},
			want: []string{"b", "a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dedupeStrings(tt.in))
		})
	}
}
