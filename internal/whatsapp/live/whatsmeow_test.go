package live

import (
	"context"
	"errors"
	stdmime "mime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// fakeMediaDownloader is a minimal mediaDownloader used to exercise
// convertMessage/downloadMediaAttachment without a live whatsmeow client
// connection or real WhatsApp CDN traffic.
type fakeMediaDownloader struct {
	data    []byte
	err     error
	calls   int
	lastMsg whatsmeow.DownloadableMessage
}

func (f *fakeMediaDownloader) Download(_ context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	f.calls++
	f.lastMsg = msg
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

func testMessageInfo(id types.MessageID) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:    types.NewJID("120363", types.GroupServer),
			Sender:  types.NewJID("15557654321", types.DefaultUserServer),
			IsGroup: true,
		},
		ID:        id,
		Timestamp: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}
}

func TestExtensionForMimetype(t *testing.T) {
	// Go's builtin/OS mime registry is environment-dependent (different
	// systems register different extensions for well-known types, and
	// ExtensionsByType returns them sorted, so picking exts[0] can vary by
	// platform). Register a synthetic type unique to this test so the
	// assertions do not depend on the host's mime database.
	require.NoError(t, stdmime.AddExtensionType(".mvtestext", "application/x-msgvault-live-test"))

	tests := []struct {
		name     string
		mimetype string
		want     string
	}{
		{"registered type", "application/x-msgvault-live-test", ".mvtestext"},
		{"registered type with params", "application/x-msgvault-live-test; charset=binary", ".mvtestext"},
		{"unregistered type", "application/x-msgvault-live-test-unregistered", ""},
		{"empty type", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extensionForMimetype(tt.mimetype))
		})
	}
}

func TestMediaFilename(t *testing.T) {
	require.NoError(t, stdmime.AddExtensionType(".mvtestext", "application/x-msgvault-live-test"))

	tests := []struct {
		name     string
		base     string
		mimetype string
		kind     string
		want     string
	}{
		{"empty base falls back to kind", "", "application/x-msgvault-live-test", "image", "image.mvtestext"},
		{"base gets extension appended", "My Photo", "application/x-msgvault-live-test", "image", "My Photo.mvtestext"},
		{"existing suffix is not duplicated", "My Photo.mvtestext", "application/x-msgvault-live-test", "image", "My Photo.mvtestext"},
		{"unknown mimetype leaves base unmodified", "My Photo", "application/x-msgvault-live-test-unregistered", "image", "My Photo"},
		{"unknown mimetype and empty base falls back to bare kind", "", "application/x-msgvault-live-test-unregistered", "document", "document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mediaFilename(tt.base, tt.mimetype, tt.kind))
		})
	}
}

