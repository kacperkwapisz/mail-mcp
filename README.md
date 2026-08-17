# mail-mcp

An MCP server that gives AI agents controlled access to IMAP and SMTP mailboxes — read, search, organize, and send — without ever handing them your credentials.

Written in Go. Single static binary, no runtime dependencies, ~20 MB resident.

## Why credentials stay server-side

The agent never sees a hostname, username, or password. Tools take an opaque `account_id`; the server resolves it against a local config file and makes the connection itself. Run it remotely and the model has no way to reconstruct how to reach your mailbox, even if it wanted to.

The same idea extends to message handles. A `message_id` is an opaque token encoding the account, folder, UIDVALIDITY, and UID together — so an agent cannot pair a handle from one mailbox with a different account, and a folder that gets renumbered produces a clear "this handle is stale" error instead of quietly acting on the wrong message.

## Install

### Docker

```bash
docker run -d --name mail-mcp \
  -p 3000:3000 \
  -v "$PWD/config.yml:/config.yml:ro" \
  -e MCP_API_KEY="$(openssl rand -hex 32)" \
  ghcr.io/kacperkwapisz/mail-mcp:latest
```

### From source

```bash
git clone https://github.com/kacperkwapisz/mail-mcp.git
cd mail-mcp
make build          # → bin/mail-mcp
```

### Binaries

