package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/mailmime"
)

type searchInput struct {
	accountInput
	Folder  string `json:"folder,omitempty" jsonschema:"folder to search; defaults to INBOX. Use list_folders to see the options"`
	From    string `json:"from,omitempty" jsonschema:"substring match against the From header"`
	To      string `json:"to,omitempty" jsonschema:"substring match against the To header"`
	Subject string `json:"subject,omitempty" jsonschema:"substring match against the Subject header"`
	Body    string `json:"body,omitempty" jsonschema:"substring match against the message body"`
	Text    string `json:"text,omitempty" jsonschema:"substring match against headers and body together"`
	Since   string `json:"since,omitempty" jsonschema:"only messages on or after this date, YYYY-MM-DD"`
	Before  string `json:"before,omitempty" jsonschema:"only messages before this date, YYYY-MM-DD"`
	Unseen  bool   `json:"unseen,omitempty" jsonschema:"only unread messages"`
	Seen    bool   `json:"seen,omitempty" jsonschema:"only already-read messages"`
	Flagged bool   `json:"flagged,omitempty" jsonschema:"only flagged/starred messages"`
	Limit   int    `json:"limit,omitempty" jsonschema:"how many messages to return, newest first; defaults to 25"`
	Offset  int    `json:"offset,omitempty" jsonschema:"how many of the newest matches to skip; use with limit to page backwards in time"`
}

type searchOutput struct {
	Summary    string            `json:"summary" jsonschema:"one-line description of the result"`
	Folder     string            `json:"folder" jsonschema:"folder that was searched"`
	Total      int               `json:"total_matches" jsonschema:"how many messages matched in total"`
	Returned   int               `json:"returned" jsonschema:"how many are in this page"`
	Offset     int               `json:"offset" jsonschema:"offset this page started at"`
	NextOffset int               `json:"next_offset,omitempty" jsonschema:"offset to pass for the following page; absent on the last page"`
	HasMore    bool              `json:"has_more" jsonschema:"true when more matches remain"`
	Messages   []mailbox.Summary `json:"messages" jsonschema:"matching messages, newest first"`
	Folders    []mailbox.Folder  `json:"-"`
}

type readInput struct {
	messageInput
	IncludeHTML    bool `json:"include_html,omitempty" jsonschema:"also return the sanitized HTML body; costs context, so only ask when the formatting matters"`
	IncludeHeaders bool `json:"include_headers,omitempty" jsonschema:"also return the full header set; useful for debugging delivery or threading"`
	MarkAsRead     bool `json:"mark_as_read,omitempty" jsonschema:"mark the message \\Seen; defaults to false so reading does not disturb the user's own unread state"`
	MaxBodyChars   int  `json:"max_body_chars,omitempty" jsonschema:"override the per-part truncation limit"`
}

type readOutput struct {
	Summary   string            `json:"summary" jsonschema:"one-line description of the result"`
	MessageID string            `json:"message_id" jsonschema:"handle for this message, unchanged"`
	Mailbox   string            `json:"mailbox" jsonschema:"folder the message lives in"`
	AccountID string            `json:"account_id" jsonschema:"account the message belongs to"`
	Message   *mailmime.Message `json:"message" jsonschema:"the parsed message"`
}

type getAttachmentInput struct {
	messageInput
	PartID    string `json:"part_id,omitempty" jsonschema:"part id from read_email's attachment list; the precise way to select one"`
	Filename  string `json:"filename,omitempty" jsonschema:"attachment filename, matched case-insensitively; use when you only know the name"`
	OutputDir string `json:"output_dir,omitempty" jsonschema:"directory to write into; defaults to the server's configured attachment directory"`
}

type getAttachmentOutput struct {
	Summary     string `json:"summary" jsonschema:"one-line description of the result"`
	FilePath    string `json:"file_path" jsonschema:"absolute path of the written file; pass this to a local file-reading tool"`
	Filename    string `json:"filename" jsonschema:"sanitized filename on disk"`
	ContentType string `json:"content_type" jsonschema:"MIME type of the attachment"`
	SizeBytes   int    `json:"size_bytes" jsonschema:"size of the written file"`
	PartID      string `json:"part_id" jsonschema:"part id that was matched"`
}

