package testutil

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/require"
)

// IMAPTestUsername and IMAPTestPassword are the credentials accepted by
// the server returned from StartIMAPMemServer.
const (
	IMAPTestUsername = "alice@example.com"
	IMAPTestPassword = "secret"
)

type imapLiteral struct {
	*bytes.Reader
}

func (l imapLiteral) Size() int64 { return int64(l.Len()) }

type specialUseSession struct {
	imapserver.Session

	mailboxes  []string
	specialUse map[string][]imap.MailboxAttr
}

type selectErrorSession struct {
	imapserver.Session

	mailbox   string
	remaining int
}

type createErrorSession struct {
	imapserver.Session

	mailboxes []string
	mailbox   string
	createErr error
	listed    *atomic.Bool
}

type phantomUIDSession struct {
	imapserver.Session

	uid     imap.UID
	claimed *atomic.Bool
}

func (s *phantomUIDSession) Search(
	kind imapserver.NumKind,
	criteria *imap.SearchCriteria,
	options *imap.SearchOptions,
) (*imap.SearchData, error) {
	if kind == imapserver.NumKindUID && s.claimed.CompareAndSwap(false, true) {
		var uids imap.UIDSet
		uids.AddNum(s.uid)
		return &imap.SearchData{
			All:   uids,
			Min:   uint32(s.uid),
			Max:   uint32(s.uid),
			Count: 1,
		}, nil
	}
	data, err := s.Session.Search(kind, criteria, options)
	if err != nil {
		return nil, fmt.Errorf("search session: %w", err)
	}
	return data, nil
}

func (s *phantomUIDSession) Move(
	w *imapserver.MoveWriter,
	numSet imap.NumSet,
	dest string,
) error {
	mover, ok := s.Session.(imapserver.SessionMove)
	if !ok {
		return errors.New("wrapped session does not support MOVE")
	}
	if err := mover.Move(w, numSet, dest); err != nil {
		return fmt.Errorf("move session: %w", err)
	}
	return nil
}

func (s *createErrorSession) Create(mailbox string, options *imap.CreateOptions) error {
	if strings.EqualFold(mailbox, s.mailbox) {
		return s.createErr
	}
	if err := s.Session.Create(mailbox, options); err != nil {
		return fmt.Errorf("create mailbox %q: %w", mailbox, err)
	}
	return nil
}

// List hides the configured mailbox from the first LIST call, forcing clients
// to issue CREATE. Later LIST calls reveal the existing mailbox so tests can
// exercise post-CREATE error verification rather than a preflight fast path.
func (s *createErrorSession) List(
	w *imapserver.ListWriter,
	ref string,
	patterns []string,
	_ *imap.ListOptions,
) error {
	hideTarget := s.listed.CompareAndSwap(false, true)
	for _, mailbox := range s.mailboxes {
		if hideTarget && strings.EqualFold(mailbox, s.mailbox) {
			continue
		}
		matches := false
		for _, pattern := range patterns {
			if imapserver.MatchList(mailbox, '/', ref, pattern) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if err := w.WriteList(&imap.ListData{Mailbox: mailbox, Delim: '/'}); err != nil {
			return fmt.Errorf("write LIST response for %q: %w", mailbox, err)
		}
	}
	return nil
}

func (s *selectErrorSession) Select(
	mailbox string,
	options *imap.SelectOptions,
) (*imap.SelectData, error) {
	if mailbox == s.mailbox && s.remaining != 0 {
		if s.remaining > 0 {
			s.remaining--
		}
		return nil, fmt.Errorf("synthetic SELECT failure for %q", mailbox)
	}
	data, err := s.Session.Select(mailbox, options)
	if err != nil {
		return nil, fmt.Errorf("select %q: %w", mailbox, err)
	}
	return data, nil
}

func (s *specialUseSession) List(
	w *imapserver.ListWriter,
	ref string,
	patterns []string,
	_ *imap.ListOptions,
) error {
	for _, mailbox := range s.mailboxes {
		matches := false
		for _, pattern := range patterns {
			if imapserver.MatchList(mailbox, '/', ref, pattern) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if err := w.WriteList(&imap.ListData{
			Mailbox: mailbox,
			Delim:   '/',
			Attrs:   s.specialUse[mailbox],
		}); err != nil {
			return fmt.Errorf("write LIST response for %q: %w", mailbox, err)
		}
	}
	return nil
}

// AppendIMAPMessage appends one synthetic RFC822 message to a mailbox
// of an in-memory IMAP test user.
func AppendIMAPMessage(t *testing.T, user *imapmemserver.User, mailbox string) {
	t.Helper()
	AppendIMAPMessageWithoutMessageID(t, user, mailbox, "body")
}

// AppendIMAPMessageWithoutMessageID appends one synthetic RFC822 message with
// the supplied body and no Message-ID header.
func AppendIMAPMessageWithoutMessageID(
	t *testing.T,
	user *imapmemserver.User,
	mailbox string,
	messageBody string,
) {
	t.Helper()
	body := fmt.Appendf(nil,
		"From: alice@example.com\r\nTo: bob@example.com\r\n\r\n%s\r\n",
		messageBody,
	)
	_, err := user.Append(mailbox, imapLiteral{bytes.NewReader(body)}, &imap.AppendOptions{})
	require.NoError(t, err)
}

// AppendIMAPMessageWithFlags appends one synthetic RFC822 message with the
// supplied IMAP flags to a mailbox of an in-memory IMAP test user.
func AppendIMAPMessageWithFlags(
	t *testing.T,
	user *imapmemserver.User,
	mailbox string,
	flags []imap.Flag,
) {
	t.Helper()
	body := []byte(
		"From: alice@example.com\r\nTo: bob@example.com\r\n\r\nbody\r\n",
	)
	_, err := user.Append(
		mailbox,
		imapLiteral{bytes.NewReader(body)},
		&imap.AppendOptions{Flags: flags},
	)
	require.NoError(t, err)
}

// AppendIMAPMessageWithMessageID appends one synthetic RFC822 message with
// the supplied Message-ID to a mailbox of an in-memory IMAP test user.
func AppendIMAPMessageWithMessageID(
	t *testing.T,
	user *imapmemserver.User,
	mailbox string,
	messageID string,
) {
	t.Helper()
	body := fmt.Appendf(nil,
		"Message-ID: <%s>\r\nFrom: alice@example.com\r\nTo: bob@example.com\r\n\r\nbody\r\n",
		messageID,
	)
	_, err := user.Append(mailbox, imapLiteral{bytes.NewReader(body)}, &imap.AppendOptions{})
	require.NoError(t, err)
}

// StartIMAPMemServer runs an in-memory IMAP server with the given
// mailboxes and per-mailbox message counts, returning its listen
// address and the user handle for later mutation. The server is shut
// down via t.Cleanup.
func StartIMAPMemServer(t *testing.T, messagesPerMailbox map[string]int) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(t, messagesPerMailbox, nil, "", 0, nil, 0)
}

// StartIMAPMemServerWithSpecialUse runs an in-memory IMAP server whose LIST
// responses advertise the supplied special-use attributes.
func StartIMAPMemServerWithSpecialUse(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(t, messagesPerMailbox, specialUse, "", 0, nil, 0)
}

// StartIMAPMemServerWithCreateError runs an in-memory IMAP server that returns
// an Exchange-style plain NO when CREATE targets one named mailbox.
func StartIMAPMemServerWithCreateError(
	t *testing.T,
	messagesPerMailbox map[string]int,
	createErrorMailbox string,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(t, messagesPerMailbox, nil, "", 0, &createErrorConfig{
		mailbox: createErrorMailbox,
		err: &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Mailbox already exists.",
		},
	}, 0)
}

