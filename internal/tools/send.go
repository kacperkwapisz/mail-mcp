package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/mailmime"
	"github.com/kacperkwapisz/mail-mcp/internal/msgid"
	"github.com/kacperkwapisz/mail-mcp/internal/send"
)

// composeFields are the body and recipient fields shared by send and draft.
type composeFields struct {
	To          []string          `json:"to" jsonschema:"recipient addresses; plain addresses or \"Name <addr@example.com>\""`
	Cc          []string          `json:"cc,omitempty" jsonschema:"carbon-copy recipients"`
	Bcc         []string          `json:"bcc,omitempty" jsonschema:"blind carbon-copy recipients"`
	Subject     string            `json:"subject" jsonschema:"subject line, single line, no line breaks"`
	BodyText    string            `json:"body_text,omitempty" jsonschema:"plain text body. A SEPARATE field from body_html — never merge the two"`
	BodyHTML    string            `json:"body_html,omitempty" jsonschema:"HTML body using real tags such as <p>, <strong>, <ul>, <a href>. A SEPARATE field from body_text — never merge the two, and never pass raw markdown"`
	ReplyTo     string            `json:"reply_to,omitempty" jsonschema:"Reply-To address, when replies should go somewhere other than the From address"`
	InReplyTo   string            `json:"in_reply_to,omitempty" jsonschema:"Message-ID header this message replies to; prefer reply_email, which fills this in for you"`
	References  []string          `json:"references,omitempty" jsonschema:"Message-ID chain of the thread"`
	Attachments []AttachmentInput `json:"attachments,omitempty" jsonschema:"files to attach"`
}

type sendInput struct {
	accountInput
	composeFields
}

type replyInput struct {
	messageInput
	BodyText                   string            `json:"body_text,omitempty" jsonschema:"plain text reply. A SEPARATE field from body_html — never merge the two"`
	BodyHTML                   string            `json:"body_html,omitempty" jsonschema:"HTML reply using real tags. A SEPARATE field from body_text — never merge the two"`
	ReplyAll                   *bool             `json:"reply_all,omitempty" jsonschema:"reply to every original recipient instead of only the sender; defaults to false"`
	QuoteOriginal              *bool             `json:"quote_original,omitempty" jsonschema:"append the quoted original message below the reply; defaults to true"`
	IncludeOriginalAttachments bool              `json:"include_original_attachments,omitempty" jsonschema:"re-attach the original message's files to the reply"`
	Attachments                []AttachmentInput `json:"attachments,omitempty" jsonschema:"additional files to attach"`
}

type forwardInput struct {
	messageInput
	To                 []string          `json:"to" jsonschema:"recipient addresses"`
	Cc                 []string          `json:"cc,omitempty" jsonschema:"carbon-copy recipients"`
	BodyText           string            `json:"body_text,omitempty" jsonschema:"optional note above the forwarded message"`
	BodyHTML           string            `json:"body_html,omitempty" jsonschema:"optional HTML note; sent as-is, the original is not embedded into it"`
	IncludeAttachments *bool             `json:"include_attachments,omitempty" jsonschema:"carry the original's attachments along; defaults to true"`
	Attachments        []AttachmentInput `json:"attachments,omitempty" jsonschema:"additional files to attach"`
}

type draftInput struct {
	accountInput
	composeFields
	Folder string `json:"folder,omitempty" jsonschema:"folder to save into; defaults to the account's Drafts folder"`
}

