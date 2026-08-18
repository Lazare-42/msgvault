// Package historymerge implements the one-time backfill of WhatsApp-sourced
// history from one msgvault archive home into another.
//
// It exists because live WhatsApp bridges must own an exclusive lock on
// their archive (see cmd/msgvault/cmd/write_lock.go), which forced splitting
// a combined Gmail+WhatsApp archive into a Gmail-only home and an
// isolated, WhatsApp-only home. Any WhatsApp history captured in the old
// combined home before the split is invisible to the new isolated home.
// This package replays that history across, using the same natural key
// (sources.source_type + conversations.source_conversation_id +
// messages.source_message_id) the rest of the WhatsApp write path already
// uses, so re-running it is idempotent and safe to interleave with an
// actively syncing target account.
//
// Scope is deliberately narrow: WhatsApp only, and only backfilling an
// EXISTING target source (see ResolveSourcePairs) — it never creates a new
// WhatsApp source in the target archive. It is not a general cross-archive
// sync tool.
package historymerge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// DefaultMaxAttachmentBytes bounds how large a single attachment blob this
// tool will read into memory while copying it between archive homes. WhatsApp
// media (even video) is comfortably under this in practice; it exists as a
// safety valve against a corrupt or unexpectedly huge blob, not a normal cap.
const DefaultMaxAttachmentBytes int64 = 300 * 1024 * 1024

// Options configures one merge run. From must be opened read-only (or at
// least never be written to); Into must be writable when Apply is true.
type Options struct {
	From *store.Store
	Into *store.Store

	// FromAttachmentsDir / IntoAttachmentsDir are the content-addressed
	// attachment store roots for the source and target archive homes
	// (config.Config.AttachmentsDir()). Required when either archive has
	// WhatsApp attachments with downloaded content.
	FromAttachmentsDir string
	IntoAttachmentsDir string

	// Apply controls whether Into is actually written to. When false, every
	// method in this package only issues reads (against From and Into) and
	// populates Report with what WOULD happen — safe to run at any time,
	// including while a live bridge is writing to Into.
	Apply bool

	// MaxAttachmentBytes overrides DefaultMaxAttachmentBytes when positive.
	MaxAttachmentBytes int64
}

// SourcePair is one matched (--from source, --into source) whatsapp account.
type SourcePair struct {
	From *store.Source
	Into *store.Source
}

// ResolveSourcePairs matches every WhatsApp source in from to a WhatsApp
// source in into with the same identifier (the WhatsApp JID). When
// identifierFilter is non-empty, only that identifier is considered on the
// from side. It is an error for a from-side WhatsApp source to have no
// matching into-side source: this tool backfills an existing live-synced
// account, it does not create one.
func ResolveSourcePairs(from, into *store.Store, identifierFilter string) ([]SourcePair, error) {
	if from == nil || into == nil {
		return nil, errors.New("from and into stores are required")
	}
	fromSources, err := from.ListSources(store.WhatsAppSourceType)
	if err != nil {
		return nil, fmt.Errorf("list source-archive whatsapp sources: %w", err)
	}
	if identifierFilter != "" {
		var filtered []*store.Source
		for _, s := range fromSources {
			if s.Identifier == identifierFilter {
				filtered = append(filtered, s)
			}
		}
		fromSources = filtered
	}
	if len(fromSources) == 0 {
		if identifierFilter != "" {
			return nil, fmt.Errorf("no whatsapp source with identifier %q found in --from archive", identifierFilter)
		}
		return nil, errors.New("no whatsapp source found in --from archive")
	}

	intoSources, err := into.ListSources(store.WhatsAppSourceType)
	if err != nil {
		return nil, fmt.Errorf("list target-archive whatsapp sources: %w", err)
	}
	byIdentifier := make(map[string]*store.Source, len(intoSources))
	for _, s := range intoSources {
		byIdentifier[s.Identifier] = s
	}

	var pairs []SourcePair
	var missing []string
	for _, fromSource := range fromSources {
		intoSource, ok := byIdentifier[fromSource.Identifier]
		if !ok {
			missing = append(missing, fromSource.Identifier)
			continue
		}
		pairs = append(pairs, SourcePair{From: fromSource, Into: intoSource})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"--into archive has no whatsapp source matching identifier(s) %s; "+
				"this tool backfills an EXISTING live-synced account and never "+
				"creates a new source — pair that account in --into first",
			strings.Join(missing, ", "),
		)
	}
	return pairs, nil
}

