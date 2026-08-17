# AGENTS.md

Guidance for coding agents working in `mail-mcp`.

## Project Snapshot

- Language: Go (1.25)
- Module: `github.com/kacperkwapisz/mail-mcp`
- Domain: IMAP + SMTP mailbox access exposed over MCP
- Transports: Streamable HTTP (default, bearer-authenticated) and stdio
- Entry point: `cmd/mail-mcp/main.go`

## Commands

Run from the repo root.

| Task | Command |
| --- | --- |
| Build | `make build` (or `go build ./...`) |
| Run | `make run` |
| Run on stdio | `make stdio` |
| Test | `go test ./...` |
| Test with race detector | `make race` |
| Coverage | `make cover` |
| Format | `gofmt -w .` |
| Vet | `go vet ./...` |
| Lint | `golangci-lint run` |
| Everything | `make check` |
| Container | `make docker` |

### Running a single test

```
go test ./internal/msgid/ -run TestRoundTrip
go test ./internal/tools/ -run TestRequiredFields -v
go test ./internal/send/ -run 'TestBuild.*' -v
```

## Validation Before Finishing

1. `gofmt -l .` must print nothing
2. `go vet ./...`
3. `go test ./...`
4. `go mod tidy` must leave `go.mod` / `go.sum` unchanged

Do not skip vet or tests for code changes.

## Package Ownership

| Package | Responsibility |
| --- | --- |
| `cmd/mail-mcp` | Flags, transport selection, HTTP wiring, graceful shutdown |
| `internal/config` | YAML loading, defaults, validation, account resolution, gates |
| `internal/msgid` | Opaque message handle encode/parse |
| `internal/mailmime` | MIME parsing, body extraction, sanitization, attachment extraction |
| `internal/mailbox` | IMAP connection pool and operations |
| `internal/send` | SMTP composition, delivery, validation |
| `internal/tools` | MCP tool definitions and handlers |
| `internal/httpx` | Bearer auth, rate limiting, route filtering, security headers |

Keep the layering one-directional: `tools` depends on `mailbox`/`send`/`config`, never the reverse.

## Invariants — Do Not Break These

### Credentials never cross the tool boundary

Tool inputs and outputs carry `account_id`, never a host, username, or password. `internal/tools/tools_test.go` asserts this. If you add a field to any output struct, make sure it cannot carry connection details.

### Message handles are opaque and self-contained

A `message_id` encodes account + mailbox + UIDVALIDITY + UID. Consequences:

- Per-message tools must **not** accept an `account_id` parameter. The handle already names the account; accepting both would let a caller aim an operation at the wrong mailbox where the same UID is valid but points elsewhere.
- Every operation on a handle must call `Session.SelectFor`, which re-checks UIDVALIDITY.
- The encoding in `msgid.Encode` is persisted by agents across turns. Changing it invalidates every cached handle, so treat it as a wire format.

### Gates default closed

`allow_send` and `allow_delete` are false unless configured. `delete_email` additionally requires `confirm: true`. Never add a code path that sends or deletes without passing through `requireSend` / `requireDelete`.

### Attachment bytes stay out of responses

`read_email` returns metadata only. `get_attachment` writes to disk and returns a path. Do not add an option that inlines attachment content into a tool result.

### Send validation precedes the network

`send.Validate` runs before any socket opens. New send parameters need validation there, not after connecting.

### Wrapper-leak rejection stays wired

`send.ValidateNoWrapperLeak` guards every outgoing body. It exists because prompt instructions alone did not stop models from leaking tool-call markup into recipients' inboxes. Any new send path must call it.

## Code Style

### Imports

Three groups, separated by blank lines, in order:

1. standard library
2. external modules
3. `github.com/kacperkwapisz/mail-mcp/...`

### Errors

- Wrap with `%w` and enough context to act on: which account, which folder, which address.
- Error strings are read by an LLM. Say what went wrong *and* what to do instead — see `requireSend` for the tone.
- Never `panic` in a request path. `Pool.Do` recovers, but do not rely on it.
- Return errors from tool handlers rather than encoding failure in the output struct, so the SDK marks the result `IsError` and the model can self-correct.

### Naming

- Types `UpperCamelCase`, functions `mixedCaps`, no underscores.
- Use protocol vocabulary where it is protocol behaviour: `UIDVALIDITY`, `mailbox`, `part_id`.
- Use user vocabulary in tool names and descriptions: `folder`, not `mailbox`.

### Comments

Explain *why*, not *what*. Existing comments justify non-obvious decisions — the provider-aware Sent logic, per-connection serialization, UID EXPUNGE over EXPUNGE. Match that. Do not narrate control flow.

### Tool schemas

- Add `jsonschema:"..."` descriptions to every field. A test enforces this.
- Optional fields need `omitempty`; required fields must not have it.
- Booleans defaulting to true must be `*bool` so unset is distinguishable from false.

### Concurrency

- All IMAP work goes through `Pool.Do`, which holds the per-account lock.
- Do not hold a `*Session` past the end of its callback.
- Every network call must be timeout-bounded by a configured value, never a hardcoded one.

## Testing Expectations

- Pure logic (parsing, validation, pagination, roles, handles) gets unit tests.
- Tool registration, schemas, and gating are covered through an in-memory MCP client in `internal/tools/tools_test.go`. Add new tools to `TestAllToolsRegister`.
- Bug fixes get a regression test reproducing the original failure.
- Tests must not require network access or a live mailbox.

## Change Discipline

- Prefer minimal, focused diffs.
- Do not rename tools without an explicit request — clients cache them.
- Do not add compatibility shims unless asked. The one exception already present is legacy flat config keys, which exist to carry poke-mail v1 configs across the rename.
- Adding an env var or config key means updating `config.example.yml`, `.env.example`, `README.md`, and this file together.
