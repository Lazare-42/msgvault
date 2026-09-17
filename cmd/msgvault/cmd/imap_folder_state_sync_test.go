package cmd

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// listedIMAPClient returns an IMAP client that has enumerated the test
// server, so it holds observed folder states ready for persistence. It
// always wires WithFolderStateSave, matching production's
// imapFolderStateOptions, which activates per-mailbox completion tracking:
// a mailbox only appears in ObservedFolderStates once every message it
// listed has been acknowledged via AcknowledgeMessages. Callers that want a
// mailbox to look individually verified must acknowledge its observed
// messages explicitly — see acknowledgeMailboxObservations.
func listedIMAPClient(t *testing.T, addr string, opts ...imaplib.Option) *imaplib.Client {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	allOpts := append([]imaplib.Option{
		imaplib.WithFolderStateSave(func(string, imaplib.FolderState) {}),
	}, opts...)
	client := imaplib.NewClient(&imaplib.Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword, allOpts...)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pageToken := ""
	for {
		resp, err := client.ListMessages(ctx, "", pageToken)
		require.NoError(t, err)
		if resp.NextPageToken == "" {
			return client
		}
		pageToken = resp.NextPageToken
	}
}

// acknowledgeMailboxObservations tells client that every message it observed
// in mailbox was safely fetched and persisted this session — standing in for
// what the real syncer does via AcknowledgeMessages once ingest succeeds.
// Returns the acknowledged source message IDs.
func acknowledgeMailboxObservations(t *testing.T, client *imaplib.Client, mailbox string) []string {
	t.Helper()
	var ids []string
	for _, observation := range client.ObservedMemberships() {
		if observation.Mailbox == mailbox {
			ids = append(ids, observation.SourceMessageID)
		}
	}
	client.AcknowledgeMessages(context.Background(), ids)
	return ids
}

// syncMailboxMessages drives client through the two production steps a real
// sync performs for a mailbox's listed messages: a raw FETCH (which is what
// actually records membership — see buildMessageListCache's
// fullScanWithoutAll — any folder-filtered session, or one against a server
// with no \All mailbox once more than one mailbox is in play, never builds a
// label map during listing, so ObservedMemberships stays empty until the
// real per-message fetch that follows) and AcknowledgeMessages. Returns the
// composite source message IDs fetched.
func syncMailboxMessages(t *testing.T, client *imaplib.Client, mailbox string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.ListMessages(ctx, "", "")
	require.NoError(t, err)
	var ids []string
	for _, m := range resp.Messages {
		mb, parseErr := imaplib.SourceMailboxFromMessageID(m.ID)
		require.NoError(t, parseErr)
		if mb == mailbox {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) > 0 {
		_, err = client.GetMessagesRawBatchWithErrors(ctx, ids)
		require.NoError(t, err)
	}
	client.AcknowledgeMessages(ctx, ids)
	return ids
}

func imapMembershipRowCount(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?`,
	), sourceID).Scan(&count))
	return count
}

func imapTombstonedMessageCount(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages
		 WHERE source_id = ? AND deleted_from_source_at IS NOT NULL`,
	), sourceID).Scan(&count))
	return count
}

func seedObservedIMAPMessages(
	t *testing.T, st *store.Store, src *store.Source, client *imaplib.Client,
) {
	t.Helper()
	conversationID, err := st.EnsureConversation(src.ID, "imap-membership", "IMAP membership")
	require.NoError(t, err)
	for _, observation := range client.ObservedMemberships() {
		_, err := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        src.ID,
			SourceMessageID: observation.SourceMessageID,
			RFC822MessageID: sql.NullString{
				String: observation.RFC822MessageID,
				Valid:  observation.RFC822MessageID != "",
			},
			MessageType: "email",
		})
		require.NoError(t, err)
	}
}

func seedIMAPMessage(
	t *testing.T, st *store.Store, src *store.Source, sourceMessageID, rfc822MessageID string,
) {
	t.Helper()
	conversationID, err := st.EnsureConversation(src.ID, "imap-delta", "IMAP delta")
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        src.ID,
		SourceMessageID: sourceMessageID,
		RFC822MessageID: sql.NullString{String: rfc822MessageID, Valid: rfc822MessageID != ""},
		MessageType:     "email",
	})
	require.NoError(t, err)
}