// Report summarizes one MergeSource run. Every counter is populated the same
// way regardless of Options.Apply: dry-run counters describe what WOULD
// happen, apply counters describe what DID happen.
type Report struct {
	FromIdentifier string
	IntoSourceID   int64

	Conversations int

	MessagesScanned        int
	MessagesAlreadyInTarget int
	MessagesCopied          int
	MessagesFailed          int

	AttachmentsWithContent   int
	AttachmentsAlreadyStored int
	AttachmentsCopied        int
	AttachmentsWouldCopy     int
	AttachmentMarkers        int
	AttachmentsFailed        int

	ReactionsScanned    int
	ReactionsCopied     int
	ReactionsWouldCopy  int
	ReactionsSkipped    int
	ReactionsFailed     int

	Errors []string
}

func (r *Report) recordError(msg string) {
	r.Errors = append(r.Errors, msg)
}

// MergeSource replays every WhatsApp conversation and message belonging to
// fromSourceID (a source row in opts.From) into intoSourceID (the matching
// source row in opts.Into, resolved by ResolveSourcePairs). Safe to call
// repeatedly: messages are deduplicated by (source_id, source_message_id),
// attachments by content hash.
func MergeSource(ctx context.Context, opts Options, fromSourceID, intoSourceID int64) (*Report, error) {
	if opts.From == nil || opts.Into == nil {
		return nil, errors.New("from and into stores are required")
	}

	report := &Report{IntoSourceID: intoSourceID}

	var fromBlobs *attachmentstore.Store
	if opts.FromAttachmentsDir != "" {
		blobs, err := attachmentstore.New(store.NewPackCatalog(opts.From), opts.FromAttachmentsDir)
		if err != nil {
			return nil, fmt.Errorf("open source attachment store: %w", err)
		}
		fromBlobs = blobs
		defer func() { _ = fromBlobs.Close() }()
	}

	conversations, err := opts.From.ListWhatsAppConversationsForSource(fromSourceID)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	report.Conversations = len(conversations)

	for _, conv := range conversations {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := mergeConversation(ctx, opts, intoSourceID, conv, fromBlobs, report); err != nil {
			report.recordError(fmt.Sprintf("conversation %s: %v", conv.SourceConversationID, err))
		}
	}

	if opts.Apply && report.MessagesCopied > 0 {
		if err := opts.Into.RecomputeConversationStats(intoSourceID); err != nil {
			report.recordError(fmt.Sprintf("recompute conversation stats: %v", err))
		}
	}

	return report, nil
}

func mergeConversation(
	ctx context.Context,
	opts Options,
	intoSourceID int64,
	conv store.WhatsAppMergeConversation,
	fromBlobs *attachmentstore.Store,
	report *Report,
) error {
	messages, err := opts.From.ListWhatsAppMessagesForConversation(conv.ID)
	if err != nil {
		return fmt.Errorf("list messages: %w", err)
	}
	if len(messages) == 0 {
		return nil
	}

	sourceMessageIDs := make([]string, len(messages))
	for i, m := range messages {
		sourceMessageIDs[i] = m.SourceMessageID
	}
	existing, err := opts.Into.MessageExistsBatch(intoSourceID, sourceMessageIDs)
	if err != nil {
		return fmt.Errorf("check existing messages in target: %w", err)
	}

	var intoConvID int64
	var convEnsured bool
	ensureConv := func() (int64, error) {
		if convEnsured {
			return intoConvID, nil
		}
		id, err := opts.Into.EnsureConversationWithType(
			intoSourceID, conv.SourceConversationID, conv.ConversationType, conv.Title,
		)
		if err != nil {
			return 0, err
		}
		intoConvID = id
		convEnsured = true
		return id, nil
	}

	for _, m := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		report.MessagesScanned++
		if _, ok := existing[m.SourceMessageID]; ok {
			report.MessagesAlreadyInTarget++
			continue
		}

		var intoConversationID int64
		if opts.Apply {
			id, err := ensureConv()
			if err != nil {
				report.MessagesFailed++
				report.recordError(fmt.Sprintf("ensure conversation %s: %v", conv.SourceConversationID, err))
				continue
			}
			intoConversationID = id
		}

		if err := mergeMessage(ctx, opts, intoSourceID, intoConversationID, m, fromBlobs, report); err != nil {
			report.MessagesFailed++
			report.recordError(fmt.Sprintf("message %s: %v", m.SourceMessageID, err))
			continue
		}
		report.MessagesCopied++
	}
	return nil
}