func (s *Server) registerRead(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "search_emails",
		Title: "Search messages",
		Description: "Search a folder and return message metadata, newest first. This is the only source of message_id handles. " +
			"Returns metadata only — no bodies and no attachments — so it is cheap to call. Combine criteria to narrow results; " +
			"with none, it returns the most recent messages in the folder.",
		Annotations: readOnlyTool(),
	}, s.searchEmails)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "read_email",
		Title: "Read a message",
		Description: "Return one message's headers, body, and attachment metadata. Bodies are truncated to the server's limit and " +
			"HTML is omitted unless requested. Attachment bytes are never included — use get_attachment for those.",
		Annotations: readOnlyTool(),
	}, s.readEmail)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "get_attachment",
		Title: "Download an attachment",
		Description: "Write one attachment to a file on disk and return its path. The bytes are deliberately kept out of the response " +
			"so a large file cannot flood the context; open the returned path with a local file tool instead. " +
			"Select the attachment by part_id (preferred) or filename, both from read_email.",
		Annotations: writeTool(),
	}, s.getAttachment)
}

func (s *Server) searchEmails(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, searchOutput{}, err
	}
	if in.Seen && in.Unseen {
		return nil, searchOutput{}, fmt.Errorf("seen and unseen are mutually exclusive")
	}

	folder := firstNonEmpty(in.Folder, "INBOX")
	limit := clamp(orDefault(in.Limit, 25), 1, s.cfg.Limits.MaxSearchResults)
	if in.Offset < 0 {
		return nil, searchOutput{}, fmt.Errorf("offset must not be negative")
	}

	opts := mailbox.SearchOptions{
		From: in.From, To: in.To, Subject: in.Subject,
		Body: in.Body, Text: in.Text,
		Unseen: in.Unseen, Seen: in.Seen, Flagged: in.Flagged,
	}
	if opts.Since, err = parseDate(in.Since, "since"); err != nil {
		return nil, searchOutput{}, err
	}
	if opts.Before, err = parseDate(in.Before, "before"); err != nil {
		return nil, searchOutput{}, err
	}

	var out searchOutput
	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		uidValidity, err := sess.Select(folder, true)
		if err != nil {
			return err
		}
		uids, err := sess.Search(opts)
		if err != nil {
			return err
		}

		// IMAP returns UIDs oldest-first; agents almost always want the
		// newest, so page from the end.
		page := paginateNewestFirst(uids, in.Offset, limit)
		summaries, err := sess.FetchSummaries(acc.ID, folder, uidValidity, page)
		if err != nil {
			return err
		}
		// FETCH results arrive in server order, not request order.
		sortByUIDDesc(summaries, page)

		out = searchOutput{
			Folder:   folder,
			Total:    len(uids),
			Returned: len(summaries),
			Offset:   in.Offset,
			HasMore:  in.Offset+len(page) < len(uids),
			Messages: summaries,
		}
		if out.HasMore {
			out.NextOffset = in.Offset + len(page)
		}
		return nil
	})
	if err != nil {
		return nil, searchOutput{}, err
	}

	out.Summary = fmt.Sprintf("%d of %d match(es) in %s", out.Returned, out.Total, folder)
	if out.HasMore {
		out.Summary += fmt.Sprintf("; call again with offset %d for more", out.NextOffset)
	}
	return nil, out, nil
}

func (s *Server) readEmail(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, readOutput, error) {
	id, acc, err := s.resolveMessage(in.MessageID)
	if err != nil {
		return nil, readOutput{}, err
	}

	maxChars := s.cfg.Limits.MaxBodyChars
	if in.MaxBodyChars > 0 {
		maxChars = clamp(in.MaxBodyChars, 100, s.cfg.Limits.MaxBodyChars)
	}

	var parsed *mailmime.Message
	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		// Marking as read is a write, so the mailbox must not be read-only.
		if err := sess.SelectFor(id, !in.MarkAsRead); err != nil {
			return err
		}
		raw, err := sess.FetchRaw(imap.UID(id.UID), !in.MarkAsRead)
		if err != nil {
			return err
		}
		parsed, err = mailmime.Parse(raw, mailmime.ParseOptions{
			MaxBodyChars:   maxChars,
			IncludeHTML:    in.IncludeHTML,
			IncludeHeaders: in.IncludeHeaders,
		})
		return err
	})
	if err != nil {
		return nil, readOutput{}, err
	}

	summary := fmt.Sprintf("%q from %s", parsed.Subject, parsed.From)
	if n := len(parsed.Attachments); n > 0 {
		summary += fmt.Sprintf(" with %s", pluralize(n, "attachment", "attachments"))
	}
	if parsed.BodyTextTrunc {
		summary += " (body truncated)"
	}

	return nil, readOutput{
		Summary:   summary,
		MessageID: in.MessageID,
		Mailbox:   id.Mailbox,
		AccountID: acc.ID,
		Message:   parsed,
	}, nil
}