type sendOutput struct {
	Summary       string `json:"summary" jsonschema:"one-line description of the result"`
	Status        string `json:"status" jsonschema:"always \"sent\" on success"`
	AccountID     string `json:"account_id" jsonschema:"account the message was sent from"`
	MessageIDHdr  string `json:"message_id_header" jsonschema:"RFC 822 Message-ID of the sent message"`
	Recipients    int    `json:"recipients" jsonschema:"total To + Cc + Bcc count"`
	SavedToSent   bool   `json:"saved_to_sent" jsonschema:"true when a copy was filed in the Sent folder"`
	SentFolder    string `json:"sent_folder,omitempty" jsonschema:"folder the copy was filed in"`
	SaveSentError string `json:"save_sent_error,omitempty" jsonschema:"why the Sent copy failed; the message was still delivered"`
	InReplyTo     string `json:"in_reply_to,omitempty" jsonschema:"handle of the message this was a reply to"`
	ForwardedFrom string `json:"forwarded_from,omitempty" jsonschema:"handle of the message that was forwarded"`
}

type draftOutput struct {
	Summary      string `json:"summary" jsonschema:"one-line description of the result"`
	Status       string `json:"status" jsonschema:"always \"saved\" on success"`
	AccountID    string `json:"account_id" jsonschema:"account the draft belongs to"`
	Folder       string `json:"folder" jsonschema:"folder the draft was saved into"`
	MessageIDHdr string `json:"message_id_header" jsonschema:"RFC 822 Message-ID of the draft"`
}

func (s *Server) registerSend(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "send_email",
		Title: "Send a message",
		Description: "Send a new email. Show the user the full recipients, subject, body, and attachment names and get explicit approval first. " +
			"body_text and body_html are separate parameters; supply both for correspondence a person will read. " +
			"Disabled unless the account allows sending — use create_draft otherwise.",
	}, s.sendEmail)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "reply_email",
		Title: "Reply to a message",
		Description: "Reply to an existing message. Threading headers, the Re: subject, and the recipient list are derived from the original, " +
			"so pass only the reply body. Use reply_all to include every original recipient.",
	}, s.replyEmail)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forward_email",
		Title:       "Forward a message",
		Description: "Forward an existing message to new recipients, carrying its attachments by default and quoting the original below any note you add.",
	}, s.forwardEmail)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "create_draft",
		Title: "Save a draft",
		Description: "Save a message to the Drafts folder without sending it. Always available, including on accounts where sending is disabled — " +
			"use this when the user should review or send the message themselves.",
		Annotations: writeTool(),
	}, s.createDraft)
}

func (s *Server) sendEmail(ctx context.Context, _ *mcp.CallToolRequest, in sendInput) (*mcp.CallToolResult, sendOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, sendOutput{}, err
	}
	if err := s.requireSend(acc); err != nil {
		return nil, sendOutput{}, err
	}

	attachments, err := s.loadAttachments(in.Attachments)
	if err != nil {
		return nil, sendOutput{}, err
	}

	comp := &send.Composition{
		To: in.To, Cc: in.Cc, Bcc: in.Bcc,
		Subject:  in.Subject,
		BodyText: in.BodyText, BodyHTML: in.BodyHTML,
		ReplyTo: in.ReplyTo, InReplyTo: in.InReplyTo, References: in.References,
		Attachments: attachments,
	}

	out, err := s.deliver(ctx, acc, comp)
	if err != nil {
		return nil, sendOutput{}, err
	}
	return nil, out, nil
}

func (s *Server) replyEmail(ctx context.Context, _ *mcp.CallToolRequest, in replyInput) (*mcp.CallToolResult, sendOutput, error) {
	id, acc, err := s.resolveMessage(in.MessageID)
	if err != nil {
		return nil, sendOutput{}, err
	}
	if err := s.requireSend(acc); err != nil {
		return nil, sendOutput{}, err
	}
	if strings.TrimSpace(in.BodyText) == "" && strings.TrimSpace(in.BodyHTML) == "" {
		return nil, sendOutput{}, fmt.Errorf("provide body_text, body_html, or both")
	}

	original, raw, err := s.fetchOriginal(ctx, acc, id)
	if err != nil {
		return nil, sendOutput{}, err
	}

	comp, err := buildReply(acc, original, raw, &in)
	if err != nil {
		return nil, sendOutput{}, err
	}
	extra, err := s.loadAttachments(in.Attachments)
	if err != nil {
		return nil, sendOutput{}, err
	}
	comp.Attachments = append(comp.Attachments, extra...)

	out, err := s.deliver(ctx, acc, comp)
	if err != nil {
		return nil, sendOutput{}, err
	}
	out.InReplyTo = in.MessageID
	out.Summary = fmt.Sprintf("replied to %q (%s)", original.Subject, strings.Join(comp.To, ", "))
	return nil, out, nil
}