Download for your platform from [Releases](https://github.com/kacperkwapisz/mail-mcp/releases).

## Setup

**1. Write the config.**

```bash
cp config.example.yml config.yml
```

```yaml
allow_send: false # opt in per account below
allow_delete: false

accounts:
  - id: icloud
    imap:
      host: imap.mail.me.com
      username: you@icloud.com
      password: xxxx-xxxx-xxxx-xxxx # app-specific password
    smtp:
      host: smtp.mail.me.com
      username: you@icloud.com
      password: xxxx-xxxx-xxxx-xxxx
    from_address: you@yourdomain.com
    from_name: Your Name
    allow_send: true
```

Most providers need an app-specific password rather than your account password — [iCloud](https://support.apple.com/en-us/102654), [Gmail](https://support.google.com/accounts/answer/185833), Fastmail, and Zoho all work this way.

SMTP inherits the IMAP host and credentials when omitted, and connection security is inferred from the port (993 → TLS, 143 → STARTTLS, 465 → TLS, 587 → STARTTLS) unless you set `security` explicitly.

**2. Generate a token and run.**

```bash
export MCP_API_KEY=$(openssl rand -hex 32)
./bin/mail-mcp --config config.yml
```

The HTTP transport refuses to start without `MCP_API_KEY`. There is no unauthenticated mode — this process can read and send your mail.

**3. Verify.**

```bash
npx @modelcontextprotocol/inspector
```

Connect to `http://localhost:3000/mcp` over Streamable HTTP with an `Authorization: Bearer <MCP_API_KEY>` header, then call `verify_account` to confirm both IMAP and SMTP authenticate.

### Local use

For a client on the same machine, stdio skips the network entirely and needs no token:

```jsonc
{
  "mcpServers": {
    "mail": {
      "command": "/usr/local/bin/mail-mcp",
      "args": ["--config", "/etc/mail-mcp/config.yml", "--transport", "stdio"]
    }
  }
}
```

## Tools

**Discovery**

| Tool              | Purpose                                                            |
| ----------------- | ------------------------------------------------------------------ |
| `list_accounts`   | Every configured mailbox and what it's allowed to do. No network.   |
| `verify_account`  | Live IMAP + SMTP connectivity and auth check.                       |
| `get_server_info` | Version and the operational limits governing the other tools.       |

**Reading**

| Tool             | Purpose                                                                  |
| ---------------- | ------------------------------------------------------------------------ |
| `search_emails`  | Search by sender, recipient, subject, body, date, or flags. Paginated.    |
| `read_email`     | One message: headers, body, attachment metadata. HTML opt-in.             |
| `get_attachment` | Write one attachment to disk, return the path.                            |

**Sending**

| Tool             | Purpose                                                            |
| ---------------- | ------------------------------------------------------------------ |
| `send_email`     | New message, with attachments and full threading control.          |
| `reply_email`    | Reply with derived subject, recipients, and threading headers.     |
| `forward_email`  | Forward, carrying attachments and quoting the original.            |
| `create_draft`   | Save to Drafts without sending. Works even when sending is off.    |

**Organizing**

| Tool             | Purpose                                                       |
| ---------------- | ------------------------------------------------------------- |
| `archive_email`  | Move to the account's Archive folder.                         |
| `move_email`     | Move to any folder.                                           |
| `mark_email`     | read / unread / flagged / unflagged / answered / unanswered.  |
| `delete_email`   | Move to Trash. Gated, and requires `confirm: true`.           |
| `list_folders`   | Folders with their normalized roles.                          |
| `create_folder` · `rename_folder` · `delete_folder` | Folder management.         |

## Design decisions

**Attachment bytes never enter the response.** `read_email` returns attachment metadata with a `part_id`; `get_attachment` writes the file to disk and returns a path. A 7 MB PDF base64-encoded into a tool result would blow the context window without accomplishing anything — hand the model a path and let it open the file with a local tool.

**Bodies are truncated and HTML is opt-in.** Message HTML is attacker-controlled and enormous. It is sanitized through bluemonday before it is ever returned, and omitted entirely unless `include_html` is set. Messages with no plain-text part get one derived from the HTML, with paragraph breaks preserved.

**One pooled IMAP connection per account.** A TLS handshake plus LOGIN on every tool call is the single biggest source of latency in servers of this kind. Connections are kept authenticated, health-checked with NOOP after idling, and reconnected transparently. Operations on one account are serialized, since IMAP's selected-mailbox state makes ordering matter; different accounts run concurrently.

**Every network operation is bounded.** Separate timeouts for IMAP connect, IMAP command, SMTP connect, and SMTP send — the last one generous, because DATA transmission for a large attachment on a slow uplink legitimately takes minutes. A command that exceeds its budget closes the socket, which is the only thing that reliably unblocks a stuck IMAP read.

**Sent copies are the exact bytes that were delivered.** Rather than rebuilding an approximation for the Sent folder, the serialized message is captured before transmission and APPENDed verbatim. Whether to append at all is provider-aware: Gmail and Zoho file their own copy on submission, so a second one is redundant or a visible duplicate; iCloud, Office 365, and generic relays file nothing, so skipping it loses the copy.

**Send validation happens before a socket opens.** Recipients, subject length, header injection via embedded newlines, and body presence are all checked locally. A malformed call fails instantly with a specific message instead of after a TLS handshake.

**Tool-call syntax in a body is rejected outright.** LLMs periodically concatenate `body_text` and `body_html` into one argument and leak the separator markup into the recipient's inbox. Prompt instructions do not reliably prevent this, so the server refuses any send whose fields contain `</body_text>`, `<parameter name="body_html">`, and similar markers. Legitimate technical content that merely mentions `<parameter>` still passes.

**Folder roles come from the server, not a name list.** SPECIAL-USE attributes are used where available, with a localized name table as fallback — so a Polish "Wysłane" or a Gmail "[Gmail]/Sent Mail" is recognized as Sent rather than triggering the creation of a duplicate folder.

**Destructive operations are gated twice.** `allow_delete` is false by default, and even when enabled `delete_email` requires `confirm: true` per call and moves to Trash rather than expunging. `delete_folder` additionally refuses INBOX and any special-use folder.

## Configuration reference

### Environment

| Variable              | Default      | Purpose                                                         |
| --------------------- | ------------ | --------------------------------------------------------------- |
| `MCP_API_KEY`         | —            | Bearer token. **Required** for HTTP; unused for stdio.           |
| `CONFIG_PATH`         | `config.yml` | Path to the YAML config.                                         |
| `TRANSPORT`           | `http`       | `http` or `stdio`.                                               |
| `PORT` / `ADDR`       | `3000`       | Listen port or full address.                                     |
| `LOG_LEVEL`           | `info`       | `debug`, `info`, `warn`, `error`.                                |
| `TRUST_PROXY`         | `false`      | Trust `X-Forwarded-For` for rate limiting.                       |
| `RATE_LIMIT_GET_RPM`  | `60`         | Per-client GET budget per minute.                                |
| `RATE_LIMIT_POST_RPM` | `240`        | Per-client POST budget per minute.                               |

Flags mirror these: `--config`, `--transport`, `--addr`, `--log-level`, `--trust-proxy`, `--version`.

### Config file

See [`config.example.yml`](config.example.yml) for the annotated version.

| Key                          | Default        | Purpose                                                    |
| ---------------------------- | -------------- | ---------------------------------------------------------- |
| `allow_send`                 | `false`        | Global send gate.                                          |
| `allow_delete`               | `false`        | Global delete gate.                                        |
| `limits.max_body_chars`      | `50000`        | Per-part body truncation.                                  |
| `limits.max_search_results`  | `100`          | Largest search page.                                       |
| `limits.max_attachment_bytes`| `26214400`     | Attachment size ceiling.                                   |
| `limits.attachment_dir`      | system temp    | Where `get_attachment` writes.                             |
| `timeouts.*`                 | see example    | `imap_connect`, `imap_command`, `smtp_connect`, `smtp_send`. |
| `accounts[].allow_send`      | inherits global| Per-account send gate.                                     |
| `accounts[].allow_delete`    | inherits global| Per-account delete gate.                                   |
| `accounts[].save_sent`       | provider-aware | Force the Sent-folder copy on or off.                      |

Configs written for poke-mail v1 still load: the flat `imap_host` / `smtp_username` style keys are folded into the nested form automatically.

## Security

- **Authentication is mandatory** on HTTP. No token, no start.
- **Credentials never leave the server.** Tools receive ids, not connection details.
- **Everything outside `/mcp` returns an empty 404**, revealing nothing to scanners.
- **TLS is mandatory for STARTTLS accounts.** No opportunistic fallback to plaintext, which would send your password in the clear.
- **Rate limited per client**, with separate GET and POST budgets so polling cannot starve real work. `X-Forwarded-For` is ignored unless you declare a trusted proxy, since otherwise any caller could spoof it.
- **Attachment filenames are sanitized** before touching the filesystem — path separators, traversal segments, and control characters are stripped.
- **HTML is sanitized** with bluemonday before it is returned.
- **DNS rebinding protection** is on by default via the MCP SDK.

Put it behind TLS in any real deployment — a reverse proxy or a tunnel. The bearer token is the only thing standing between the internet and your mail.

## Development

```bash
make check    # fmt + vet + test
make test
make race
make cover
make build
make docker
```

## License

MIT — see [LICENSE](LICENSE).