func completedIMAPSyncSummary(
	t *testing.T, st *store.Store, src *store.Source,
) *gmail.SyncSummary {
	t.Helper()
	syncRunID, err := st.StartSync(src.ID, "full")
	require.NoError(t, err)
	require.NoError(t, st.CompleteSync(syncRunID, "0"))
	return &gmail.SyncSummary{SyncRunID: syncRunID}
}

func TestSaveIMAPFolderStates_CleanRunPersists(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"INBOX": {
			UIDValidity: loaded["INBOX"].UIDValidity, UIDNext: 3,
			HighestModSeq: loaded["INBOX"].HighestModSeq, KnownUIDs: []uint32{1, 2},
		},
		"Archive": {
			UIDValidity: loaded["Archive"].UIDValidity, UIDNext: 2,
			HighestModSeq: loaded["Archive"].HighestModSeq, KnownUIDs: []uint32{1},
		},
	}, loaded)
}

func TestSaveIMAPFolderStates_SupersededGenerationCannotOverwriteNewerRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://generation@example.com")
	require.NoError(err)
	require.NoError(st.UpsertIMAPFolderStates(src.ID, []store.IMAPFolderState{{
		Mailbox: "Prior", UIDValidity: 7, UIDNext: 9, HighestModSeq: 11,
	}}))

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	oldRunID, err := st.StartSync(src.ID, "full")
	require.NoError(err)
	require.NoError(st.CompleteSync(oldRunID, "0"))
	newRunID, err := st.StartSync(src.ID, "full")
	require.NoError(err)

	err = saveIMAPFolderStates(
		context.Background(), st, src, client, &gmail.SyncSummary{SyncRunID: oldRunID}, 0)
	require.ErrorIs(err, store.ErrSyncRunSuperseded)

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(map[string]imaplib.FolderState{
		"Prior": {
			UIDValidity: 7, UIDNext: 9, HighestModSeq: 11, KnownUIDs: []uint32{},
		},
	}, loaded)
	active, err := st.GetActiveSync(src.ID)
	require.NoError(err)
	assert.Equal(newRunID, active.ID)
}

// TestSaveIMAPFolderStates_UnacknowledgedMailboxIsNotCommitted covers a
// mailbox whose messages were not all safely fetched and persisted this
// session (a fetch error, in production): the run reports an error and the
// mailbox never reaches full acknowledgement, so it must not advance past
// its unresolved work — its prior state stays exactly as an earlier run
// left it, ready to retry in full next time.
func TestSaveIMAPFolderStates_UnacknowledgedMailboxIsNotCommitted(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)
	require.NoError(st.UpsertIMAPFolderStates(src.ID, []store.IMAPFolderState{{
		Mailbox: "INBOX", UIDValidity: 7, UIDNext: 2, HighestModSeq: 11,
	}}))

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	var inboxIDs []string
	for _, observation := range client.ObservedMemberships() {
		if observation.Mailbox == "INBOX" {
			inboxIDs = append(inboxIDs, observation.SourceMessageID)
		}
	}
	require.Len(inboxIDs, 2)
	// Only one of INBOX's two observed messages gets acknowledged, standing
	// in for the other one's raw fetch failing during the real sync.
	client.AcknowledgeMessages(context.Background(), inboxIDs[:1])

	summary := completedIMAPSyncSummary(t, st, src)
	summary.Errors = 1
	require.NoError(saveIMAPFolderStates(context.Background(), st, src, client, summary, 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"INBOX": {UIDValidity: 7, UIDNext: 2, HighestModSeq: 11, KnownUIDs: []uint32{}},
	}, loaded, "a mailbox with an unacknowledged message must not advance its folder high water mark")
	assert.Zero(t, imapMembershipRowCount(t, st, src.ID))
}