func (s *Server) forwardEmail(ctx context.Context, _ *mcp.CallToolRequest, in forwardInput) (*mcp.CallToolResult, sendOutput, error) {
	id, acc, err := s.resolveMessage(in.MessageID)
	if err != nil {
		return nil, sendOutput{}, err
	}
	if err := s.requireSend(acc); err != nil {
		return nil, sendOutput{}, err
	}

	original, raw, err := s.fetchOriginal(ctx, acc, id)
	if err != nil {
		return nil, sendOutput{}, err
	}

	subject := original.Subject
	if !hasPrefixFold(subject, "fwd:") && !hasPrefixFold(subject, "fw:") {
		subject = "Fwd: " + subject
	}

	body := in.BodyText + send.QuoteOriginal(
		original.From, original.Date, original.Subject,
		strings.Join(original.To, ", "), original.BodyText,
	)

	comp := &send.Composition{
		To: in.To, Cc: in.Cc,
		Subject:  subject,
		BodyText: body, BodyHTML: in.BodyHTML,
	}

	if boolOr(in.IncludeAttachments, true) {
		for _, meta := range original.Attachments {
			if meta.Inline {
				continue // part of the original HTML body, not a real file
			}
			extracted, err := mailmime.ExtractAttachment(raw, meta.PartID, "", s.cfg.Limits.MaxAttachmentBytes)
			if err != nil {
				// One unreadable attachment should not block the forward.
				s.logger.Warn("skipping attachment while forwarding",
					"account", acc.ID, "part", meta.PartID, "error", err)
				continue
			}
			comp.Attachments = append(comp.Attachments, send.Attachment{
				Filename:    extracted.Filename,
				ContentType: extracted.ContentType,
				Content:     extracted.Content,
			})
		}
	}

	extra, err := s.loadAttachments(in.Attachments)
	if err != nil {
		return nil, sendOutput{}, err
	}
	comp.Attachments = append(comp.Attachments, extra...)

	out, err := s.deliver(ctx, acc, comp)
	if err != nil {
		return nil, sendOutput{}, err
	}
	out.ForwardedFrom = in.MessageID
	out.Summary = fmt.Sprintf("forwarded %q to %s", original.Subject, strings.Join(in.To, ", "))
	return nil, out, nil
}

func (s *Server) createDraft(ctx context.Context, _ *mcp.CallToolRequest, in draftInput) (*mcp.CallToolResult, draftOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, draftOutput{}, err
	}

	attachments, err := s.loadAttachments(in.Attachments)
	if err != nil {
		return nil, draftOutput{}, err
	}

	msg, err := send.Build(acc, &send.Composition{
		To: in.To, Cc: in.Cc, Bcc: in.Bcc,
		Subject:  in.Subject,
		BodyText: in.BodyText, BodyHTML: in.BodyHTML,
		ReplyTo: in.ReplyTo, InReplyTo: in.InReplyTo, References: in.References,
		Attachments: attachments,
	})
	if err != nil {
		return nil, draftOutput{}, err
	}

	var buf strings.Builder
	if _, err := msg.WriteTo(&stringWriter{&buf}); err != nil {
		return nil, draftOutput{}, fmt.Errorf("serialize draft: %w", err)
	}
	rawDraft := []byte(buf.String())

	folder := in.Folder
	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		if folder == "" {
			detected, err := sess.FolderForRole("drafts")
			if err != nil {
				return err
			}
			folder = firstNonEmpty(detected, "Drafts")
		}
		// \Seen alongside \Draft keeps the folder from showing a phantom
		// unread count for something the user just composed.
		return sess.Append(folder, rawDraft, []imap.Flag{imap.FlagDraft, imap.FlagSeen})
	})
	if err != nil {
		return nil, draftOutput{}, err
	}

	return nil, draftOutput{
		Summary:      fmt.Sprintf("draft %q saved to %s", in.Subject, folder),
		Status:       "saved",
		AccountID:    acc.ID,
		Folder:       folder,
		MessageIDHdr: strings.TrimSpace(msg.GetMessageID()),
	}, nil
}

