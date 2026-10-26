package live

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGroupOperationsPreserveParticipantFailures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	participants := []GroupParticipantInfo{{JID: "15555550123@s.whatsapp.net", Error: 403}}
	transport := &mockTransport{
		status: Status{AccountJID: "15555550100@s.whatsapp.net", Paired: true, Connected: true, LoggedIn: true},
		createGroup: func(_ context.Context, req TransportCreateGroupRequest) (TransportCreateGroupResult, error) {
			assert.Equal("Test group", req.Name)
			return TransportCreateGroupResult{JID: "120363000000@g.us", Name: req.Name, Participants: participants}, nil
		},
		getGroupInfo: func(_ context.Context, req TransportGetGroupInfoRequest) (TransportGroupInfoResult, error) {
			return TransportGroupInfoResult{JID: req.JID, Participants: participants}, nil
		},
		updateGroupParticipants: func(_ context.Context, req TransportUpdateGroupParticipantsRequest) (TransportGroupParticipantsResult, error) {
			assert.Equal("add", req.Action)
			return TransportGroupParticipantsResult{Participants: participants}, nil
		},
	}
	svc, err := NewService(ServiceOptions{Store: st, Transport: transport})
	require.NoError(err)
	created, err := svc.CreateGroup(t.Context(), CreateGroupRequest{Name: "Test group", Participants: []string{participants[0].JID}})
	require.NoError(err)
	assert.Equal(participants, created.Participants)
	info, err := svc.GetGroupInfo(t.Context(), GetGroupInfoRequest{JID: created.JID})
	require.NoError(err)
	assert.Equal(participants, info.Participants)
	updated, err := svc.UpdateGroupParticipants(t.Context(), UpdateGroupParticipantsRequest{JID: created.JID, Action: "add", Participants: []string{participants[0].JID}})
	require.NoError(err)
	assert.Equal(participants, updated.Participants)

	transport.status.Connected = false
	_, err = svc.CreateGroup(t.Context(), CreateGroupRequest{Name: "Test group", Participants: []string{participants[0].JID}})
	require.Error(err)
	_, err = svc.GetGroupInfo(t.Context(), GetGroupInfoRequest{JID: created.JID})
	require.Error(err)
	_, err = svc.UpdateGroupParticipants(t.Context(), UpdateGroupParticipantsRequest{JID: created.JID, Action: "add", Participants: []string{participants[0].JID}})
	require.Error(err)
}
