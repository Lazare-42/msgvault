package googledocs

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
	"testing"
)

func TestResolveSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	single, err := NewClient([]SourceServices{testSourceServices("work")})
	require.NoError(err, "NewClient single")

	src, err := single.resolveSource("")
	require.NoError(err, "resolve single default source")
	assert.Equal("work", src.source.Name, "source name")

	multi, err := NewClient([]SourceServices{
		testSourceServices("work"),
		testSourceServices("personal"),
	})
	require.NoError(err, "NewClient multi")

	_, err = multi.resolveSource("")
	require.Error(err, "resolveSource should require source for multiple configured sources")
	assert.Contains(err.Error(), "multiple Google Docs sources", "error")

	src, err = multi.resolveSource("personal")
	require.NoError(err, "resolve explicit source")
	assert.Equal("personal", src.source.Name, "source name")
}

func TestValidateSourceAndDocumentID(t *testing.T) {
	require := require.New(t)
	client, err := NewClient([]SourceServices{testSourceServices("work")})
	require.NoError(err, "NewClient")

	_, err = NewClient([]SourceServices{{
		Source: config.GoogleDocsSource{
			Name:          "bad",
			Enabled:       true,
			FolderID:      "folder with spaces",
			GoogleAccount: "user@example.com",
		},
		Drive: &drive.Service{},
		Docs:  &docs.Service{},
	}})
	require.Error(err, "invalid folder ID should fail")

	_, err = client.GetDoc(context.Background(), "work", "bad document id", 100)
	require.Error(err, "invalid document ID should fail before API call")
	assert.Contains(t, err.Error(), "document_id is invalid", "error")
}

func testSourceServices(name string) SourceServices {
	return SourceServices{
		Source: config.GoogleDocsSource{
			Name:          name,
			Enabled:       true,
			FolderID:      "folder_" + name,
			GoogleAccount: "user@example.com",
		},
		Drive: &drive.Service{},
		Docs:  &docs.Service{},
	}
}