// StartIMAPMemServerWithCreateFailure hides an existing mailbox from the first
// LIST and returns a genuine CREATE failure. Clients must not swallow the error
// merely because a later LIST reveals a same-named mailbox.
func StartIMAPMemServerWithCreateFailure(
	t *testing.T,
	messagesPerMailbox map[string]int,
	createErrorMailbox string,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(t, messagesPerMailbox, nil, "", 0, &createErrorConfig{
		mailbox: createErrorMailbox,
		err: &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeOverQuota,
			Text: "Mailbox quota exceeded.",
		},
	}, 0)
}

// StartIMAPMemServerWithPhantomUID runs an in-memory IMAP server whose first
// UID SEARCH reports a missing UID. MOVE then emits an empty COPYUID response,
// reproducing servers where a message disappears between lookup and mutation.
func StartIMAPMemServerWithPhantomUID(
	t *testing.T,
	messagesPerMailbox map[string]int,
	phantomUID imap.UID,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(t, messagesPerMailbox, nil, "", 0, nil, phantomUID)
}

// StartIMAPMemServerWithSelectError runs an in-memory IMAP server that rejects
// SELECT for one mailbox while serving all other mailboxes normally.
func StartIMAPMemServerWithSelectError(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	selectErrorMailbox string,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(
		t, messagesPerMailbox, specialUse, selectErrorMailbox, -1, nil, 0)
}

// StartIMAPMemServerWithOneShotSelectError runs an in-memory IMAP server that
// rejects the first SELECT for one mailbox, then serves it normally.
func StartIMAPMemServerWithOneShotSelectError(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	selectErrorMailbox string,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(
		t, messagesPerMailbox, specialUse, selectErrorMailbox, 1, nil, 0)
}

type createErrorConfig struct {
	mailbox string
	err     error
}

func startIMAPMemServer(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	selectErrorMailbox string,
	selectErrorCount int,
	createError *createErrorConfig,
	phantomUID imap.UID,
) (string, *imapmemserver.User) {
	t.Helper()
	user := imapmemserver.NewUser(IMAPTestUsername, IMAPTestPassword)
	mailboxes := make([]string, 0, len(messagesPerMailbox))
	for mailbox, count := range messagesPerMailbox {
		require.NoError(t, user.Create(mailbox, nil))
		mailboxes = append(mailboxes, mailbox)
		for range count {
			AppendIMAPMessage(t, user, mailbox)
		}
	}
	sort.Strings(mailboxes)
	memServer := imapmemserver.New()
	memServer.AddUser(user)
	var phantomUIDClaimed atomic.Bool
	var createErrorListed atomic.Bool
	var caps imap.CapSet
	if phantomUID != 0 {
		caps = imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapMove:      {},
			imap.CapUIDPlus:   {},
		}
	}

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			var session imapserver.Session
			session = memServer.NewSession()
			if len(specialUse) > 0 {
				session = &specialUseSession{
					Session:    session,
					mailboxes:  mailboxes,
					specialUse: specialUse,
				}
			}
			if selectErrorMailbox != "" {
				session = &selectErrorSession{
					Session:   session,
					mailbox:   selectErrorMailbox,
					remaining: selectErrorCount,
				}
			}
			if createError != nil {
				session = &createErrorSession{
					Session:   session,
					mailboxes: mailboxes,
					mailbox:   createError.mailbox,
					createErr: createError.err,
					listed:    &createErrorListed,
				}
			}
			if phantomUID != 0 {
				session = &phantomUIDSession{
					Session: session,
					uid:     phantomUID,
					claimed: &phantomUIDClaimed,
				}
			}
			return session, nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })

	return ln.Addr().String(), user
}
