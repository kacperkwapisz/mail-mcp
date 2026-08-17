// Package send composes and delivers outgoing mail over SMTP.
//
// Every send returns the exact bytes that went over the wire, so the caller
// can APPEND a byte-identical copy to the IMAP Sent folder rather than
// reconstructing an approximation of it.
package send

import (
	"bytes"
	"context"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	gomail "github.com/wneessen/go-mail"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
)

// MaxRecipients bounds a single send. An agent looping over a contact list
// should be visibly rejected rather than quietly turned into a bulk mailer.
const MaxRecipients = 50

// MaxSubjectLen is the RFC 5322 practical line limit.
const MaxSubjectLen = 998

// Attachment is a file to attach. Exactly one source field must be set.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

// Composition is a message to build and send.
type Composition struct {
	From     string
	FromName string
	To       []string
	Cc       []string
	Bcc      []string
	Subject  string
	BodyText string
	BodyHTML string

	ReplyTo    string
	InReplyTo  string
	References []string

	Attachments []Attachment
}

// Result describes a delivered message.
type Result struct {
	// MessageID is the RFC 822 Message-ID header actually sent.
	MessageID string
	// Raw is the serialized message, suitable for IMAP APPEND.
	Raw []byte
	// Recipients counts To + Cc + Bcc.
	Recipients int
}

// Send builds and delivers the message.
func Send(ctx context.Context, acc *config.Account, timeouts config.Timeouts, comp *Composition) (*Result, error) {
	msg, err := Build(acc, comp)
	if err != nil {
		return nil, err
	}

	var raw bytes.Buffer
	if _, err := msg.WriteTo(&raw); err != nil {
		return nil, fmt.Errorf("serialize message: %w", err)
	}

	client, err := newClient(acc, timeouts)
	if err != nil {
		return nil, err
	}

	// The send timeout covers DATA transmission, which for a large
	// attachment on a slow uplink legitimately takes minutes.
	ctx, cancel := context.WithTimeout(ctx, timeouts.SMTPSend)
	defer cancel()

	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return nil, classifySMTPError(err)
	}

	return &Result{
		MessageID:  strings.TrimSpace(msg.GetMessageID()),
		Raw:        raw.Bytes(),
		Recipients: len(comp.To) + len(comp.Cc) + len(comp.Bcc),
	}, nil
}

// Verify checks connectivity and credentials without sending anything.
func Verify(ctx context.Context, acc *config.Account, timeouts config.Timeouts) error {
	client, err := newClient(acc, timeouts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeouts.SMTPConnect)
	defer cancel()

	if err := client.DialWithContext(ctx); err != nil {
		return classifySMTPError(err)
	}
	return client.Close()
}

// Build assembles the MIME message without sending it.
//
// Exported so drafts can reuse exactly the same composition path as sends;
// a draft that differs structurally from the eventual send is a bug factory.
func Build(acc *config.Account, comp *Composition) (*gomail.Msg, error) {
	if err := Validate(comp); err != nil {
		return nil, err
	}

	msg := gomail.NewMsg(gomail.WithCharset(gomail.CharsetUTF8))

	from := comp.From
	if from == "" {
		from = acc.FromAddress
	}
	name := comp.FromName
	if name == "" {
		name = acc.FromName
	}
	if name != "" {
		if err := msg.FromFormat(name, from); err != nil {
			return nil, fmt.Errorf("invalid From address %q: %w", from, err)
		}
	} else if err := msg.From(from); err != nil {
		return nil, fmt.Errorf("invalid From address %q: %w", from, err)
	}

	if err := msg.To(comp.To...); err != nil {
		return nil, fmt.Errorf("invalid To address: %w", err)
	}
	if len(comp.Cc) > 0 {
		if err := msg.Cc(comp.Cc...); err != nil {
			return nil, fmt.Errorf("invalid Cc address: %w", err)
		}
	}
	if len(comp.Bcc) > 0 {
		if err := msg.Bcc(comp.Bcc...); err != nil {
			return nil, fmt.Errorf("invalid Bcc address: %w", err)
		}
	}
	if comp.ReplyTo != "" {
		if err := msg.ReplyTo(comp.ReplyTo); err != nil {
			return nil, fmt.Errorf("invalid Reply-To address %q: %w", comp.ReplyTo, err)
		}
	}

	msg.Subject(comp.Subject)
	msg.SetDate()
	// Generate the Message-ID up front so the caller can report it and match
	// the archived copy against the delivered one.
	msg.SetMessageID()

	if comp.InReplyTo != "" {
		msg.SetGenHeader(gomail.HeaderInReplyTo, ensureAngleBrackets(comp.InReplyTo))
	}
	if len(comp.References) > 0 {
		refs := make([]string, 0, len(comp.References))
		for _, r := range comp.References {
			if r = strings.TrimSpace(r); r != "" {
				refs = append(refs, ensureAngleBrackets(r))
			}
		}
		if len(refs) > 0 {
			msg.SetGenHeader(gomail.HeaderReferences, strings.Join(refs, " "))
		}
	}

	text := sanitizeBody(comp.BodyText)
	html := sanitizeBody(comp.BodyHTML)
	switch {
	case text != "" && html != "":
		msg.SetBodyString(gomail.TypeTextPlain, text)
		msg.AddAlternativeString(gomail.TypeTextHTML, html)
	case html != "":
		// Still provide a text fallback: a bare text/html message trips
		// spam filters and is unreadable in text-only clients.
		msg.SetBodyString(gomail.TypeTextPlain, htmlFallbackNotice)
		msg.AddAlternativeString(gomail.TypeTextHTML, html)
	default:
		msg.SetBodyString(gomail.TypeTextPlain, text)
	}

	for _, att := range comp.Attachments {
		name := filepath.Base(att.Filename)
		if name == "" || name == "." || name == string(filepath.Separator) {
			name = "attachment"
		}
		contentType := att.ContentType
		if contentType == "" {
			contentType = GuessContentType(name)
		}
		if err := msg.AttachReader(name, bytes.NewReader(att.Content),
			gomail.WithFileContentType(gomail.ContentType(contentType)),
		); err != nil {
			return nil, fmt.Errorf("attach %q: %w", name, err)
		}
	}

	return msg, nil
}

