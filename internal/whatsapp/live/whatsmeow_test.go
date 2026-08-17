package live

import (
	"context"
	"errors"
	"fmt"
	"math"
	stdmime "mime"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
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

func TestMessageTextLocationFallsBackToCoordinates(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{
			name: "location with name",
			msg: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
				DegreesLatitude: proto.Float64(48.8566), DegreesLongitude: proto.Float64(2.3522),
				Name: proto.String("Eiffel Tower"),
			}},
			want: "Eiffel Tower",
		},
		{
			name: "location with address only",
			msg: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
				DegreesLatitude: proto.Float64(48.8566), DegreesLongitude: proto.Float64(2.3522),
				Address: proto.String("Champ de Mars, Paris"),
			}},
			want: "Champ de Mars, Paris",
		},
		{
			name: "bare location pin falls back to coordinates",
			msg: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
				DegreesLatitude: proto.Float64(48.8566), DegreesLongitude: proto.Float64(2.3522),
			}},
			want: "📍 48.8566, 2.3522",
		},
		{
			name: "live location with caption",
			msg: &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
				DegreesLatitude: proto.Float64(40.7128), DegreesLongitude: proto.Float64(-74.0060),
				Caption: proto.String("on my way"),
			}},
			want: "on my way",
		},
		{
			name: "bare live location falls back to coordinates",
			msg: &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
				DegreesLatitude: proto.Float64(40.7128), DegreesLongitude: proto.Float64(-74.0060),
			}},
			want: "📍 40.7128, -74.0060",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MessageText(tt.msg))
		})
	}
}

func TestConvertMessageBareLocationPinIsArchived(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
	}{
		{
			name: "location",
			msg: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
				DegreesLatitude: proto.Float64(48.8566), DegreesLongitude: proto.Float64(2.3522),
			}},
		},
		{
			name: "live location",
			msg: &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
				DegreesLatitude: proto.Float64(48.8566), DegreesLongitude: proto.Float64(2.3522),
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &WhatsmeowTransport{account: "15551234567@s.whatsapp.net", downloader: &fakeMediaDownloader{}}
			evt := &events.Message{Info: testMessageInfo("msg-loc"), Message: tt.msg}

			inbound, ok := transport.convertMessage(context.Background(), evt)
			require.True(t, ok, "a bare location pin must still be archived, not dropped")
			assert.NotEmpty(t, inbound.Text)
			assert.Contains(t, inbound.Text, "48.8566")
			assert.Contains(t, inbound.Text, "2.3522")
			assert.Nil(t, inbound.Attachment, "location messages are not downloadable media")
		})
	}
}

// TestNewWhatsmeowTransportAndResetClientKeepDownloaderInSync exercises the
// real NewWhatsmeowTransport/resetClient path (not a bare struct literal) so
// a regression that drops resetClient's `t.downloader = client` (or
// `t.peerSender = client`) resync — see downloadMediaAttachment and
// RequestHistorySync, which both read their client-scoped field under t.mu
// specifically because of that race — would fail this test. Both calls are
// fully local (sqlite session store + in-memory client construction), so no
// live connection or pairing is needed.
func TestNewWhatsmeowTransportAndResetClientKeepDownloaderInSync(t *testing.T) {
	dir := t.TempDir()
	transport, err := NewWhatsmeowTransport(context.Background(), WhatsmeowOptions{
		SessionPath: filepath.Join(dir, "session.db"),
		Account:     "15551234567@s.whatsapp.net",
	})
	require.NoError(t, err)
	defer func() { _ = transport.Close() }()

	require.NotNil(t, transport.downloader)
	assert.Same(t, transport.client, transport.downloader.(*whatsmeow.Client))
	require.NotNil(t, transport.peerSender)
	assert.Same(t, transport.client, transport.peerSender.(*whatsmeow.Client))

	oldClient := transport.client
	require.NoError(t, transport.resetClient(context.Background()))
	assert.NotSame(t, oldClient, transport.client, "resetClient must install a fresh client")
	require.NotNil(t, transport.downloader)
	assert.Same(t, transport.client, transport.downloader.(*whatsmeow.Client),
		"downloader must be resynced to the new client after resetClient")
	require.NotNil(t, transport.peerSender)
	assert.Same(t, transport.client, transport.peerSender.(*whatsmeow.Client),
		"peerSender must be resynced to the new client after resetClient")
}