// TestSaveIMAPFolderStates_OneMailboxFailsHealthyMailboxesReconcile is the
// direct regression test for the production bug: a fetch failure in one
// mailbox must not block every other mailbox in the same run from updating
// its membership and checkpoint.
func TestSaveIMAPFolderStates_OneMailboxFailsHealthyMailboxesReconcile(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	acknowledgeMailboxObservations(t, client, "Archive")
	var inboxIDs []string
	for _, observation := range client.ObservedMemberships() {
		if observation.Mailbox == "INBOX" {
			inboxIDs = append(inboxIDs, observation.SourceMessageID)
		}
	}
	require.Len(inboxIDs, 2)
	// Leave one INBOX message unacknowledged, as if its fetch failed.
	client.AcknowledgeMessages(context.Background(), inboxIDs[:1])

	summary := completedIMAPSyncSummary(t, st, src)
	summary.Errors = 1
	require.NoError(saveIMAPFolderStates(context.Background(), st, src, client, summary, 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"Archive": {
			UIDValidity: loaded["Archive"].UIDValidity, UIDNext: 2,
			HighestModSeq: loaded["Archive"].HighestModSeq, KnownUIDs: []uint32{1},
		},
	}, loaded, "the healthy mailbox must reconcile even though INBOX did not finish")
	assert.Equal(t, 1, imapMembershipRowCount(t, st, src.ID),
		"only Archive's membership should be committed")
}

func TestSaveIMAPFolderStates_InterruptionBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	acknowledgeMailboxObservations(t, client, "INBOX")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(saveIMAPFolderStates(
		ctx, st, src, client, completedIMAPSyncSummary(t, st, src), 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded, "an interrupted run must leave durable incremental state untouched")
}

func TestSaveIMAPFolderStates_ResumedRunBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	acknowledgeMailboxObservations(t, client, "INBOX")
	summary := completedIMAPSyncSummary(t, st, src)
	summary.WasResumed = true
	require.NoError(saveIMAPFolderStates(context.Background(), st, src, client, summary, 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded,
		"a resumed run must not publish a partial membership snapshot, even one fully acknowledged")
}

func TestSaveIMAPFolderStates_LimitTruncationBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 5})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	summary := completedIMAPSyncSummary(t, st, src)
	summary.MessagesFound = 3
	require.NoError(saveIMAPFolderStates(context.Background(), st, src, client, summary, 3))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded, "a --limit-truncated run must not advance folder high water marks")
}

func TestSaveIMAPFolderStates_BelowLimitStillBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	acknowledgeMailboxObservations(t, client, "INBOX")
	summary := completedIMAPSyncSummary(t, st, src)
	summary.MessagesFound = 1
	require.NoError(saveIMAPFolderStates(context.Background(), st, src, client, summary, 3))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded,
		"any nonzero --limit must leave durable incremental state untouched, even a fully acknowledged run")
}

func TestSaveIMAPFolderStates_NonIMAPClientIsNoOp(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)

	var notIMAP gmail.API
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, notIMAP, &gmail.SyncSummary{}, 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded)
}

func TestIMAPFolderStateOptions_RoundTripSkipsUnchangedFolders(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	first := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, first)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, first, completedIMAPSyncSummary(t, st, src), 0))
	require.NoError(first.Close())

	beforeMemberships := imapMembershipRowCount(t, st, src.ID)
	require.NotZero(beforeMemberships)

	opts := imapFolderStateOptions(st, src, false)
	require.NotEmpty(opts, "saved states must produce a client option")

	second := listedIMAPClient(t, addr, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := second.ListMessages(ctx, "", "")
	require.NoError(err)
	assert.Empty(t, resp.Messages,
		"a stored baseline plus a matching message count proves nothing changed")

	// The whole point of the skip is that it costs nothing durable: applying
	// the resulting deltas must leave every membership row and every message
	// exactly as the first sync left them.
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, second, completedIMAPSyncSummary(t, st, src), 0))
	assert.Equal(t, beforeMemberships, imapMembershipRowCount(t, st, src.ID),
		"skipping a mailbox must not delete its memberships")
	assert.Zero(t, imapTombstonedMessageCount(t, st, src.ID),
		"skipping a mailbox must not tombstone its messages")
}