const htmlFallbackNotice = "This message is formatted in HTML. Please view it in an HTML-capable mail client."

// Validate rejects malformed compositions before any socket is opened.
func Validate(comp *Composition) error {
	if err := ValidateRecipients(comp.To, "to"); err != nil {
		return err
	}
	if len(comp.Cc) > 0 {
		if err := ValidateRecipients(comp.Cc, "cc"); err != nil {
			return err
		}
	}
	if len(comp.Bcc) > 0 {
		if err := ValidateRecipients(comp.Bcc, "bcc"); err != nil {
			return err
		}
	}
	total := len(comp.To) + len(comp.Cc) + len(comp.Bcc)
	if total > MaxRecipients {
		return fmt.Errorf("%d total recipients exceeds the limit of %d", total, MaxRecipients)
	}

	subject := strings.TrimSpace(comp.Subject)
	if subject == "" {
		return fmt.Errorf("subject is required")
	}
	if len(subject) > MaxSubjectLen {
		return fmt.Errorf("subject is %d characters, over the %d limit", len(subject), MaxSubjectLen)
	}
	if strings.ContainsAny(comp.Subject, "\r\n") {
		return fmt.Errorf("subject must not contain line breaks")
	}

	if strings.TrimSpace(comp.BodyText) == "" && strings.TrimSpace(comp.BodyHTML) == "" {
		return fmt.Errorf("provide body_text, body_html, or both")
	}
	if err := ValidateNoWrapperLeak("body_text", comp.BodyText); err != nil {
		return err
	}
	if err := ValidateNoWrapperLeak("body_html", comp.BodyHTML); err != nil {
		return err
	}
	if err := ValidateNoWrapperLeak("subject", comp.Subject); err != nil {
		return err
	}
	return nil
}

// ValidateRecipients checks an address list for shape and size.
func ValidateRecipients(addrs []string, field string) error {
	if len(addrs) == 0 {
		return fmt.Errorf("%s needs at least one recipient", field)
	}
	if len(addrs) > MaxRecipients {
		return fmt.Errorf("%s has %d recipients, over the limit of %d", field, len(addrs), MaxRecipients)
	}
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			return fmt.Errorf("%s contains an empty address", field)
		}
		if strings.ContainsAny(a, "\r\n") {
			// Newlines in an address are header injection, not a typo.
			return fmt.Errorf("%s contains a line break in %q", field, a)
		}
		if _, err := mail.ParseAddress(a); err != nil {
			return fmt.Errorf("%s contains an invalid address %q", field, a)
		}
	}
	return nil
}

// wrapperMarkers are tool-call syntax fragments that have no legitimate place
// in an email a human will read.
//
// LLMs periodically concatenate body_text and body_html into one argument and
// leak the separator markup into the recipient's inbox. Prompt instructions
// alone do not reliably prevent this, so the server refuses the call.
var wrapperMarkers = []string{
	"<body_text>", "</body_text>",
	"<body_html>", "</body_html>",
	"<function_calls>", "</function_calls>",
	"<invoke name=", "</invoke>",
	`<parameter name="body_`,
	"<parameter name='body_",
}

// ValidateNoWrapperLeak rejects a field carrying tool-call wrapper syntax.
func ValidateNoWrapperLeak(field, value string) error {
	if value == "" {
		return nil
	}
	lower := strings.ToLower(value)
	for _, marker := range wrapperMarkers {
		if strings.Contains(lower, marker) {
			return fmt.Errorf(
				"%s contains tool-call wrapper syntax (%q). body_text and body_html are separate "+
					"parameters — pass them as two distinct fields and never wrap content in pseudo-tags",
				field, marker)
		}
	}
	return nil
}