func (s *Server) getAttachment(ctx context.Context, _ *mcp.CallToolRequest, in getAttachmentInput) (*mcp.CallToolResult, getAttachmentOutput, error) {
	id, acc, err := s.resolveMessage(in.MessageID)
	if err != nil {
		return nil, getAttachmentOutput{}, err
	}
	if in.PartID == "" && in.Filename == "" {
		return nil, getAttachmentOutput{}, fmt.Errorf("provide part_id or filename; read_email lists both for every attachment")
	}

	outputDir := firstNonEmpty(in.OutputDir, s.cfg.Limits.AttachmentDir)
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, getAttachmentOutput{}, fmt.Errorf("cannot create output directory %q: %w", outputDir, err)
	}

	var extracted *mailmime.ExtractedAttachment
	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		if err := sess.SelectFor(id, true); err != nil {
			return err
		}
		raw, err := sess.FetchRaw(imap.UID(id.UID), true)
		if err != nil {
			return err
		}
		extracted, err = mailmime.ExtractAttachment(raw, in.PartID, in.Filename, s.cfg.Limits.MaxAttachmentBytes)
		return err
	})
	if err != nil {
		return nil, getAttachmentOutput{}, err
	}

	// Prefix with the uid and part id so two attachments named invoice.pdf
	// from different messages cannot overwrite one another.
	safeName := mailmime.SanitizeFilename(extracted.Filename)
	diskName := fmt.Sprintf("%d-%s-%s", id.UID, strings.ReplaceAll(extracted.PartID, ".", "_"), safeName)
	path := filepath.Join(outputDir, diskName)

	if err := os.WriteFile(path, extracted.Content, 0o600); err != nil {
		return nil, getAttachmentOutput{}, fmt.Errorf("write attachment to %q: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	return nil, getAttachmentOutput{
		Summary:     fmt.Sprintf("saved %s (%d bytes) to %s", safeName, len(extracted.Content), abs),
		FilePath:    abs,
		Filename:    safeName,
		ContentType: extracted.ContentType,
		SizeBytes:   len(extracted.Content),
		PartID:      extracted.PartID,
	}, nil
}

// ---- helpers ---------------------------------------------------------------

// paginateNewestFirst takes a page from the end of an oldest-first UID list.
func paginateNewestFirst(uids []imap.UID, offset, limit int) []imap.UID {
	if offset >= len(uids) {
		return nil
	}
	end := len(uids) - offset
	start := end - limit
	if start < 0 {
		start = 0
	}
	page := make([]imap.UID, 0, end-start)
	for i := end - 1; i >= start; i-- {
		page = append(page, uids[i])
	}
	return page
}

// sortByUIDDesc reorders summaries to match the requested UID order, since
// the server may return FETCH results in any order.
func sortByUIDDesc(summaries []mailbox.Summary, order []imap.UID) {
	position := make(map[string]int, len(order))
	for i, uid := range order {
		position[fmt.Sprint(uid)] = i
	}
	// Summaries carry an opaque id, so rank by the trailing uid segment.
	rank := func(s mailbox.Summary) int {
		idx := strings.LastIndex(s.MessageID, ".")
		if idx < 0 {
			return len(order)
		}
		if p, ok := position[s.MessageID[idx+1:]]; ok {
			return p
		}
		return len(order)
	}
	for i := 1; i < len(summaries); i++ {
		for j := i; j > 0 && rank(summaries[j]) < rank(summaries[j-1]); j-- {
			summaries[j], summaries[j-1] = summaries[j-1], summaries[j]
		}
	}
}

func parseDate(v, field string) (time.Time, error) {
	if strings.TrimSpace(v) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a date like 2024-01-31, got %q", field, v)
	}
	return t, nil
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