// fakePeerMessageSender captures the *waE2E.Message passed to SendPeerMessage
// so RequestHistorySync tests can assert on whatsmeow's own
// BuildHistorySyncRequest output without a live, authenticated session (see
// peerMessageSender's doc comment).
type fakePeerMessageSender struct {
	sent *waE2E.Message
	err  error
}

func (f *fakePeerMessageSender) SendPeerMessage(_ context.Context, message *waE2E.Message) (whatsmeow.SendResponse, error) {
	f.sent = message
	if f.err != nil {
		return whatsmeow.SendResponse{}, f.err
	}
	return whatsmeow.SendResponse{ID: "peer-resp-1"}, nil
}

// newRequestHistorySyncTestTransport builds a real *WhatsmeowTransport (so
// BuildHistorySyncRequest runs as a genuine *whatsmeow.Client method, the
// same production code path used live) with peerSender swapped for a fake,
// since SendPeerMessage itself requires a live, authenticated connection.
func newRequestHistorySyncTestTransport(t *testing.T) (*WhatsmeowTransport, *fakePeerMessageSender) {
	t.Helper()
	dir := t.TempDir()
	transport, err := NewWhatsmeowTransport(context.Background(), WhatsmeowOptions{
		SessionPath: filepath.Join(dir, "session.db"),
		Account:     "15551234567@s.whatsapp.net",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = transport.Close() })
	sender := &fakePeerMessageSender{}
	transport.peerSender = sender
	return transport, sender
}

func TestRequestHistorySyncBuildsAndSendsOnDemandRequest(t *testing.T) {
	transport, sender := newRequestHistorySyncTestTransport(t)
	anchorTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	err := transport.RequestHistorySync(context.Background(), TransportRequestHistorySyncRequest{
		ChatJID:         "120363@g.us",
		AnchorMessageID: "ANCHOR123",
		AnchorTimestamp: anchorTime,
		AnchorIsFromMe:  true,
		Count:           25,
	})
	require.NoError(t, err)
	require.NotNil(t, sender.sent)

	req := sender.sent.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
	require.NotNil(t, req)
	assert.Equal(t, waE2E.PeerDataOperationRequestType_HISTORY_SYNC_ON_DEMAND,
		sender.sent.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPeerDataOperationRequestType())
	assert.Equal(t, "120363@g.us", req.GetChatJID())
	assert.Equal(t, "ANCHOR123", req.GetOldestMsgID())
	assert.True(t, req.GetOldestMsgFromMe())
	assert.EqualValues(t, 25, req.GetOnDemandMsgCount())
	// The field is named "...TimestampMS" but whatsmeow's own doc comment on
	// BuildHistorySyncRequest notes it actually holds seconds.
	assert.Equal(t, anchorTime.Unix(), req.GetOldestMsgTimestampMS())
}

func TestRequestHistorySyncDefaultsAndClampsCount(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"zero defaults", 0, DefaultHistorySyncRequestCount},
		{"negative defaults", -1, DefaultHistorySyncRequestCount},
		{"within range kept", 30, 30},
		{"over max clamped", 500, MaxHistorySyncRequestCount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport, sender := newRequestHistorySyncTestTransport(t)
			err := transport.RequestHistorySync(context.Background(), TransportRequestHistorySyncRequest{
				ChatJID:         "120363@g.us",
				AnchorMessageID: "anchor",
				AnchorTimestamp: time.Now(),
				Count:           tt.in,
			})
			require.NoError(t, err)
			req := sender.sent.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
			require.NotNil(t, req)
			assert.Equal(t, tt.want, req.GetOnDemandMsgCount())
		})
	}
}

