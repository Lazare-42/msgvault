package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
