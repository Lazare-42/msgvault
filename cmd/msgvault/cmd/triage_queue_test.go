package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

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

func TestResolveTriageAccount(t *testing.T) {
	imapAlice := daemonclient.CLIAccount{ID: 1, Email: "alice@example.com", Type: sourceTypeIMAP}
	gmailBob := daemonclient.CLIAccount{ID: 2, Email: "bob@example.com", Type: sourceTypeGmail, DisplayName: "Bob Archive"}
	mboxCarol := daemonclient.CLIAccount{ID: 3, Email: "carol@example.com", Type: sourceTypeMbox}

	tests := []struct {
		name     string
		accounts []daemonclient.CLIAccount
		input    string
		want     daemonclient.CLIAccount
		wantErr  string
	}{
		{
			name:     "single syncable account is the default",
			accounts: []daemonclient.CLIAccount{imapAlice, mboxCarol},
			want:     imapAlice,
		},
		{
			name:     "no syncable accounts",
			accounts: []daemonclient.CLIAccount{mboxCarol},
			wantErr:  "no syncable",
		},
		{
			name:     "multiple syncable accounts need --account",
			accounts: []daemonclient.CLIAccount{imapAlice, gmailBob},
			wantErr:  "use --account",
		},
		{
			name:     "explicit email match is case-insensitive",
			accounts: []daemonclient.CLIAccount{imapAlice, gmailBob},
			input:    "Alice@Example.com",
			want:     imapAlice,
		},
		{
			name:     "explicit display name match",
			accounts: []daemonclient.CLIAccount{imapAlice, gmailBob},
			input:    "bob archive",
			want:     gmailBob,
		},
		{
			name:     "unknown identifier",
			accounts: []daemonclient.CLIAccount{imapAlice},
			input:    "nobody@example.com",
			wantErr:  "no syncable account found",
		},
		{
			name:     "non-syncable identifier not matched",
			accounts: []daemonclient.CLIAccount{imapAlice, mboxCarol},
			input:    "carol@example.com",
			wantErr:  "no syncable account found",
		},
		{
			name: "ambiguous identifier",
			accounts: []daemonclient.CLIAccount{
				imapAlice,
				{ID: 4, Email: "alice@example.com", Type: sourceTypeGmail},
			},
			input:   "alice@example.com",
			wantErr: "ambiguous account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTriageAccount(tt.accounts, tt.input)
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
