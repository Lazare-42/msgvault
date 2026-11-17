package cmd

import (
	"context"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubWhatsAppSender struct {
	send func(context.Context, whatsapplive.SendMessageRequest) (whatsapplive.SendResult, error)
}

func (s stubWhatsAppSender) SendMessage(ctx context.Context, req whatsapplive.SendMessageRequest) (whatsapplive.SendResult, error) {
	return s.send(ctx, req)
}

func TestWhatsAppLiveAPISendMessage(t *testing.T) {
	assert := assert.New(t)
	var got whatsapplive.SendMessageRequest
	handler := &whatsappLiveAPIHandler{
		token: "secret",
		sender: stubWhatsAppSender{send: func(_ context.Context, req whatsapplive.SendMessageRequest) (whatsapplive.SendResult, error) {
			got = req
			return whatsapplive.SendResult{
				LocalRequestID:  req.LocalRequestID,
				RemoteMessageID: "remote-1",
				Status:          "sent",
			}, nil
		}},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(`{
		"chat_id":"15551234567@s.whatsapp.net",
		"body":"hello",
		"local_request_id":"wa-desk:1"
	}`))
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()

	handler.sendMessage(response, req)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal("15551234567@s.whatsapp.net", got.ChatID)
	assert.Equal("hello", got.Body)
	assert.Equal("wa-desk:1", got.LocalRequestID)
	assert.Contains(response.Body.String(), `"remote_message_id":"remote-1"`)
}

func TestWhatsAppLiveAPISendRequiresBearerToken(t *testing.T) {
	handler := &whatsappLiveAPIHandler{token: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(`{}`))
	response := httptest.NewRecorder()

	handler.sendMessage(response, req)

	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestWhatsAppLiveAPISendReportsNotReady(t *testing.T) {
	handler := &whatsappLiveAPIHandler{
		token: "secret",
		sender: stubWhatsAppSender{send: func(context.Context, whatsapplive.SendMessageRequest) (whatsapplive.SendResult, error) {
			return whatsapplive.SendResult{}, errors.New("whatsapp is not ready: ready=false")
		}},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(`{"chat_id":"a","body":"b"}`))
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()

	handler.sendMessage(response, req)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
}