// ---- shared machinery ------------------------------------------------------

// deliver sends the composition and files a Sent copy when configured.
func (s *Server) deliver(ctx context.Context, acc *config.Account, comp *send.Composition) (sendOutput, error) {
	result, err := send.Send(ctx, acc, s.cfg.Timeouts, comp)
	if err != nil {
		return sendOutput{}, err
	}

	out := sendOutput{
		Summary:      fmt.Sprintf("sent %q to %s", comp.Subject, strings.Join(comp.To, ", ")),
		Status:       "sent",
		AccountID:    acc.ID,
		MessageIDHdr: result.MessageID,
		Recipients:   result.Recipients,
	}

	if !s.cfg.ShouldSaveSent(acc) {
		return out, nil
	}

	// The mail is already delivered at this point, so a failure to archive a
	// copy is reported alongside success rather than turned into an error —
	// telling the agent the send failed would invite a duplicate send.
	saveErr := s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		folder, err := sess.FolderForRole("sent")
		if err != nil {
			return err
		}
		folder = firstNonEmpty(folder, "Sent")
		if err := sess.Append(folder, result.Raw, []imap.Flag{imap.FlagSeen}); err != nil {
			return err
		}
		out.SentFolder = folder
		return nil
	})
	if saveErr != nil {
		s.logger.Warn("could not file a copy in the Sent folder", "account", acc.ID, "error", saveErr)
		out.SaveSentError = saveErr.Error()
		return out, nil
	}
	out.SavedToSent = true
	return out, nil
}

// fetchOriginal loads and parses the message a reply or forward is based on.
func (s *Server) fetchOriginal(ctx context.Context, acc *config.Account, id msgid.ID) (*mailmime.Message, []byte, error) {
	var (
		parsed *mailmime.Message
		raw    []byte
	)
	err := s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		if err := sess.SelectFor(id, true); err != nil {
			return err
		}
		var err error
		raw, err = sess.FetchRaw(imap.UID(id.UID), true)
		if err != nil {
			return err
		}
		parsed, err = mailmime.Parse(raw, mailmime.ParseOptions{MaxBodyChars: s.cfg.Limits.MaxBodyChars})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return parsed, raw, nil
}

// buildReply derives recipients, subject, and threading headers from the
// original message.
func buildReply(acc *config.Account, original *mailmime.Message, _ []byte, in *replyInput) (*send.Composition, error) {
	subject := original.Subject
	if !hasPrefixFold(subject, "re:") {
		subject = "Re: " + subject
	}

	// Reply-To wins over From when the sender asked for it.
	to := original.ReplyTo
	if len(to) == 0 {
		to = []string{original.From}
	}

	var cc []string
	if boolOr(in.ReplyAll, false) {
		for _, addr := range append(append([]string{}, original.To...), original.Cc...) {
			if !isSelf(addr, acc) && !containsAddress(to, addr) && !containsAddress(cc, addr) {
				cc = append(cc, addr)
			}
		}
	}

	to = dedupeAddresses(filterOut(to, func(a string) bool { return strings.TrimSpace(a) == "" }))
	if len(to) == 0 {
		return nil, fmt.Errorf("the original message has no usable sender address to reply to")
	}

	// References chain: whatever the original carried, plus the original itself.
	references := append([]string{}, original.References...)
	if original.MessageID != "" && !contains(references, original.MessageID) {
		references = append(references, original.MessageID)
	}

	bodyText := in.BodyText
	if boolOr(in.QuoteOriginal, true) && original.BodyText != "" {
		bodyText += send.QuoteReply(original.From, parseTimeLoose(original.Date), original.Date, original.BodyText)
	}

	comp := &send.Composition{
		To: to, Cc: cc,
		Subject:  subject,
		BodyText: bodyText, BodyHTML: in.BodyHTML,
		InReplyTo:  original.MessageID,
		References: references,
	}
	return comp, nil
}

