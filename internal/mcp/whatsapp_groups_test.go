package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

func TestWhatsAppGroupToolsProtocolAndAccess(t *testing.T) {
	for _, name := range []string{ToolCreateWhatsAppGroup, ToolGetWhatsAppGroupInfo, ToolUpdateWhatsAppGroupParticipants} {
		t.Run(name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			calls := 0
			const account = "15555550100@s.whatsapp.net"
			const jid = "120363000000@g.us"
			participants := []whatsapplive.GroupParticipantInfo{{JID: "15555550123@s.whatsapp.net", Error: 403}}
			client := &mockWhatsAppClient{
				status: whatsapplive.Status{Paired: true, Connected: true, LoggedIn: true},
				createGroup: func(_ context.Context, req whatsapplive.CreateGroupRequest) (whatsapplive.CreateGroupResult, error) {
					calls++
					checks.Equal(account, req.Account)
					checks.Equal("Test group", req.Name)
					checks.Equal([]string{participants[0].JID}, req.Participants)
					return whatsapplive.CreateGroupResult{JID: jid, Participants: participants}, nil
				},
				getGroupInfo: func(_ context.Context, req whatsapplive.GetGroupInfoRequest) (whatsapplive.GroupInfoResult, error) {
					calls++
					checks.Equal(account, req.Account)
					checks.Equal(jid, req.JID)
					return whatsapplive.GroupInfoResult{JID: jid, Participants: participants}, nil
				},
				updateGroupParticipants: func(_ context.Context, req whatsapplive.UpdateGroupParticipantsRequest) (whatsapplive.GroupParticipantsResult, error) {
					calls++
					checks.Equal(account, req.Account)
					checks.Equal(jid, req.JID)
					checks.Equal("add", req.Action)
					checks.Equal([]string{participants[0].JID}, req.Participants)
					return whatsapplive.GroupParticipantsResult{Participants: participants}, nil
				},
			}
			opts := ServeOptions{
				Engine: &querytest.MockEngine{Accounts: []query.AccountInfo{{ID: 1, SourceType: "whatsapp", Identifier: account}}},
				WhatsAppFactory: func(_ context.Context, got string) (whatsapplive.Client, error) {
					checks.Equal(account, got)
					return client, nil
				},
			}
			args := map[string]any{}
			switch name {
			case ToolCreateWhatsAppGroup:
				args["name"] = "Test group"
				args["participants"] = []string{participants[0].JID}
			case ToolGetWhatsAppGroupInfo:
				args["jid"] = jid
			case ToolUpdateWhatsAppGroupParticipants:
				args["jid"] = jid
				args["action"] = "add"
				args["participants"] = []string{participants[0].JID}
			}
			result := rawCallTool(t, opts, name, args)
			checks.NotEqual(true, result["isError"])
			checks.Equal(1, calls)
			content, ok := result["content"].([]any)
			must.True(ok)
			must.NotEmpty(content)
			item, ok := content[0].(map[string]any)
			must.True(ok)
			checks.Contains(item["text"], "403")

			for _, delegated := range []bool{false, true} {
				if !delegated && name == ToolGetWhatsAppGroupInfo {
					continue
				}
				opts.DelegatedOnly = delegated
				handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: delegated}).Handler
				response, _ := task4RawRequest(t, handler, "tools/call", map[string]any{"name": name, "arguments": args}, nil)
				checks.True(len(response.Error) > 0 || response.Result["isError"] == true)
				checks.Equal(1, calls, "unavailable tools must not reach the live client")
			}
		})
	}
}
