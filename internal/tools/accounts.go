package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/send"
)

// AccountInfo is the non-secret description of a configured mailbox.
type AccountInfo struct {
	ID          string `json:"id" jsonschema:"identifier to pass as account_id"`
	FromAddress string `json:"from_address" jsonschema:"address outgoing mail is sent from"`
	FromName    string `json:"from_name,omitempty" jsonschema:"display name on outgoing mail"`
	IMAPHost    string `json:"imap_host" jsonschema:"IMAP server hostname"`
	SMTPHost    string `json:"smtp_host" jsonschema:"SMTP server hostname"`
	CanSend     bool   `json:"can_send" jsonschema:"false when send_email is disabled for this account"`
	CanDelete   bool   `json:"can_delete" jsonschema:"false when delete_email and delete_folder are disabled for this account"`
	SavesSent   bool   `json:"saves_sent_copy" jsonschema:"true when the server files a copy of outgoing mail in the Sent folder"`
}

type listAccountsOutput struct {
	Summary  string        `json:"summary" jsonschema:"one-line description of the result"`
	Accounts []AccountInfo `json:"accounts" jsonschema:"every configured mailbox"`
}

type verifyAccountOutput struct {
	Summary   string `json:"summary" jsonschema:"one-line description of the result"`
	AccountID string `json:"account_id" jsonschema:"account that was checked"`
	IMAPOK    bool   `json:"imap_ok" jsonschema:"true when IMAP connected and authenticated"`
	IMAPError string `json:"imap_error,omitempty" jsonschema:"reason IMAP failed, if it did"`
	SMTPOK    bool   `json:"smtp_ok" jsonschema:"true when SMTP connected and authenticated"`
	SMTPError string `json:"smtp_error,omitempty" jsonschema:"reason SMTP failed, if it did"`
}

type serverInfoOutput struct {
	Summary            string   `json:"summary" jsonschema:"one-line description of the result"`
	Name               string   `json:"name" jsonschema:"server name"`
	Version            string   `json:"version" jsonschema:"server version"`
	AccountIDs         []string `json:"account_ids" jsonschema:"every configured account id"`
	MaxBodyChars       int      `json:"max_body_chars" jsonschema:"per-part body truncation limit used by read_email"`
	MaxSearchResults   int      `json:"max_search_results" jsonschema:"largest page size search_emails will return"`
	MaxAttachmentBytes int64    `json:"max_attachment_bytes" jsonschema:"largest attachment get_attachment will download"`
	AttachmentDir      string   `json:"attachment_dir" jsonschema:"default directory get_attachment writes into"`
}

func (s *Server) registerAccounts(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_accounts",
		Title:       "List mailboxes",
		Description: "List every configured mailbox. Call this first — all other tools need an account_id or a message_id obtained from a search. Makes no network connections.",
		Annotations: readOnlyTool(),
	}, s.listAccounts)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "verify_account",
		Title:       "Verify mailbox connectivity",
		Description: "Test IMAP and SMTP connectivity and authentication for one account without sending or reading anything. Use this to diagnose failures.",
		Annotations: readOnlyTool(),
	}, s.verifyAccount)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_server_info",
		Title:       "Server information",
		Description: "Report the server version and the operational limits that govern the other tools. Makes no network connections.",
		Annotations: readOnlyTool(),
	}, s.serverInfo)
}

func (s *Server) listAccounts(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listAccountsOutput, error) {
	accounts := make([]AccountInfo, 0, len(s.cfg.Accounts))
	for _, a := range s.cfg.Accounts {
		accounts = append(accounts, AccountInfo{
			ID:          a.ID,
			FromAddress: a.FromAddress,
			FromName:    a.FromName,
			IMAPHost:    a.IMAP.Host,
			SMTPHost:    a.SMTP.Host,
			CanSend:     s.cfg.SendAllowed(a),
			CanDelete:   s.cfg.DeleteAllowed(a),
			SavesSent:   s.cfg.ShouldSaveSent(a),
		})
	}
	return nil, listAccountsOutput{
		Summary:  pluralize(len(accounts), "mailbox", "mailboxes") + " configured",
		Accounts: accounts,
	}, nil
}

func (s *Server) verifyAccount(ctx context.Context, _ *mcp.CallToolRequest, in accountInput) (*mcp.CallToolResult, verifyAccountOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, verifyAccountOutput{}, err
	}

	out := verifyAccountOutput{AccountID: acc.ID}

	if err := mailbox.Verify(acc, s.cfg.Timeouts.IMAPConnect); err != nil {
		out.IMAPError = err.Error()
	} else {
		out.IMAPOK = true
	}

	if err := send.Verify(ctx, acc, s.cfg.Timeouts); err != nil {
		out.SMTPError = err.Error()
	} else {
		out.SMTPOK = true
	}

	switch {
	case out.IMAPOK && out.SMTPOK:
		out.Summary = "IMAP and SMTP both reachable and authenticated"
	case out.IMAPOK:
		out.Summary = "IMAP works; SMTP failed"
	case out.SMTPOK:
		out.Summary = "SMTP works; IMAP failed"
	default:
		out.Summary = "both IMAP and SMTP failed"
	}
	return nil, out, nil
}

func (s *Server) serverInfo(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, serverInfoOutput, error) {
	return nil, serverInfoOutput{
		Summary:            "mail-mcp " + s.version,
		Name:               "mail-mcp",
		Version:            s.version,
		AccountIDs:         s.cfg.AccountIDs(),
		MaxBodyChars:       s.cfg.Limits.MaxBodyChars,
		MaxSearchResults:   s.cfg.Limits.MaxSearchResults,
		MaxAttachmentBytes: s.cfg.Limits.MaxAttachmentBytes,
		AttachmentDir:      s.cfg.Limits.AttachmentDir,
	}, nil
}
