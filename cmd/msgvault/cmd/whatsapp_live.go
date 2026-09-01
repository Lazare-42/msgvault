package cmd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

var whatsappLinkPhone string
var whatsappLiveMCPAddr string
var whatsappLivePairingToken string
var whatsappLiveHTTPBasePath string
var whatsappLivePublicBaseURL string

var whatsappLinkCmd = &cobra.Command{
	Use:   "whatsapp-link",
	Short: "Link a WhatsApp account for live MCP sending",
	RunE: func(cmd *cobra.Command, args []string) error {
		transport, err := openWhatsAppTransport(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = transport.Close() }()

		if whatsappLinkPhone != "" {
			return transport.PairPhone(cmd.Context(), whatsappLinkPhone, os.Stdout)
		}
		return transport.LinkQR(cmd.Context(), os.Stdout)
	},
}

var whatsappStatusCmd = &cobra.Command{
	Use:   "whatsapp-status",
	Short: "Show live WhatsApp pairing status",
	RunE: func(cmd *cobra.Command, args []string) error {
		transport, err := openWhatsAppTransport(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = transport.Close() }()

		status, err := transport.Status(cmd.Context())
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	},
}

var whatsappLiveMCPCmd = &cobra.Command{
	Use:   "whatsapp-live-mcp",
	Short: "Run writable live WhatsApp MCP server",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, err := normalizeMCPHTTPAddr(whatsappLiveMCPAddr, false, cfg.Server.APIKey != "")
		if err != nil {
			return usageErr(cmd, err)
		}

		st, cleanup, err := openWritableStoreAndInit()
		if err != nil {
			return err
		}
		defer cleanup()

		transport, err := openWhatsAppTransport(cmd.Context())
		if err != nil {
			return err
		}
		var notifier *whatsapplive.WebhookNotifier
		if webhookURL := strings.TrimSpace(os.Getenv("MSGVAULT_WHATSAPP_INBOUND_WEBHOOK_URL")); webhookURL != "" {
			notifier = whatsapplive.NewWebhookNotifier(whatsapplive.WebhookOptions{
				URL:    webhookURL,
				Secret: os.Getenv("MSGVAULT_WHATSAPP_INBOUND_WEBHOOK_SECRET"),
				Logger: logger,
			})
			defer notifier.Close()
			logger.Info("WhatsApp inbound webhook enabled", "url", webhookURL)
		}
		serviceOpts := whatsapplive.ServiceOptions{
			Store:          st,
			Transport:      transport,
			LoginContext:   cmd.Context(),
			AttachmentsDir: cfg.AttachmentsDir(),
			Logger:         logger,
		}
		if notifier != nil {
			serviceOpts.Notify = notifier.Notify
		}
		service, err := whatsapplive.NewService(serviceOpts)
		if err != nil {
			_ = transport.Close()
			return err
		}
		transport.SetInboundHandler(func(ctx context.Context, msg whatsapplive.InboundMessage) error {
			_, archiveErr := service.ArchiveInbound(ctx, msg)
			return archiveErr
		})
		transport.SetHistorySyncHandler(service.ArchiveHistorySync)
		defer func() { _ = service.Close() }()

		initialStatus, err := service.Status(cmd.Context())
		if err != nil {
			return fmt.Errorf("whatsapp status: %w", err)
		}
		if initialStatus.Paired {
			if err := service.Connect(cmd.Context()); err != nil {
				logger.Warn("WhatsApp session did not connect; serving recovery page", "err", err)
			}
		} else {
			logger.Warn("WhatsApp is not paired; serving QR pairing page")
		}

		engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
		opts := mcpserver.ServeOptions{
			Engine:         engine,
			AttachmentsDir: cfg.AttachmentsDir(),
			DataDir:        cfg.Data.DataDir,
			WhatsAppFactory: func(ctx context.Context, account string) (whatsapplive.Client, error) {
				return service, nil
			},
		}
		pairingToken := whatsappLivePairingToken
		if pairingToken == "" {
			pairingToken = os.Getenv("MSGVAULT_WHATSAPP_PAIRING_TOKEN")
		}
		basePath := whatsappLiveHTTPBasePath
		if basePath == "" {
			basePath = os.Getenv("MSGVAULT_WHATSAPP_HTTP_BASE_PATH")
		}
		basePath, err = normalizeHTTPBasePath(basePath)
		if err != nil {
			return usageErr(cmd, err)
		}
		publicBaseURL := whatsappLivePublicBaseURL
		if publicBaseURL == "" {
			publicBaseURL = os.Getenv("MSGVAULT_WHATSAPP_PUBLIC_BASE_URL")
		}
		publicBaseURL, err = normalizeHTTPPublicBaseURL(publicBaseURL)
		if err != nil {
			return usageErr(cmd, err)
		}
		opts.WhatsAppLoginURL = whatsappLoginPageURL(publicBaseURL, basePath)
		apiToken := strings.TrimSpace(os.Getenv("MSGVAULT_WHATSAPP_API_TOKEN"))
		return serveWhatsAppLiveHTTP(cmd.Context(), addr, st, service, transport, opts, pairingToken, basePath, apiToken)
	},
}