func mergeMessage(
	ctx context.Context,
	opts Options,
	intoSourceID, intoConversationID int64,
	m store.WhatsAppMergeMessage,
	fromBlobs *attachmentstore.Store,
	report *Report,
) error {
	var senderJID, senderDisplayName string
	var senderFound bool
	if m.SenderID.Valid {
		jid, displayName, found, err := opts.From.GetParticipantIdentifier(m.SenderID.Int64, store.WhatsAppIdentifierType)
		if err != nil {
			return fmt.Errorf("resolve sender identity: %w", err)
		}
		senderJID, senderDisplayName, senderFound = jid, displayName, found
	}

	var intoMessageID int64
	if opts.Apply {
		var intoSenderID sql.NullInt64
		if senderFound {
			pid, err := opts.Into.EnsureParticipantByIdentifier(store.WhatsAppIdentifierType, senderJID, senderDisplayName)
			if err != nil {
				return fmt.Errorf("ensure sender participant: %w", err)
			}
			intoSenderID = sql.NullInt64{Int64: pid, Valid: true}
			if err := opts.Into.EnsureConversationParticipant(intoConversationID, pid, "member"); err != nil {
				return fmt.Errorf("ensure conversation participant: %w", err)
			}
		}
		id, err := opts.Into.UpsertMessage(&store.Message{
			ConversationID:          intoConversationID,
			SourceID:                intoSourceID,
			SourceMessageID:         m.SourceMessageID,
			MessageType:             store.WhatsAppMessageType,
			SentAt:                  m.SentAt,
			ReceivedAt:              m.ReceivedAt,
			InternalDate:            m.InternalDate,
			SenderID:                intoSenderID,
			IsFromMe:                m.IsFromMe,
			IdentityDerivedIsFromMe: m.IdentityIsFromMe,
			Snippet:                 m.Snippet,
			SizeEstimate:            m.SizeEstimate,
		})
		if err != nil {
			return fmt.Errorf("upsert message: %w", err)
		}
		intoMessageID = id
	}

	bodyText, bodyHTML, hasBody, err := opts.From.GetMessageBodyText(m.ID)
	if err != nil {
		return fmt.Errorf("read message body: %w", err)
	}
	if opts.Apply && hasBody {
		if err := opts.Into.UpsertMessageBody(intoMessageID, bodyText, bodyHTML); err != nil {
			return fmt.Errorf("write message body: %w", err)
		}
		if err := opts.Into.UpsertFTS(intoMessageID, "", bodyText.String, senderJID, "", ""); err != nil {
			return fmt.Errorf("update fts: %w", err)
		}
	}

	raw, rawErr := opts.From.GetMessageRaw(m.ID)
	if rawErr != nil && !errors.Is(rawErr, sql.ErrNoRows) {
		return fmt.Errorf("read raw message: %w", rawErr)
	}
	if opts.Apply && rawErr == nil && len(raw) > 0 {
		if err := opts.Into.UpsertMessageRawWithFormat(intoMessageID, raw, store.WhatsAppRawFormat); err != nil {
			return fmt.Errorf("write raw message: %w", err)
		}
	}

	if err := mergeAttachments(ctx, opts, m.ID, intoMessageID, fromBlobs, report); err != nil {
		return fmt.Errorf("attachments: %w", err)
	}
	if err := mergeReactions(m.ID, intoMessageID, opts, report); err != nil {
		return fmt.Errorf("reactions: %w", err)
	}
	return nil
}

func mergeAttachments(
	ctx context.Context,
	opts Options,
	fromMessageID, intoMessageID int64,
	fromBlobs *attachmentstore.Store,
	report *Report,
) error {
	refs, err := opts.From.MessageWhatsAppAttachments(fromMessageID)
	if err != nil {
		return fmt.Errorf("list source attachments: %w", err)
	}
	if len(refs) == 0 {
		return nil
	}

	targetRefs := make([]store.AttachmentRef, 0, len(refs))
	for _, ref := range refs {
		if ref.ContentHash == "" {
			// Download-failed marker row (see storeInboundAttachment in
			// internal/whatsapp/live/service.go) — no blob to move, carry
			// the marker over as-is so the target message still shows
			// "had an attachment" instead of silently dropping it.
			report.AttachmentMarkers++
			targetRefs = append(targetRefs, ref)
			continue
		}

		hash := strings.ToLower(ref.ContentHash)
		if err := export.ValidateContentHash(hash); err != nil {
			report.AttachmentsFailed++
			report.recordError(fmt.Sprintf(
				"message %d: attachment %s has malformed content hash: %v",
				fromMessageID, ref.SourceAttachmentID, err))
			continue
		}
		ref.ContentHash = hash
		report.AttachmentsWithContent++

		loc, locErr := opts.Into.ResolveAttachmentBlob(hash)
		if locErr == nil && loc.Referenced {
			// Content-addressed: an existing reference to this hash in the
			// target archive means the blob is already stored there
			// (possibly via independent live sync of the same media).
			report.AttachmentsAlreadyStored++
			ref.StoragePath = canonicalStoragePath(hash)
			targetRefs = append(targetRefs, ref)
			continue
		}

		if !opts.Apply {
			report.AttachmentsWouldCopy++
			continue
		}

		copied, copyErr := copyAttachmentBytes(ctx, opts, fromBlobs, ref)
		if copyErr != nil {
			report.AttachmentsFailed++
			report.recordError(fmt.Sprintf(
				"message %d: copy attachment %s: %v", fromMessageID, hash, copyErr))
			continue
		}
		report.AttachmentsCopied++
		targetRefs = append(targetRefs, copied)
	}

	if !opts.Apply || len(targetRefs) == 0 {
		return nil
	}
	if err := opts.Into.ReplaceMessageWhatsAppAttachments(intoMessageID, targetRefs); err != nil {
		return fmt.Errorf("write attachments: %w", err)
	}
	if err := opts.Into.RecomputeMessageAttachmentStats(intoMessageID); err != nil {
		return fmt.Errorf("recompute attachment stats: %w", err)
	}
	return nil
}