// LoadAttachment reads an attachment from disk, bounded by maxBytes.
func LoadAttachment(path string, maxBytes int64) (*Attachment, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read attachment %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("attachment %q is a directory", path)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("attachment %q is %d bytes, over the %d byte limit", path, info.Size(), maxBytes)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read attachment %q: %w", path, err)
	}
	name := filepath.Base(path)
	return &Attachment{
		Filename:    name,
		ContentType: GuessContentType(name),
		Content:     content,
	}, nil
}

// contentTypesByExt covers the formats that actually travel by email.
// Anything else falls back to application/octet-stream, which every client
// handles as a download.
var contentTypesByExt = map[string]string{
	".pdf": "application/pdf", ".txt": "text/plain", ".csv": "text/csv",
	".html": "text/html", ".htm": "text/html", ".xml": "application/xml",
	".json": "application/json", ".md": "text/markdown", ".ics": "text/calendar",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml",
	".heic": "image/heic", ".bmp": "image/bmp", ".tiff": "image/tiff",
	".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar",
	".7z":   "application/x-7z-compressed",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".mp3":  "audio/mpeg", ".mp4": "video/mp4", ".mov": "video/quicktime",
	".eml": "message/rfc822",
}

// GuessContentType infers a MIME type from a filename extension.
func GuessContentType(filename string) string {
	if ct, ok := contentTypesByExt[strings.ToLower(filepath.Ext(filename))]; ok {
		return ct
	}
	return "application/octet-stream"
}

// ---- internals -------------------------------------------------------------

func newClient(acc *config.Account, timeouts config.Timeouts) (*gomail.Client, error) {
	opts := []gomail.Option{
		gomail.WithPort(acc.SMTP.Port),
		gomail.WithTimeout(timeouts.SMTPConnect),
	}

	switch acc.SMTP.Security {
	case config.SecurityTLS:
		opts = append(opts, gomail.WithSSL())
	case config.SecuritySTARTTLS:
		// Mandatory, not opportunistic: silently falling back to plaintext
		// would send the password and the message in the clear.
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	case config.SecurityPlain:
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		return nil, fmt.Errorf("account %q: unsupported smtp security %q", acc.ID, acc.SMTP.Security)
	}

	if acc.SMTP.Username != "" && acc.SMTP.Password != "" {
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover),
			gomail.WithUsername(acc.SMTP.Username),
			gomail.WithPassword(acc.SMTP.Password),
		)
	} else {
		opts = append(opts, gomail.WithSMTPAuth(gomail.SMTPAuthNoAuth))
	}

	client, err := gomail.NewClient(acc.SMTP.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("configure SMTP client for %s: %w", acc.SMTP.Host, err)
	}
	return client, nil
}

// classifySMTPError turns a transport error into something actionable.
func classifySMTPError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "535"), strings.Contains(lower, "534"),
		strings.Contains(lower, "authentication failed"),
		strings.Contains(lower, "auth"):
		return fmt.Errorf("SMTP authentication failed — check the account's smtp username/password "+
			"(most providers require an app-specific password, not your login password): %s", msg)
	case strings.Contains(lower, "context deadline exceeded"), strings.Contains(lower, "timeout"):
		return fmt.Errorf("SMTP operation timed out: %s", msg)
	case strings.Contains(lower, "certificate"), strings.Contains(lower, "tls"):
		return fmt.Errorf("SMTP TLS negotiation failed — check host, port, and security mode: %s", msg)
	case strings.Contains(lower, "550"), strings.Contains(lower, "553"), strings.Contains(lower, "554"):
		return fmt.Errorf("SMTP server rejected the message (relay or policy denial): %s", msg)
	default:
		return fmt.Errorf("SMTP send failed: %s", msg)
	}
}

// sanitizeBody strips CDATA fragments that some webmail composers (notably
// Zoho) leak into plain text when converting from an HTML template.
func sanitizeBody(s string) string {
	if s == "" {
		return ""
	}
	return strings.NewReplacer("]]>", "", "<![CDATA[", "").Replace(s)
}

func ensureAngleBrackets(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if !strings.HasPrefix(id, "<") {
		id = "<" + id
	}
	if !strings.HasSuffix(id, ">") {
		id += ">"
	}
	return id
}

// QuoteOriginal renders the standard forwarded-message block.
func QuoteOriginal(from, date, subject, to, body string) string {
	var b strings.Builder
	b.WriteString("\n\n---------- Forwarded message ----------\n")
	b.WriteString("From: " + from + "\n")
	if date != "" {
		b.WriteString("Date: " + date + "\n")
	}
	b.WriteString("Subject: " + subject + "\n")
	if to != "" {
		b.WriteString("To: " + to + "\n")
	}
	b.WriteString("\n")
	b.WriteString(body)
	return b.String()
}

// QuoteReply renders the standard attribution line plus a quoted body.
func QuoteReply(from string, date time.Time, rawDate, body string) string {
	when := rawDate
	if !date.IsZero() {
		when = date.Format("Mon, 2 Jan 2006 at 15:04")
	}
	var b strings.Builder
	b.WriteString("\n\nOn " + when + ", " + from + " wrote:\n")
	for _, line := range strings.Split(body, "\n") {
		b.WriteString("> " + line + "\n")
	}
	return b.String()
}