func openWhatsAppTransport(ctx context.Context) (*whatsapplive.WhatsmeowTransport, error) {
	sessionPath := filepath.Join(cfg.Data.DataDir, "whatsapp-session.db")
	return whatsapplive.NewWhatsmeowTransport(ctx, whatsapplive.WhatsmeowOptions{
		SessionPath: sessionPath,
	})
}

func init() {
	rootCmd.AddCommand(whatsappLinkCmd)
	rootCmd.AddCommand(whatsappStatusCmd)
	rootCmd.AddCommand(whatsappLiveMCPCmd)

	whatsappLinkCmd.Flags().StringVar(&whatsappLinkPhone, "phone", "", "International phone number for pairing code login instead of QR")
	whatsappLiveMCPCmd.Flags().StringVar(&whatsappLiveMCPAddr, "addr", "127.0.0.1:8121", "Loopback address to listen on (host:port)")
	whatsappLiveMCPCmd.Flags().StringVar(&whatsappLivePairingToken, "pairing-token", "", "Token required for WhatsApp QR pairing pages (or MSGVAULT_WHATSAPP_PAIRING_TOKEN)")
	whatsappLiveMCPCmd.Flags().StringVar(&whatsappLiveHTTPBasePath, "http-base-path", "", "Public URL base path for QR pages, e.g. /personal (or MSGVAULT_WHATSAPP_HTTP_BASE_PATH)")
	whatsappLiveMCPCmd.Flags().StringVar(&whatsappLivePublicBaseURL, "public-base-url", "", "Public base URL for QR pages, e.g. https://whats.lazare.ai/personal (or MSGVAULT_WHATSAPP_PUBLIC_BASE_URL)")
}

func serveWhatsAppLiveHTTP(ctx context.Context, addr string, st *store.Store, service *whatsapplive.Service, transport *whatsapplive.WhatsmeowTransport, opts mcpserver.ServeOptions, pairingToken string, basePath string, apiToken string) error {
	mcpHandler := mcpserver.NewStreamableHTTPHandler(opts, true)
	pairing := &whatsappPairingHandler{
		ctx:          ctx,
		service:      service,
		transport:    transport,
		pairingToken: strings.TrimSpace(pairingToken),
		basePath:     basePath,
	}
	api := &whatsappLiveAPIHandler{store: st, sender: service, token: apiToken}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/", pairing.redirectRoot)
	mux.HandleFunc("/qr", pairing.qrPage)
	mux.HandleFunc("/qr.png", pairing.qrPNG)
	mux.HandleFunc("/status.json", pairing.statusJSON)
	mux.HandleFunc("/api/chats", api.listChats)
	mux.HandleFunc("/api/messages", api.listMessages)
	mux.HandleFunc("/api/send", api.sendMessage)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("Starting WhatsApp live MCP server", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return ctx.Err()
	}
}

type whatsappPairingHandler struct {
	ctx          context.Context
	service      *whatsapplive.Service
	transport    *whatsapplive.WhatsmeowTransport
	pairingToken string
	basePath     string
}

type whatsappPairingView struct {
	Status              whatsapplive.Status
	Pairing             whatsapplive.QRPairingState
	Authorized          bool
	TokenQuery          string
	NeedsToken          bool
	HasQRCode           bool
	NeedsAuthentication bool
	RefreshSecs         int
	BasePath            string
}

func (h *whatsappPairingHandler) redirectRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, h.publicPath("/qr"), http.StatusFound)
}

