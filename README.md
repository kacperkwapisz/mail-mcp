# poke-mail

An MCP server that bridges IMAP/SMTP email accounts to [Poke](https://poke.com). Provides AI agents with tools to search, read, send, and manage emails, and automatically forwards new incoming emails to the Poke inbound endpoint.

## Features

- **13 MCP tools**: list accounts, search, read, send, draft, archive, move, mark, list/create/rename/delete folders, server info
- **Send toggle**: Disable `send_email` globally or per account — agents use `create_draft` instead
- **IMAP IDLE watcher**: Real-time monitoring of new emails, forwarded to Poke automatically
- **Multi-account support**: Configure multiple email accounts in a single config file
- **Bearer token auth**: Secure the server with `MCP_API_KEY` so only you can use it
- **No delete tool**: Emails can be archived or moved, never deleted by an agent

## Quick Start

**Prerequisites:** Python 3.10+ and Node.js 18+ (which includes `npx` and `npm`).

```bash
git clone https://github.com/kacperkwapisz/poke-mail.git
cd poke-mail
```

If you haven't logged into Poke yet, do that first — `start.sh` will pick up your token automatically:

```bash
npx poke login
```

Then just run:

```bash
./start.sh
```

On the **first run**, `start.sh` automatically handles the full setup:
1. Creates a Python virtualenv and installs dependencies
2. Copies `config.example.yml` → `config.yml`
3. Reads your Poke API key from `poke login` credentials and injects it into `config.yml`
4. Generates a random `MCP_API_KEY` and saves it to `.env`
5. Immediately starts the server and tunnel

After the first run completes (or if the server exits with email auth errors), open `config.yml` and fill in your email account credentials, then run `./start.sh` again.

> **Note:** If your email credentials in `config.yml` are still placeholders, IMAP/SMTP connections will fail on startup. Update the file and rerun `./start.sh`.

On **subsequent runs**, `start.sh` skips setup and goes straight to starting the server and tunnel.

### AI coding agent setup

Copy this prompt into your AI coding agent (Claude Code, Cursor, etc.):

```text
Set up poke-mail (https://github.com/kacperkwapisz/poke-mail) for me — clone the repo, run 'npx poke login' so I can authenticate with Poke (wait for me to confirm), then run './start.sh' which will automatically wire up my Poke API key, generate an MCP_API_KEY, set up the virtualenv, and start the server and tunnel — then help me fill in my email credentials in config.yml (guide me on IMAP/SMTP host and port for my provider but do NOT type passwords or secrets — tell me to enter those myself and confirm when done); if the server fails due to missing/invalid email credentials, have me update config.yml and run './start.sh' again to restart it.
```

## Manual Setup

### 1. Configure accounts

```bash
cp config.example.yml config.yml
```

Edit `config.yml` with your email credentials and Poke API key (from [poke.com/settings/advanced](https://poke.com/settings/advanced)):

```yaml
webhook_url: https://poke.com/api/v1/inbound/api-message
poke_api_key: your-api-key  # from https://poke.com/settings/advanced

accounts:
  # iCloud Mail — login is @icloud.com, send as your custom domain
  - id: icloud
    imap_host: imap.mail.me.com
    imap_username: you@icloud.com
    imap_password: your-app-password
    smtp_host: smtp.mail.me.com
    smtp_username: you@icloud.com
    smtp_password: your-app-password
    from_address: you@yourdomain.com  # optional — override From: header
    watch_folders:
      - INBOX

  # Custom SMTP server
  - id: work
    imap_host: imap.example.com
    imap_username: you@example.com
    imap_password: your-password
    smtp_host: smtp.example.com
    smtp_username: you@example.com
    smtp_password: your-password
    watch_folders:
      - INBOX
```

For iCloud, generate an [App-Specific Password](https://support.apple.com/en-us/102654).

### 2. Install dependencies

```bash
pip install -r requirements.txt
```

### 3. Run

```bash
MCP_API_KEY=your-secret-key python3 src/server.py
```

### 4. Test

```bash
npx @modelcontextprotocol/inspector
```

Open http://localhost:3000 and connect to `http://localhost:3000/mcp` using "Streamable HTTP" transport. Pass `Authorization: Bearer your-secret-key` header.

## Authentication

Set `MCP_API_KEY` to secure the server. All requests must include `Authorization: Bearer <MCP_API_KEY>`.

When running via `start.sh` (which uses `poke tunnel`), set `POKE_TUNNEL=1` to make `MCP_API_KEY` optional — the tunnel handles authentication. `start.sh` sets this automatically.

If `MCP_API_KEY` is not set and `POKE_TUNNEL` is not `1`, the server runs unauthenticated (with a warning). **Always set it in non-tunnel deployments.**

When connecting from Poke, add the bearer token in your connection settings.

## Docker

```bash
docker build -t poke-mail .

docker run -d \
  -p 3000:3000 \
  -v $(pwd)/config.yml:/app/config.yml:ro \
  -e MCP_API_KEY=your-secret-key \
  poke-mail
```

Or use the pre-built image from GitHub Container Registry:

```bash
docker run -d \
  -p 3000:3000 \
  -v $(pwd)/config.yml:/app/config.yml:ro \
  -e MCP_API_KEY=your-secret-key \
  ghcr.io/kacperkwapisz/poke-mail:main
```

### Resource Limits

The server is mostly idle (IMAP IDLE + lightweight HTTP). Recommended limits for container orchestrators:

| Resource | Reservation | Limit |
|----------|-------------|-------|
| Memory   | 128 MB      | 256 MB |
| CPU      | 0.25        | 0.5    |

## MCP Tools

> **All email tools require `account_id`.** Call `list_accounts` first to discover available inboxes. `account_id` accepts either the configured `id` or the account's email address (`from_address`, `imap_username`, or `smtp_username`).

| Tool | Description |
|------|-------------|
| `list_accounts` | List all configured inboxes (no IMAP connection — cheap discovery) |
| `search_emails` | Search by from, to, subject, date range |
| `read_email` | Read full email content by UID |
| `send_email` | Send email with text/HTML, CC/BCC, reply threading (can be disabled) |
| `create_draft` | Save email as draft for review before sending |
| `archive_email` | Move email to Archive folder |
| `move_email` | Move email between folders |
| `mark_email` | Set read/unread/flagged/unflagged |
| `list_folders` | List all IMAP folders |
| `create_folder` | Create a new folder |
| `rename_folder` | Rename a folder |
| `delete_folder` | Delete a folder (protected folders blocked) |
| `get_server_info` | Server status and account connectivity (performs live IMAP check per account) |

### Breaking change in v1.1.0

`account_id` is now **required** on every per-account tool (`search_emails`, `read_email`, `send_email`, `create_draft`, `archive_email`, `move_email`, `mark_email`, `list_folders`, `create_folder`, `rename_folder`, `delete_folder`). Previously, omitting it silently fell back to the first configured account. Agents must now call `list_accounts` (or pass an email address) to specify which inbox to act on.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_API_KEY` | — | Bearer token to secure the MCP server. Optional when `POKE_TUNNEL=1`. |
| `POKE_TUNNEL` | `0` | Set to `1` when running behind the poke tunnel — skips `MCP_API_KEY` auth requirement. `start.sh` sets this automatically. |
| `CONFIG_PATH` | `config.yml` | Path to config file |
| `POKE_WEBHOOK_URL` | from config | Overrides webhook URL in config |
| `POKE_API_KEY` | from config | Overrides Poke API key in config |
| `PORT` | `3000` | HTTP server port |

## Poke Setup

Connect your MCP server to Poke at [poke.com/settings/connections](https://poke.com/settings/connections). Add the bearer token (`MCP_API_KEY`) in the connection auth settings.

The IDLE watcher automatically forwards new emails to Poke. You can also use the tools directly through Poke's AI agent.