func TestIMAPFolderStateOptions_ForceRescanRetainsStatesAndEnumerates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(
		t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(
		"imap", "imap://alice@example.com")
	require.NoError(err)

	first := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, first)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, first, completedIMAPSyncSummary(t, st, src), 0))
	require.NoError(first.Close())

	opts := imapFolderStateOptions(st, src, true)
	require.Len(opts, 4,
		"--noresume needs saved identity and alias state, forced enumeration, "+
			"and per-mailbox completion tracking")

	second := listedIMAPClient(t, addr, opts...)
	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second)
	defer cancel()
	resp, err := second.ListMessages(ctx, "", "")
	require.NoError(err)
	assert.Len(resp.Messages, 1,
		"forced enumeration must not skip an unchanged mailbox")
}

func TestSaveIMAPFolderStates_UnmappableObservationRollsBackCursor(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	err = saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0)
	require.Error(err)
	assert.Contains(t, err.Error(), "resolve IMAP membership")
	loaded, loadErr := loadIMAPFolderStates(st, src.ID)
	require.NoError(loadErr)
	assert.Empty(t, loaded)
}

func TestSaveIMAPFolderStates_StatusFailureBlocksDurableApply(t *testing.T) {
	require := require.New(t)
	addr, user := testutil.StartIMAPMemServerWithStatusError(
		t,
		map[string]int{
			"INBOX":            0,
			"[Gmail]/All Mail": 0,
		},
		map[string][]imapapi.MailboxAttr{
			"[Gmail]/All Mail": {imapapi.MailboxAttrAll},
		},
		"INBOX",
	)
	const messageID = "status-failure-store@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "[Gmail]/All Mail", messageID)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	// No AcknowledgeMessages call for either mailbox: this test never claims
	// [Gmail]/All Mail was safely persisted either, so nothing commits even
	// though its own STATUS and enumeration succeeded.
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0))

	states, err := st.GetIMAPFolderStates(src.ID)
	require.NoError(err)
	assert.Empty(t, states)
	known, err := st.GetIMAPKnownUIDs(src.ID)
	require.NoError(err)
	assert.Empty(t, known)
	var membershipCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?
	`), src.ID).Scan(&membershipCount))
	assert.Zero(t, membershipCount)
}

// TestSaveIMAPFolderStates_ExcludedFolderRetainsStateAndLabels covers
// acceptance criterion 3: a mailbox excluded from this run must keep its
// saved state, memberships, and labels exactly as an earlier run left them,
// while the mailboxes actually scanned still reconcile normally.
func TestSaveIMAPFolderStates_ExcludedFolderRetainsStateAndLabels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1, "Personal": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	// A prior run reconciled "Personal" before this run starts excluding it.
	seedIMAPMessage(t, st, src, "Personal|1", "")
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Personal",
		State:   store.IMAPFolderState{Mailbox: "Personal", UIDValidity: 99, UIDNext: 2, HighestModSeq: 900},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "Personal", UIDValidity: 99, UID: 1, SourceMessageID: "Personal|1",
		}},
	}}))
	require.Contains(queryScriptedRFC7162LabelsBySourceMessageID(t, st, src.ID), "Personal|1|Personal")

	client := listedIMAPClient(t, addr, imaplib.WithFolderFilter(nil, []string{"Personal"}))
	inboxIDs := syncMailboxMessages(t, client, "INBOX")
	require.Len(inboxIDs, 1)
	seedObservedIMAPMessages(t, st, src, client)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(map[string]imaplib.FolderState{
		"INBOX": {
			UIDValidity: loaded["INBOX"].UIDValidity, UIDNext: 2,
			HighestModSeq: loaded["INBOX"].HighestModSeq, KnownUIDs: []uint32{1},
		},
		"Personal": {UIDValidity: 99, UIDNext: 2, HighestModSeq: 900, KnownUIDs: []uint32{1}},
	}, loaded, "the excluded mailbox's saved cursor must be untouched")
	assert.Contains(queryScriptedRFC7162LabelsBySourceMessageID(t, st, src.ID), "Personal|1|Personal",
		"the excluded mailbox's message must keep its label")
	assert.Contains(queryScriptedRFC7162LabelsBySourceMessageID(t, st, src.ID),
		inboxIDs[0]+"|INBOX", "the scanned mailbox must still reconcile normally")
}

// TestSaveIMAPFolderStates_MovedMessageAcrossTwoScopedSyncsWithExclusion
// covers acceptance criterion 1 in the exact context of this bug: an
// excluded mailbox is present throughout, and a message moved externally
// between two scanned mailboxes must have its stale membership and label
// disappear on the next sync, driven entirely through the scoped path
// (Personal's exclusion keeps every run in this test off the whole-account
// authoritative path).
func TestSaveIMAPFolderStates_MovedMessageAcrossTwoScopedSyncsWithExclusion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServer(
		t, map[string]int{"INBOX": 0, "Archive": 0, "Personal": 1})
	const rfc822ID = "moved-across-syncs@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", rfc822ID)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)
	excludePersonal := imaplib.WithFolderFilter(nil, []string{"Personal"})

	// Personal was reconciled by a run from before this test's exclusion
	// window; nothing here should ever touch it again.
	seedIMAPMessage(t, st, src, "Personal|1", "")
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Personal",
		State:   store.IMAPFolderState{Mailbox: "Personal", UIDValidity: 99, UIDNext: 2, HighestModSeq: 900},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "Personal", UIDValidity: 99, UID: 1, SourceMessageID: "Personal|1",
		}},
	}}))

	// First sync: the message lives in INBOX; Personal is excluded.
	first := listedIMAPClient(t, addr, excludePersonal)
	inboxIDs := syncMailboxMessages(t, first, "INBOX")
	require.Len(inboxIDs, 1)
	syncMailboxMessages(t, first, "Archive")
	seedObservedIMAPMessages(t, st, src, first)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, first, completedIMAPSyncSummary(t, st, src), 0))

	inboxParts := strings.SplitN(inboxIDs[0], "|", 2)
	require.Len(inboxParts, 2)
	movedUIDNum, err := strconv.ParseUint(inboxParts[1], 10, 32)
	require.NoError(err)
	movedUID := imapapi.UID(movedUIDNum)
	require.NoError(first.Close())
	require.Contains(queryScriptedRFC7162LabelsBySourceMessageID(t, st, src.ID), "INBOX|1|INBOX")

	// The message moves to Archive outside msgvault's control.
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", rfc822ID)
	testutil.ExpungeIMAPMessage(t, addr, "INBOX", movedUID)

	// Second sync: reload saved identity state exactly as production does
	// (imapFolderStateOptions), and keep Personal excluded throughout. The
	// moved message already exists in the store from the first sync and
	// resolves via its RFC822 Message-ID (see applyIMAPMailboxDeltasScoped's
	// resolver) -- no seeding needed, and re-seeding it here under its new
	// "Archive|1" source ID would just create ambiguity for UpsertMessage's
	// own identity matching, unrelated to the scoped-apply path this test
	// means to exercise.
	opts := append(imapFolderStateOptions(st, src, false), excludePersonal)
	second := listedIMAPClient(t, addr, opts...)
	syncMailboxMessages(t, second, "INBOX")
	syncMailboxMessages(t, second, "Archive")
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, second, completedIMAPSyncSummary(t, st, src), 0))

	afterSecond := queryScriptedRFC7162LabelsBySourceMessageID(t, st, src.ID)
	assert.NotContains(afterSecond, "INBOX|1|INBOX",
		"the stale INBOX membership and label must disappear once the move is observed")
	assert.Contains(afterSecond, "INBOX|1|Archive",
		"the message (still identified by its original source_message_id) must carry the new mailbox's label")
	assert.Contains(afterSecond, "Personal|1|Personal",
		"the excluded mailbox must be untouched across both syncs")

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, []uint32{}, loaded["INBOX"].KnownUIDs)
	assert.Equal(t, []uint32{1}, loaded["Archive"].KnownUIDs)
}

// TestSaveIMAPFolderStates_BootstrapWithEmptyMembershipTables covers
// acceptance criterion 5, exercised through the scoped path specifically
// (an excluded "Other" mailbox keeps this run off the whole-account
// authoritative path): production shipped with imap_message_memberships and
// imap_folder_state completely empty while messages and their legacy labels
// already existed. A full re-enumeration of a mailbox must still clear a
// stale legacy label with no membership row behind it at all.
func TestSaveIMAPFolderStates_BootstrapWithEmptyMembershipTables(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServer(
		t, map[string]int{"INBOX": 0, "Archive": 0, "Other": 0})
	const rfc822ID = "legacy-bootstrap@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", rfc822ID)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	conversationID, err := st.EnsureConversation(src.ID, "legacy", "Legacy")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        src.ID,
		SourceMessageID: "legacy-inbox-1",
		RFC822MessageID: sql.NullString{String: rfc822ID, Valid: true},
		MessageType:     "email",
	})
	require.NoError(err)
	labelID, err := st.EnsureLabel(src.ID, "INBOX", "INBOX", "user")
	require.NoError(err)
	require.NoError(st.LinkMessageLabel(messageID, labelID))
	require.Zero(imapMembershipRowCount(t, st, src.ID),
		"reproduces production: labels exist but imap_message_memberships is empty")

	client := listedIMAPClient(t, addr, imaplib.WithFolderFilter(nil, []string{"Other"}))
	syncMailboxMessages(t, client, "INBOX")
	archiveIDs := syncMailboxMessages(t, client, "Archive")
	require.Len(archiveIDs, 1)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0))

	afterLabels := queryScriptedRFC7162LabelsBySourceMessageID(t, st, src.ID)
	assert.NotContains(afterLabels, "legacy-inbox-1|INBOX",
		"a full re-enumeration of INBOX must clear the stale legacy label even with no prior membership row")
	assert.Contains(afterLabels, "legacy-inbox-1|Archive",
		"the message resolves via RFC822 Message-ID to its current mailbox")
	assert.Zero(t, imapTombstonedMessageCount(t, st, src.ID),
		"bootstrap reconciliation must not tombstone")
}

func TestSaveIMAPFolderStates_StoreRoundTripValues(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	seedIMAPMessage(t, st, src, "INBOX|7", "round-trip@example.com")
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State: store.IMAPFolderState{
			Mailbox: "INBOX", UIDValidity: 42, UIDNext: 100, HighestModSeq: 123456789,
		},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 42, UID: 7, SourceMessageID: "INBOX|7",
		}},
	}}))
	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"INBOX": {
			UIDValidity: 42, UIDNext: 100, HighestModSeq: 123456789, KnownUIDs: []uint32{7},
		},
	}, loaded)
}

func TestApplyIMAPMailboxDeltas_ConvertsObservationsAndVanishedUIDs(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)
	seedIMAPMessage(t, st, src, "INBOX|1", "removed@example.com")
	seedIMAPMessage(t, st, src, "INBOX|2", "retained@example.com")
	seedIMAPMessage(t, st, src, "INBOX|3", "added@example.com")
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State: store.IMAPFolderState{
			Mailbox: "INBOX", UIDValidity: 17, UIDNext: 3, HighestModSeq: 100,
		},
		Memberships: []store.IMAPMembershipObservation{
			{UID: 1, SourceMessageID: "INBOX|1"},
			{UID: 2, SourceMessageID: "INBOX|2"},
		},
	}}))

	summary := completedIMAPSyncSummary(t, st, src)
	err = applyIMAPMailboxDeltas(context.Background(), st, src, summary.SyncRunID, []imaplib.MailboxDelta{{
		Mailbox: "INBOX",
		State: imaplib.FolderState{
			UIDValidity: 17, UIDNext: 4, HighestModSeq: 101, KnownUIDs: []uint32{2, 3},
		},
		ChangedUIDs:  []imapapi.UID{3},
		VanishedUIDs: []imapapi.UID{1},
		Incremental:  true,
	}}, []imaplib.MembershipObservation{{
		Mailbox: "INBOX", UIDValidity: 17, UID: 3,
		SourceMessageID: "INBOX|3", RFC822MessageID: "added@example.com",
		Flags: []string{"\\Flagged"},
	}})
	require.NoError(err)

	known, err := st.GetIMAPKnownUIDs(src.ID)
	require.NoError(err)
	assert.Equal(t, map[string][]uint32{"INBOX": {2, 3}}, known)
	states, err := st.GetIMAPFolderStates(src.ID)
	require.NoError(err)
	assert.Equal(t, []store.IMAPFolderState{{
		Mailbox: "INBOX", UIDValidity: 17, UIDNext: 4, HighestModSeq: 101,
	}}, states)
}