func TestExtractMediaMessage(t *testing.T) {
	t.Run("nil message", func(t *testing.T) {
		downloadable, meta, ok := extractMediaMessage(nil)
		assert.False(t, ok)
		assert.Nil(t, downloadable)
		assert.Equal(t, waMediaMeta{}, meta)
	})

	t.Run("no media", func(t *testing.T) {
		_, _, ok := extractMediaMessage(&waE2E.Message{Conversation: proto.String("hello")})
		assert.False(t, ok)
	})

	t.Run("image", func(t *testing.T) {
		img := &waE2E.ImageMessage{
			Mimetype:   proto.String("image/jpeg"),
			FileLength: proto.Uint64(1024),
			MediaKey:   []byte("mediakey"),
			DirectPath: proto.String("/mms/image/abc"),
		}
		downloadable, meta, ok := extractMediaMessage(&waE2E.Message{ImageMessage: img})
		require.True(t, ok)
		assert.Same(t, img, downloadable)
		assert.Equal(t, "image/jpeg", meta.mimetype)
		assert.Equal(t, "image", meta.mediaType)
		assert.EqualValues(t, 1024, meta.size)
		assert.True(t, strings.HasPrefix(meta.filename, "image"))
	})

	t.Run("video", func(t *testing.T) {
		vid := &waE2E.VideoMessage{
			Mimetype:   proto.String("video/mp4"),
			FileLength: proto.Uint64(2048),
			MediaKey:   []byte("mediakey"),
			DirectPath: proto.String("/mms/video/abc"),
		}
		downloadable, meta, ok := extractMediaMessage(&waE2E.Message{VideoMessage: vid})
		require.True(t, ok)
		assert.Same(t, vid, downloadable)
		assert.Equal(t, "video/mp4", meta.mimetype)
		assert.Equal(t, "video", meta.mediaType)
		assert.EqualValues(t, 2048, meta.size)
	})

	t.Run("document with filename", func(t *testing.T) {
		doc := &waE2E.DocumentMessage{
			FileName:   proto.String("RIB.pdf"),
			Mimetype:   proto.String("application/pdf"),
			FileLength: proto.Uint64(4096),
			MediaKey:   []byte("mediakey"),
			DirectPath: proto.String("/mms/document/abc"),
		}
		downloadable, meta, ok := extractMediaMessage(&waE2E.Message{DocumentMessage: doc})
		require.True(t, ok)
		assert.Same(t, doc, downloadable)
		assert.Equal(t, "RIB.pdf", meta.filename)
		assert.Equal(t, "application/pdf", meta.mimetype)
		assert.Equal(t, "document", meta.mediaType)
		assert.EqualValues(t, 4096, meta.size)
	})

	t.Run("document without filename falls back to title", func(t *testing.T) {
		doc := &waE2E.DocumentMessage{
			Title:      proto.String("Bank Details"),
			Mimetype:   proto.String("application/pdf"),
			FileLength: proto.Uint64(4096),
		}
		_, meta, ok := extractMediaMessage(&waE2E.Message{DocumentMessage: doc})
		require.True(t, ok)
		assert.True(t, strings.HasPrefix(meta.filename, "Bank Details"))
	})

	t.Run("audio voice note", func(t *testing.T) {
		aud := &waE2E.AudioMessage{
			Mimetype:   proto.String("audio/ogg"),
			FileLength: proto.Uint64(512),
			PTT:        proto.Bool(true),
		}
		_, meta, ok := extractMediaMessage(&waE2E.Message{AudioMessage: aud})
		require.True(t, ok)
		assert.Equal(t, "voice_note", meta.mediaType)
	})

	t.Run("audio regular", func(t *testing.T) {
		aud := &waE2E.AudioMessage{
			Mimetype:   proto.String("audio/mpeg"),
			FileLength: proto.Uint64(512),
		}
		_, meta, ok := extractMediaMessage(&waE2E.Message{AudioMessage: aud})
		require.True(t, ok)
		assert.Equal(t, "audio", meta.mediaType)
	})

	t.Run("sticker", func(t *testing.T) {
		sticker := &waE2E.StickerMessage{
			Mimetype:   proto.String("image/webp"),
			FileLength: proto.Uint64(128),
		}
		downloadable, meta, ok := extractMediaMessage(&waE2E.Message{StickerMessage: sticker})
		require.True(t, ok)
		assert.Same(t, sticker, downloadable)
		assert.Equal(t, "sticker", meta.mediaType)
	})
}

func TestConvertMessageCaptionlessMediaIsArchived(t *testing.T) {
	dl := &fakeMediaDownloader{data: []byte("%PDF-1.4 synthetic bank details")}
	transport := &WhatsmeowTransport{account: "15551234567@s.whatsapp.net", downloader: dl}
	evt := &events.Message{
		Info: testMessageInfo("msg-1"),
		Message: &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				FileName:   proto.String("RIB.pdf"),
				Mimetype:   proto.String("application/pdf"),
				FileLength: proto.Uint64(uint64(len("%PDF-1.4 synthetic bank details"))),
				MediaKey:   []byte("mediakey"),
				DirectPath: proto.String("/mms/document/abc"),
				// No Caption set: this is the captionless-attachment bug case.
			},
		},
	}

	inbound, ok := transport.convertMessage(context.Background(), evt)
	require.True(t, ok, "a captionless media message must still be archived")
	assert.Empty(t, inbound.Text)
	require.NotNil(t, inbound.Attachment)
	assert.Equal(t, "RIB.pdf", inbound.Attachment.Filename)
	assert.Equal(t, "application/pdf", inbound.Attachment.MimeType)
	assert.Equal(t, "document", inbound.Attachment.MediaType)
	assert.Equal(t, []byte("%PDF-1.4 synthetic bank details"), inbound.Attachment.Data)
	assert.Empty(t, inbound.Attachment.DownloadError)
	assert.Equal(t, 1, dl.calls)
}