func TestRequestHistorySyncRequiresAnchorMessageID(t *testing.T) {
	transport, sender := newRequestHistorySyncTestTransport(t)
	err := transport.RequestHistorySync(context.Background(), TransportRequestHistorySyncRequest{
		ChatJID: "120363@g.us",
	})
	require.Error(t, err)
	assert.Nil(t, sender.sent, "must not send when the anchor is missing")
}

func TestRequestHistorySyncRejectsInvalidChatJID(t *testing.T) {
	transport, sender := newRequestHistorySyncTestTransport(t)
	err := transport.RequestHistorySync(context.Background(), TransportRequestHistorySyncRequest{
		ChatJID:         "",
		AnchorMessageID: "anchor",
	})
	require.Error(t, err)
	assert.Nil(t, sender.sent)
}

func TestRequestHistorySyncPropagatesSendError(t *testing.T) {
	transport, sender := newRequestHistorySyncTestTransport(t)
	sender.err = errors.New("boom")

	err := transport.RequestHistorySync(context.Background(), TransportRequestHistorySyncRequest{
		ChatJID:         "120363@g.us",
		AnchorMessageID: "anchor",
		AnchorTimestamp: time.Now(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestMediaDownloadTimeoutScalesWithDeclaredSize(t *testing.T) {
	tests := []struct {
		name         string
		declaredSize int64
		want         time.Duration
	}{
		{"unknown size uses floor", 0, 10 * time.Minute},
		{"negative size uses floor", -1, 10 * time.Minute},
		{"small file uses floor", 1 << 20, 10 * time.Minute},
		{"large file scales above floor", 200 << 20, time.Duration(int64(200<<20)/(128<<10)) * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mediaDownloadTimeout(tt.declaredSize)
			assert.Equal(t, tt.want, got)
			assert.GreaterOrEqual(t, got, 10*time.Minute, "must never be below the floor")
		})
	}
}

func TestFileLengthInt64ClampsOverflow(t *testing.T) {
	tests := []struct {
		name string
		in   uint64
		want int64
	}{
		{"zero", 0, 0},
		{"typical attachment size", 4096, 4096},
		{"max safe int64", math.MaxInt64, math.MaxInt64},
		{"overflowing value becomes unknown", math.MaxInt64 + 1, 0},
		{"uint64 max becomes unknown", math.MaxUint64, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fileLengthInt64(tt.in)
			assert.Equal(t, tt.want, got)
			assert.GreaterOrEqual(t, got, int64(0), "must never wrap negative")
		})
	}
}

func TestExtractMediaMessageAdversarialFileLengthDoesNotGoNegative(t *testing.T) {
	doc := &waE2E.DocumentMessage{
		FileName:   proto.String("bomb.bin"),
		Mimetype:   proto.String("application/octet-stream"),
		FileLength: proto.Uint64(math.MaxUint64),
	}
	_, meta, ok := extractMediaMessage(&waE2E.Message{DocumentMessage: doc})
	require.True(t, ok)
	assert.Zero(t, meta.size, "an unrepresentable declared size must become unknown (0), not negative")
}

func TestDownloadMediaAttachmentRejectsOversizedDeclaredSize(t *testing.T) {
	dl := &fakeMediaDownloader{data: []byte("should never be fetched")}
	transport := &WhatsmeowTransport{account: "15551234567@s.whatsapp.net", downloader: dl, log: newWhatsmeowLogger("")}
	msg := &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{
			FileName:   proto.String("huge.bin"),
			Mimetype:   proto.String("application/octet-stream"),
			FileLength: proto.Uint64(uint64(maxMediaDownloadBytes) + 1),
			MediaKey:   []byte("mediakey"),
			DirectPath: proto.String("/mms/document/abc"),
		},
	}

	attachment := transport.downloadMediaAttachment(context.Background(), msg)
	require.NotNil(t, attachment)
	assert.Empty(t, attachment.Data)
	assert.NotEmpty(t, attachment.DownloadError)
	assert.Equal(t, 0, dl.calls, "an oversized declared size must be rejected before the network call")
}

// concurrencyTrackingDownloader is a mediaDownloader that records the
// maximum number of Download calls observed in flight at once, and blocks
// each call for delay so overlapping calls are observable.
type concurrencyTrackingDownloader struct {
	data  []byte
	delay time.Duration

	mu      sync.Mutex
	current int
	max     int
}

func (d *concurrencyTrackingDownloader) Download(ctx context.Context, _ whatsmeow.DownloadableMessage) ([]byte, error) {
	d.mu.Lock()
	d.current++
	if d.current > d.max {
		d.max = d.current
	}
	d.mu.Unlock()

	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
	}

	d.mu.Lock()
	d.current--
	d.mu.Unlock()
	return d.data, nil
}