// copyAttachmentBytes streams ref's content from the source archive's
// attachment store (packed CAS or loose, transparently) and writes it into
// the target archive's content-addressed store. export.StoreAttachmentFile
// verifies the hash and de-duplicates against any blob already on disk at
// the target, so this is safe to re-run.
func copyAttachmentBytes(ctx context.Context, opts Options, fromBlobs *attachmentstore.Store, ref store.AttachmentRef) (store.AttachmentRef, error) {
	if fromBlobs == nil {
		return store.AttachmentRef{}, errors.New("source archive has no attachments directory configured")
	}
	maxBytes := opts.MaxAttachmentBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxAttachmentBytes
	}

	rc, size, err := fromBlobs.OpenStream(ctx, ref.ContentHash)
	if err != nil {
		return store.AttachmentRef{}, fmt.Errorf("open source attachment: %w", err)
	}
	defer func() { _ = rc.Close() }()
	if size > maxBytes {
		return store.AttachmentRef{}, fmt.Errorf("attachment is %d bytes, exceeds max %d bytes", size, maxBytes)
	}

	data, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return store.AttachmentRef{}, fmt.Errorf("read source attachment: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return store.AttachmentRef{}, fmt.Errorf("attachment exceeds max size %d bytes", maxBytes)
	}

	att := &mime.Attachment{
		Filename:    ref.Filename,
		ContentType: ref.MimeType,
		Content:     data,
		ContentHash: ref.ContentHash,
	}
	storagePath, err := export.StoreAttachmentFile(opts.IntoAttachmentsDir, att)
	if err != nil {
		return store.AttachmentRef{}, fmt.Errorf("store attachment: %w", err)
	}

	out := ref
	out.StoragePath = storagePath
	out.ContentHash = att.ContentHash
	out.Size = len(data)
	return out, nil
}

func canonicalStoragePath(hash string) string {
	return hash[:2] + "/" + hash
}

func mergeReactions(fromMessageID, intoMessageID int64, opts Options, report *Report) error {
	reactions, err := opts.From.ListActiveWhatsAppReactions(fromMessageID)
	if err != nil {
		return fmt.Errorf("list source reactions: %w", err)
	}
	for _, r := range reactions {
		report.ReactionsScanned++
		jid, displayName, found, err := opts.From.GetParticipantIdentifier(r.ParticipantID, store.WhatsAppIdentifierType)
		if err != nil {
			report.ReactionsFailed++
			report.recordError(fmt.Sprintf("message %d: resolve reactor: %v", fromMessageID, err))
			continue
		}
		if !found {
			report.ReactionsSkipped++
			continue
		}
		if !opts.Apply {
			report.ReactionsWouldCopy++
			continue
		}
		pid, err := opts.Into.EnsureParticipantByIdentifier(store.WhatsAppIdentifierType, jid, displayName)
		if err != nil {
			report.ReactionsFailed++
			report.recordError(fmt.Sprintf("message %d: ensure reactor participant: %v", fromMessageID, err))
			continue
		}
		if err := opts.Into.UpsertReaction(intoMessageID, pid, r.ReactionType, r.ReactionValue, r.CreatedAt); err != nil {
			report.ReactionsFailed++
			report.recordError(fmt.Sprintf("message %d: write reaction: %v", fromMessageID, err))
			continue
		}
		report.ReactionsCopied++
	}
	return nil
}