func TestConvertMessageMediaWithCaptionKeepsBothTextAndAttachment(t *testing.T) {
	dl := &fakeMediaDownloader{data: []byte("image bytes")}
	transport := &WhatsmeowTransport{account: "15551234567@s.whatsapp.net", downloader: dl}
	evt := &events.Message{
		Info: testMessageInfo("msg-2"),
		Message: &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				Caption:    proto.String("check this out"),
				Mimetype:   proto.String("image/jpeg"),
				FileLength: proto.Uint64(11),
				MediaKey:   []byte("mediakey"),
				DirectPath: proto.String("/mms/image/abc"),
			},
		},
	}

	inbound, ok := transport.convertMessage(context.Background(), evt)
	require.True(t, ok)
	assert.Equal(t, "check this out", inbound.Text)
	require.NotNil(t, inbound.Attachment)
	assert.Equal(t, []byte("image bytes"), inbound.Attachment.Data)
}

func TestConvertMessageDownloadFailureStillArchivesMessage(t *testing.T) {
	dl := &fakeMediaDownloader{err: errors.New("media key expired")}
	transport := &WhatsmeowTransport{account: "15551234567@s.whatsapp.net", downloader: dl}
	evt := &events.Message{
		Info: testMessageInfo("msg-3"),
		Message: &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				FileName:   proto.String("RIB.pdf"),
				Mimetype:   proto.String("application/pdf"),
				FileLength: proto.Uint64(4096),
				MediaKey:   []byte("mediakey"),
				DirectPath: proto.String("/mms/document/abc"),
			},
		},
	}

	inbound, ok := transport.convertMessage(context.Background(), evt)
	require.True(t, ok, "a download failure must not drop the message")
	require.NotNil(t, inbound.Attachment)
	assert.Empty(t, inbound.Attachment.Data)
	assert.Contains(t, inbound.Attachment.DownloadError, "media key expired")
	// Metadata gathered ahead of the download attempt is preserved even
	// though the bytes themselves could not be fetched.
	assert.Equal(t, "RIB.pdf", inbound.Attachment.Filename)
	assert.Equal(t, "application/pdf", inbound.Attachment.MimeType)
}

func TestConvertMessageNoDownloaderRecordsErrorWithoutDroppingMessage(t *testing.T) {
	transport := &WhatsmeowTransport{account: "15551234567@s.whatsapp.net", downloader: nil}
	evt := &events.Message{
		Info: testMessageInfo("msg-4"),
		Message: &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				FileName:   proto.String("RIB.pdf"),
				Mimetype:   proto.String("application/pdf"),
				FileLength: proto.Uint64(4096),
			},
		},
	}

	inbound, ok := transport.convertMessage(context.Background(), evt)
	require.True(t, ok)
	require.NotNil(t, inbound.Attachment)
	assert.Empty(t, inbound.Attachment.Data)
	assert.NotEmpty(t, inbound.Attachment.DownloadError)
}

func TestConvertMessageTextOnlyMessageHasNoAttachment(t *testing.T) {
	transport := &WhatsmeowTransport{account: "15551234567@s.whatsapp.net", downloader: &fakeMediaDownloader{}}
	evt := &events.Message{
		Info:    testMessageInfo("msg-5"),
		Message: &waE2E.Message{Conversation: proto.String("hello there")},
	}

	inbound, ok := transport.convertMessage(context.Background(), evt)
	require.True(t, ok)
	assert.Equal(t, "hello there", inbound.Text)
	assert.Nil(t, inbound.Attachment)
}

func TestConvertMessageEmptyMessageIsDropped(t *testing.T) {
	transport := &WhatsmeowTransport{account: "15551234567@s.whatsapp.net", downloader: &fakeMediaDownloader{}}
	evt := &events.Message{
		Info:    testMessageInfo("msg-6"),
		Message: &waE2E.Message{},
	}

	_, ok := transport.convertMessage(context.Background(), evt)
	assert.False(t, ok, "a message with neither text nor media must still be dropped")
}