func (d *concurrencyTrackingDownloader) maxConcurrent() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.max
}

// buildHistoryConversation constructs a synthetic history-sync conversation
// with count image messages, for exercising archiveHistorySync without a
// live whatsmeow connection (WhatsmeowTransport.client.ParseWebMessage is
// purely local for a non-from-me message with a resolvable chat JID).
func buildHistoryConversation(chatJID string, count int) *waHistorySync.Conversation {
	messages := make([]*waHistorySync.HistorySyncMsg, count)
	for i := 0; i < count; i++ {
		messages[i] = &waHistorySync.HistorySyncMsg{
			Message: &waWeb.WebMessageInfo{
				Key: &waCommon.MessageKey{
					RemoteJID: proto.String(chatJID),
					FromMe:    proto.Bool(false),
					ID:        proto.String(fmt.Sprintf("hist-%s-%d", chatJID, i)),
				},
				MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
				Message: &waE2E.Message{
					ImageMessage: &waE2E.ImageMessage{
						Mimetype:   proto.String("image/jpeg"),
						FileLength: proto.Uint64(11),
						MediaKey:   []byte("mediakey"),
						DirectPath: proto.String("/mms/image/abc"),
					},
				},
			},
		}
	}
	return &waHistorySync.Conversation{ID: proto.String(chatJID), Messages: messages}
}

// TestArchiveHistorySyncPersistsPerConversationWithBoundedConcurrency covers
// two history-sync regressions together: (1) every conversation used to be
// accumulated in memory for one write at the very end, losing all progress on
// a crash or cancellation partway through a large sync; (2) messages were
// converted (including any media download) fully serially, so a large backlog
// of media could take hours. Uses a real WhatsmeowTransport (NewWhatsmeowTransport)
// because archiveHistorySync calls the concrete *whatsmeow.Client's
// ParseWebMessage/Log, which a bare struct literal cannot supply.
func TestArchiveHistorySyncPersistsPerConversationWithBoundedConcurrency(t *testing.T) {
	dir := t.TempDir()
	transport, err := NewWhatsmeowTransport(context.Background(), WhatsmeowOptions{
		SessionPath: filepath.Join(dir, "session.db"),
		Account:     "15551234567@s.whatsapp.net",
	})
	require.NoError(t, err)
	defer func() { _ = transport.Close() }()

	dl := &concurrencyTrackingDownloader{data: []byte("photo bytes"), delay: 25 * time.Millisecond}
	transport.downloader = dl

	var mu sync.Mutex
	var batches [][]InboundMessage
	transport.SetHistorySyncHandler(func(_ context.Context, messages []InboundMessage) error {
		mu.Lock()
		batches = append(batches, append([]InboundMessage(nil), messages...))
		mu.Unlock()
		return nil
	})

	const perConversation = historySyncDownloadConcurrency * 2
	event := &events.HistorySync{Data: &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{
			buildHistoryConversation("15557654321@s.whatsapp.net", perConversation),
			buildHistoryConversation("15558765432@s.whatsapp.net", perConversation),
		},
	}}

	transport.archiveHistorySync(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, batches, 2, "each conversation with messages must be persisted as its own batch, not accumulated into one")
	for _, batch := range batches {
		assert.Len(t, batch, perConversation)
	}
	assert.LessOrEqual(t, dl.maxConcurrent(), historySyncDownloadConcurrency, "must not exceed the configured worker pool bound")
	assert.Greater(t, dl.maxConcurrent(), 1, "downloads within a conversation must run concurrently, not serially")
}