// loadAttachments resolves tool inputs into concrete bytes.
func (s *Server) loadAttachments(inputs []AttachmentInput) ([]send.Attachment, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([]send.Attachment, 0, len(inputs))
	for i, in := range inputs {
		switch {
		case in.FilePath != "":
			att, err := send.LoadAttachment(in.FilePath, s.cfg.Limits.MaxAttachmentBytes)
			if err != nil {
				return nil, err
			}
			if in.Filename != "" {
				att.Filename = in.Filename
			}
			if in.ContentType != "" {
				att.ContentType = in.ContentType
			}
			out = append(out, *att)

		case in.ContentBase64 != "":
			content, err := base64.StdEncoding.DecodeString(in.ContentBase64)
			if err != nil {
				return nil, fmt.Errorf("attachment %d has invalid base64 content: %w", i+1, err)
			}
			if int64(len(content)) > s.cfg.Limits.MaxAttachmentBytes {
				return nil, fmt.Errorf("attachment %d is %d bytes, over the %d byte limit",
					i+1, len(content), s.cfg.Limits.MaxAttachmentBytes)
			}
			name := firstNonEmpty(in.Filename, fmt.Sprintf("attachment-%d", i+1))
			out = append(out, send.Attachment{
				Filename:    mailmime.SanitizeFilename(name),
				ContentType: firstNonEmpty(in.ContentType, send.GuessContentType(name)),
				Content:     content,
			})

		default:
			return nil, fmt.Errorf("attachment %d needs either file_path or content_base64", i+1)
		}
	}
	return out, nil
}

// ---- small helpers ---------------------------------------------------------

// stringWriter adapts strings.Builder to io.Writer for go-mail's WriteTo.
type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// parseTimeLoose accepts the date formats that actually appear in the wild,
// falling back to a zero time so callers can use the raw string instead.
func parseTimeLoose(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// isSelf reports whether an address belongs to this account, so a reply-all
// does not copy the user on their own message.
func isSelf(addr string, acc *config.Account) bool {
	needle := strings.ToLower(extractEmail(addr))
	if needle == "" {
		return false
	}
	for _, mine := range []string{acc.FromAddress, acc.SMTP.Username, acc.IMAP.Username} {
		if mine != "" && strings.EqualFold(extractEmail(mine), needle) {
			return true
		}
	}
	return false
}

// extractEmail pulls the bare address out of "Name <addr>".
func extractEmail(addr string) string {
	addr = strings.TrimSpace(addr)
	if open := strings.LastIndex(addr, "<"); open >= 0 {
		if close := strings.Index(addr[open:], ">"); close > 0 {
			return strings.TrimSpace(addr[open+1 : open+close])
		}
	}
	return addr
}

func containsAddress(list []string, addr string) bool {
	needle := strings.ToLower(extractEmail(addr))
	for _, a := range list {
		if strings.EqualFold(extractEmail(a), needle) {
			return true
		}
	}
	return false
}

func dedupeAddresses(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, a := range list {
		key := strings.ToLower(extractEmail(a))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func filterOut(list []string, drop func(string) bool) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if !drop(v) {
			out = append(out, v)
		}
	}
	return out
}