func (h *whatsappPairingHandler) qrPage(w http.ResponseWriter, r *http.Request) {
	authorized := h.authorized(r)
	if !authorized {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = whatsappPairingTemplate.Execute(w, whatsappPairingView{
			Authorized:  false,
			NeedsToken:  h.pairingToken != "",
			RefreshSecs: 0,
			BasePath:    h.basePath,
		})
		return
	}
	status, err := h.service.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status.Paired && !status.Connected {
		status, err = h.reconnect(r.Context(), status)
		if err != nil {
			logger.Warn("WhatsApp reconnect from QR page failed", "err", err)
		}
	}
	if !status.Paired {
		if err := h.transport.StartQRPairing(h.ctx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	pairing, err := h.transport.PairingState(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tokenQuery := ""
	if token := h.requestToken(r); token != "" {
		tokenQuery = "?token=" + template.URLQueryEscaper(token)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = whatsappPairingTemplate.Execute(w, whatsappPairingView{
		Status:              status,
		Pairing:             pairing,
		Authorized:          true,
		TokenQuery:          tokenQuery,
		NeedsToken:          h.pairingToken != "",
		HasQRCode:           pairing.Code != "" && !pairing.Paired,
		NeedsAuthentication: status.Paired && !status.LoggedIn,
		RefreshSecs:         5,
		BasePath:            h.basePath,
	})
}

func (h *whatsappPairingHandler) reconnect(reqCtx context.Context, fallback whatsapplive.Status) (whatsapplive.Status, error) {
	connectCtx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	if err := h.service.Connect(connectCtx); err != nil {
		status, statusErr := h.service.Status(reqCtx)
		if statusErr != nil {
			return fallback, err
		}
		return status, err
	}
	return h.service.Status(reqCtx)
}

func (h *whatsappPairingHandler) qrPNG(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	pairing, err := h.transport.PairingState(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if pairing.Code == "" || pairing.Paired {
		http.Error(w, "QR code not available", http.StatusNotFound)
		return
	}
	png, err := qrcode.Encode(pairing.Code, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (h *whatsappPairingHandler) statusJSON(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	status, err := h.service.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pairing, err := h.transport.PairingState(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Status  whatsapplive.Status         `json:"status"`
		Pairing whatsapplive.QRPairingState `json:"pairing"`
	}{
		Status:  status,
		Pairing: pairing,
	})
}

func (h *whatsappPairingHandler) authorized(r *http.Request) bool {
	if h.pairingToken == "" {
		return true
	}
	got := h.requestToken(r)
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.pairingToken)) == 1
}

func (h *whatsappPairingHandler) requestToken(r *http.Request) string {
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}
	if token := r.Header.Get("X-Pairing-Token"); token != "" {
		return token
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

func (h *whatsappPairingHandler) publicPath(path string) string {
	if h.basePath == "" {
		return path
	}
	return h.basePath + path
}

func normalizeHTTPBasePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "/" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("http base path must start with /")
	}
	if strings.ContainsAny(raw, "?#") {
		return "", fmt.Errorf("http base path must not contain query or fragment")
	}
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		return "", nil
	}
	return raw, nil
}

func normalizeHTTPPublicBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("public base URL must start with http:// or https://")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("public base URL must include a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("public base URL must not contain query or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func whatsappLoginPageURL(publicBaseURL string, basePath string) string {
	if publicBaseURL != "" {
		return publicBaseURL + "/qr"
	}
	if basePath != "" {
		return basePath + "/qr"
	}
	return "/qr"
}

var whatsappPairingTemplate = template.Must(template.New("whatsapp-pairing").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  {{if gt .RefreshSecs 0}}<meta http-equiv="refresh" content="{{.RefreshSecs}}">{{end}}
  <title>WhatsApp pairing</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #f4f5f0; color: #1c241f; }
    main { width: min(92vw, 520px); border: 1px solid #26342c; background: #ffffff; padding: 28px; box-shadow: 8px 8px 0 #26342c; }
    h1 { font-size: 24px; margin: 0 0 16px; }
    p { line-height: 1.45; }
    .qr { display: grid; place-items: center; min-height: 340px; border: 1px solid #c9d1c5; background: #fafbf7; margin: 18px 0; }
    img { image-rendering: pixelated; width: 320px; height: 320px; }
    code { background: #eef1e8; padding: 2px 5px; }
    .ok { color: #0d6b32; font-weight: 700; }
    .err { color: #a11616; font-weight: 700; }
    @media (prefers-color-scheme: dark) {
      body { background: #141815; color: #e7ece4; }
      main { background: #1d231f; border-color: #9ca894; box-shadow: 8px 8px 0 #070907; }
      .qr { background: #f7faf5; }
      code { color: #111; }
    }
  </style>
</head>
<body>
<main>
  <h1>WhatsApp pairing</h1>
  {{if not .Authorized}}
    <p class="err">Pairing token required.</p>
    <p>Open this page with <code>?token=...</code> or send <code>Authorization: Bearer ...</code>.</p>
  {{else if .NeedsAuthentication}}
    <p class="err">Pairing incomplete.</p>
    <p>WhatsApp created a local device record, but the session is not authenticated yet.</p>
    <p>Account: <code>{{.Status.AccountJID}}</code></p>
    <p>Connected: <code>{{.Status.Connected}}</code></p>
    <p>Logged in: <code>{{.Status.LoggedIn}}</code></p>
    <p>Session: <code>{{.Status.SessionPath}}</code></p>
  {{else if .Status.Paired}}
    <p class="ok">Linked.</p>
    <p>Account: <code>{{.Status.AccountJID}}</code></p>
    <p>Connected: <code>{{.Status.Connected}}</code></p>
    <p>Logged in: <code>{{.Status.LoggedIn}}</code></p>
  {{else}}
    {{if .HasQRCode}}
      <p>Scan this QR code in WhatsApp: Settings → Linked devices → Link a device.</p>
      <div class="qr"><img alt="WhatsApp pairing QR code" src="qr.png{{.TokenQuery}}"></div>
      {{if not .Pairing.ExpiresAt.IsZero}}<p>Expires: <code>{{.Pairing.ExpiresAt.Format "15:04:05 MST"}}</code></p>{{end}}
    {{else}}
      <p>Waiting for a QR code. This page refreshes automatically.</p>
      {{if .Pairing.Error}}<p class="err">{{.Pairing.Error}}</p>{{end}}
    {{end}}
    <p>Session: <code>{{.Status.SessionPath}}</code></p>
  {{end}}
</main>
</body>
</html>`))
