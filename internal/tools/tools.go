// Package tools exposes mailbox operations as MCP tools.
//
// Credentials never cross this boundary: tools accept an opaque account_id
// (or an opaque message_id that embeds one) and the server resolves it
// against server-side configuration. An agent can act on a mailbox without
// ever learning how to connect to it.
package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/msgid"
)

// Server holds the dependencies shared by every tool handler.
type Server struct {
	cfg     *config.Config
	pool    *mailbox.Pool
	logger  *slog.Logger
	version string
}

// New builds a tool server.
func New(cfg *config.Config, pool *mailbox.Pool, logger *slog.Logger, version string) *Server {
	return &Server{cfg: cfg, pool: pool, logger: logger, version: version}
}

// Register attaches every tool to the MCP server.
func (s *Server) Register(srv *mcp.Server) {
	s.registerAccounts(srv)
	s.registerRead(srv)
	s.registerSend(srv)
	s.registerManage(srv)
	s.registerFolders(srv)
}

// Instructions is the guidance handed to the connecting client.
const Instructions = `Read, search, organize, and send email across the mailboxes configured on this server.

DISCOVERY. Call list_accounts first. Every tool needs either an account_id or a
message_id, and message ids are only obtainable from search_emails.

MESSAGE IDS. A message_id is an opaque handle that already identifies the
account, folder, and message. Pass it back exactly as received — never edit,
truncate, or rebuild one, and never pair it with a different account. If a tool
reports a stale handle, the folder was renumbered: search again for fresh ids.

BODIES ARE SEPARATE FIELDS. body_text and body_html are two distinct
parameters. Never concatenate them into one string and never wrap content in
pseudo-tags such as <body_text> or <parameter name="body_html">. The server
rejects any send whose fields contain tool-call syntax.

FORMATTING. For correspondence a person will read, supply both body_text and
body_html so the message renders well while keeping a plain-text fallback.
Convert markdown to real HTML tags (<p>, <strong>, <ul>, <a href>) before
sending — never paste raw markdown into body_html. For short automated notes,
body_text alone is fine.

BEFORE SENDING. Show the user To, Cc, Bcc, Subject, the full body rendered as
readable text, and any attachment names, then wait for explicit approval. Do
not dump raw HTML source into that preview; describe it as a formatted message
instead. Send only what the user approved.

READING COSTS CONTEXT. search_emails returns metadata only. read_email returns
one message and omits HTML unless you ask for it. Attachment bytes are never
inlined — read_email lists attachment metadata, and get_attachment writes a
chosen file to disk and returns its path.

DESTRUCTIVE ACTIONS. Prefer archive_email over delete_email. Deleting requires
confirm: true, and moves the message to Trash rather than erasing it.`

// ---- shared input fragments ------------------------------------------------

// accountInput is embedded by tools that act on a whole account.
type accountInput struct {
	AccountID string `json:"account_id" jsonschema:"account to act on; accepts the configured id or the account's email address. Call list_accounts to discover valid values"`
}

// messageInput is embedded by tools that act on one message.
//
// No account_id: the handle already carries it, which makes it impossible to
// aim an operation at the wrong mailbox.
type messageInput struct {
	MessageID string `json:"message_id" jsonschema:"opaque message handle returned by search_emails; identifies the account, folder, and message"`
}

// AttachmentInput is a file to attach to an outgoing message.
type AttachmentInput struct {
	FilePath      string `json:"file_path,omitempty" jsonschema:"path to a file on the server's filesystem; preferred for anything non-trivial"`
	Filename      string `json:"filename,omitempty" jsonschema:"name the recipient sees; inferred from file_path when omitted"`
	ContentType   string `json:"content_type,omitempty" jsonschema:"MIME type; inferred from the file extension when omitted"`
	ContentBase64 string `json:"content_base64,omitempty" jsonschema:"base64-encoded content; use only for small generated files, prefer file_path otherwise"`
}

// ---- helpers ---------------------------------------------------------------

// resolveAccount looks up an account by id or address.
func (s *Server) resolveAccount(id string) (*config.Account, error) {
	return s.cfg.Resolve(id)
}

// resolveMessage parses a handle and resolves the account it names.
func (s *Server) resolveMessage(handle string) (msgid.ID, *config.Account, error) {
	id, err := msgid.Parse(handle)
	if err != nil {
		return msgid.ID{}, nil, err
	}
	acc, err := s.cfg.Resolve(id.Account)
	if err != nil {
		return msgid.ID{}, nil, fmt.Errorf("message_id references account %q which is no longer configured: %w", id.Account, err)
	}
	return id, acc, nil
}

// requireSend rejects the call when sending is gated off for the account.
func (s *Server) requireSend(acc *config.Account) error {
	if !s.cfg.SendAllowed(acc) {
		return fmt.Errorf("sending is disabled for account %q. Use create_draft instead, "+
			"or set allow_send: true for this account in the server config", acc.ID)
	}
	return nil
}

// requireDelete rejects the call when deletion is gated off for the account.
func (s *Server) requireDelete(acc *config.Account) error {
	if !s.cfg.DeleteAllowed(acc) {
		return fmt.Errorf("deleting is disabled for account %q. Use archive_email or move_email instead, "+
			"or set allow_delete: true for this account in the server config", acc.ID)
	}
	return nil
}

// withSession runs fn against a pooled connection for acc.
func (s *Server) withSession(ctx context.Context, acc *config.Account, fn func(*mailbox.Session) error) error {
	return s.pool.Do(ctx, acc, fn)
}

// readOnlyTool marks tools that never modify server state.
func readOnlyTool() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true}
}

// writeTool marks tools that modify state but are safe to retry.
func writeTool() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{IdempotentHint: true}
}

// destructiveTool marks tools that remove or relocate user data.
func destructiveTool() *mcp.ToolAnnotations {
	t := true
	return &mcp.ToolAnnotations{DestructiveHint: &t}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
