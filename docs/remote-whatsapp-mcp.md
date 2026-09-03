# Remote WhatsApp MCP

`whatsapp-live-mcp` can optionally expose a second, OAuth-protected MCP route
for a delegated remote user. The private `/mcp` route keeps the complete
WhatsApp administration catalog. The remote `/external/mcp` route exposes only:

- `whatsapp_status`
- `list_whatsapp_chats`
- `list_whatsapp_messages`
- `send_whatsapp_message`
- `send_whatsapp_reaction`

Login, logout, QR pairing, and history synchronization are intentionally absent
from the delegated catalog.

## Configuration

The remote route is disabled unless `MSGVAULT_WHATSAPP_REMOTE_OAUTH_ISSUER` is
set. When enabled, all variables below are required:

```dotenv
MSGVAULT_WHATSAPP_REMOTE_OAUTH_ISSUER=https://whatsapp.example.com
MSGVAULT_WHATSAPP_REMOTE_OAUTH_RESOURCE=https://whatsapp.example.com/mcp
MSGVAULT_WHATSAPP_REMOTE_OAUTH_CLIENT_ID=claude-whatsapp
MSGVAULT_WHATSAPP_REMOTE_OAUTH_CLIENT_SECRET=<at-least-32-random-characters>
MSGVAULT_WHATSAPP_REMOTE_OAUTH_LOGIN_USER=user@example.com
MSGVAULT_WHATSAPP_REMOTE_OAUTH_LOGIN_PASSWORD=<at-least-16-random-characters>
MSGVAULT_WHATSAPP_REMOTE_OAUTH_STATE_FILE=/var/lib/msgvault/remote-oauth.json
MSGVAULT_WHATSAPP_REMOTE_OAUTH_REDIRECT_URIS=https://claude.ai/api/mcp/auth_callback,https://claude.com/api/mcp/auth_callback
```

The server implements a preregistered confidential OAuth client,
authorization-code flow with mandatory S256 PKCE, one-hour access tokens, and
rotating 30-day refresh tokens. Persistent state contains token hashes, never
the bearer or refresh tokens themselves.

## Reverse proxy

Publish a dedicated HTTPS origin. Route only these public paths:

```text
/mcp                                      -> /external/mcp
/.well-known/oauth-protected-resource     -> same path
/.well-known/oauth-protected-resource/mcp -> same path
/.well-known/oauth-authorization-server   -> same path
/authorize                                -> same path
/oauth/login                              -> same path
/token                                    -> same path
```

Do not publish the private `/mcp`, `/api/*`, `/qr`, `/qr.png`, or
`/status.json` routes. Add rate limiting to `/authorize`, `/oauth/login`, and
`/token` at the public reverse proxy.

In Claude, add the public `https://whatsapp.example.com/mcp` URL as a custom
connector and enter the configured client ID and client secret under Advanced
settings. The browser authorization page then asks for the delegated user's
login password.
