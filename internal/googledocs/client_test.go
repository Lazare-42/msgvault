package googledocs

import (
	"context"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
)

func TestResolveSource(t *testing.T) {
	single, err := NewClient([]SourceServices{testSourceServices("work")})
	requirepkg.NoError(t, err, "NewClient single")

	src, err := single.resolveSource("")
	requirepkg.NoError(t, err, "resolve single default source")
	assertpkg.Equal(t, "work", src.source.Name, "source name")

	multi, err := NewClient([]SourceServices{
		testSourceServices("work"),
		testSourceServices("personal"),
	})
	requirepkg.NoError(t, err, "NewClient multi")

	_, err = multi.resolveSource("")
	requirepkg.Error(t, err, "resolveSource should require source for multiple configured sources")
	assertpkg.Contains(t, err.Error(), "multiple Google Docs sources", "error")

	src, err = multi.resolveSource("personal")
	requirepkg.NoError(t, err, "resolve explicit source")
	assertpkg.Equal(t, "personal", src.source.Name, "source name")
}

func TestValidateSourceAndDocumentID(t *testing.T) {
	client, err := NewClient([]SourceServices{testSourceServices("work")})
	requirepkg.NoError(t, err, "NewClient")

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
	requirepkg.Error(t, err, "invalid folder ID should fail")

	_, err = client.GetDoc(context.Background(), "work", "bad document id", 100)
	requirepkg.Error(t, err, "invalid document ID should fail before API call")
	assertpkg.Contains(t, err.Error(), "document_id is invalid", "error")
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
