// Package mailmime parses RFC 822 messages into agent-friendly structures.
//
// Two concerns drive the design:
//
//  1. Context budget. A raw message can be megabytes; an agent's context
//     cannot. Bodies are truncated, HTML is opt-in, and attachment bytes are
//     never inlined — only metadata plus a part id the agent can use to
//     download the file separately.
//
//  2. Safety. Message HTML is attacker-controlled. It is sanitized before it
//     is ever handed back.
package mailmime

import (
	"fmt"
	"io"
	"mime"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	message "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // register non-UTF-8 charsets
	gomail "github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
)

// htmlPolicy strips scripts, styles, event handlers, and remote form targets
// while keeping the formatting a human would want to read.
var htmlPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("style").OnElements("span", "p", "div", "td", "th", "table")
	p.AllowElements("table", "thead", "tbody", "tr", "td", "th", "figure", "figcaption")
	p.AllowAttrs("colspan", "rowspan").OnElements("td", "th")
	// Images are allowed but remote loading is the client's decision, not ours.
	p.AllowImages()
	p.RequireNoFollowOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

// textPolicy removes every tag, leaving only text content.
var textPolicy = bluemonday.StrictPolicy()

// Attachment is metadata about one non-text part.
//
// Content is deliberately absent: use ExtractAttachment to fetch bytes.
type Attachment struct {
	// PartID is an IMAP-style dotted path (e.g. "2" or "1.3") identifying
	// this part within the message. Pass it to get_attachment.
	PartID string `json:"part_id" jsonschema:"IMAP-style part path identifying this attachment"`
	// Filename as declared by the sender, or "" when absent.
	Filename string `json:"filename" jsonschema:"declared filename, may be empty"`
	// ContentType is the MIME type, e.g. "application/pdf".
	ContentType string `json:"content_type" jsonschema:"MIME type of the attachment"`
	// Size is the decoded byte length.
	Size int64 `json:"size_bytes" jsonschema:"decoded size in bytes"`
	// Inline marks parts referenced from the HTML body (e.g. embedded images)
	// rather than presented as a separate file.
	Inline bool `json:"inline" jsonschema:"true for inline/embedded parts such as images in the HTML body"`
}

// Message is a parsed email.
type Message struct {
	From       string   `json:"from" jsonschema:"sender address"`
	To         []string `json:"to" jsonschema:"primary recipients"`
	Cc         []string `json:"cc,omitempty" jsonschema:"carbon-copy recipients"`
	ReplyTo    []string `json:"reply_to,omitempty" jsonschema:"Reply-To addresses if set"`
	Subject    string   `json:"subject" jsonschema:"decoded subject line"`
	Date       string   `json:"date" jsonschema:"RFC 3339 date the message was sent"`
	MessageID  string   `json:"message_id_header,omitempty" jsonschema:"RFC 822 Message-ID header of this message"`
	InReplyTo  string   `json:"in_reply_to,omitempty" jsonschema:"Message-ID this message replies to"`
	References []string `json:"references,omitempty" jsonschema:"Message-ID chain of the thread"`

	BodyText      string `json:"body_text" jsonschema:"plain text body, derived from HTML when no text part exists"`
	BodyTextTrunc bool   `json:"body_text_truncated" jsonschema:"true when body_text was cut off at the size limit"`
	BodyHTML      string `json:"body_html,omitempty" jsonschema:"sanitized HTML body, only present when requested"`
	BodyHTMLTrunc bool   `json:"body_html_truncated,omitempty" jsonschema:"true when body_html was cut off at the size limit"`

	Attachments []Attachment      `json:"attachments" jsonschema:"attachment metadata; use get_attachment to download bytes"`
	Headers     map[string]string `json:"headers,omitempty" jsonschema:"selected raw headers, only present when requested"`
}

// ParseOptions controls how much of a message is materialized.
type ParseOptions struct {
	// MaxBodyChars caps each body part. Zero means 50000.
	MaxBodyChars int
	// IncludeHTML returns the sanitized HTML body alongside the text body.
	IncludeHTML bool
	// IncludeHeaders returns the full header set.
	IncludeHeaders bool
}

const defaultMaxBodyChars = 50_000

// readCapFactor bounds how many raw bytes we buffer per text part relative to
// the character cap, leaving room for multi-byte runes and markup overhead.
const readCapFactor = 8

// Parse decodes a raw RFC 822 message.
func Parse(raw []byte, opts ParseOptions) (*Message, error) {
	if opts.MaxBodyChars <= 0 {
		opts.MaxBodyChars = defaultMaxBodyChars
	}

	entity, err := message.Read(strings.NewReader(string(raw)))
	if err != nil && message.IsUnknownCharset(err) {
		// Unknown charset is recoverable: the body is still readable, it may
		// just contain replacement characters.
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}

	hdr := gomail.Header{Header: entity.Header}
	out := &Message{
		From:        joinAddresses(addressList(&hdr, "From")),
		To:          addressList(&hdr, "To"),
		Cc:          addressList(&hdr, "Cc"),
		ReplyTo:     addressList(&hdr, "Reply-To"),
		Subject:     headerText(&entity.Header, "Subject"),
		MessageID:   strings.TrimSpace(entity.Header.Get("Message-ID")),
		InReplyTo:   strings.TrimSpace(entity.Header.Get("In-Reply-To")),
		References:  splitMsgIDs(entity.Header.Get("References")),
		Attachments: []Attachment{},
	}
	if t, err := hdr.Date(); err == nil && !t.IsZero() {
		out.Date = t.Format("2006-01-02T15:04:05Z07:00")
	} else {
		out.Date = strings.TrimSpace(entity.Header.Get("Date"))
	}
	if opts.IncludeHeaders {
		out.Headers = collectHeaders(&entity.Header)
	}

	var rawText, rawHTML string
	readCap := int64(opts.MaxBodyChars) * readCapFactor

	// Walk consumes the entity, so this is the single pass over the tree.
	walkErr := entity.Walk(func(path []int, part *message.Entity, partErr error) error {
		if partErr != nil {
			// Deliberately swallowed: one malformed sub-part should not cost
			// the caller the rest of an otherwise readable message. Returning
			// the error here would abort the whole walk.
			return nil //nolint:nilerr
		}
		if part.MultipartReader() != nil {
			return nil // container, not content
		}

		contentType, _, _ := part.Header.ContentType()
		disposition, dispParams, _ := part.Header.ContentDisposition()
		filename := dispParams["filename"]
		if filename == "" {
			if _, ctParams, _ := part.Header.ContentType(); ctParams != nil {
				filename = ctParams["name"]
			}
		}
		filename = decodeWord(filename)

		if isAttachment(contentType, disposition, filename) {
			size, _ := io.Copy(io.Discard, part.Body)
			out.Attachments = append(out.Attachments, Attachment{
				PartID:      imapPartID(path),
				Filename:    filename,
				ContentType: contentType,
				Size:        size,
				Inline:      strings.EqualFold(disposition, "inline"),
			})
			return nil
		}

		switch strings.ToLower(contentType) {
		case "text/plain":
			if rawText == "" {
				rawText = readCapped(part.Body, readCap)
			}
		case "text/html":
			if rawHTML == "" {
				rawHTML = readCapped(part.Body, readCap)
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk message parts: %w", walkErr)
	}

	// Prefer a real text part; fall back to flattening the HTML so an agent
	// always gets something readable.
	if strings.TrimSpace(rawText) == "" && rawHTML != "" {
		rawText = HTMLToText(rawHTML)
	}
	out.BodyText, out.BodyTextTrunc = truncate(normalizeNewlines(rawText), opts.MaxBodyChars)

	if opts.IncludeHTML && rawHTML != "" {
		out.BodyHTML, out.BodyHTMLTrunc = truncate(htmlPolicy.Sanitize(rawHTML), opts.MaxBodyChars)
	}

	return out, nil
}

// ExtractedAttachment carries decoded attachment bytes.
type ExtractedAttachment struct {
	PartID      string
	Filename    string
	ContentType string
	Content     []byte
}

// ExtractAttachment pulls one attachment out of a raw message.
//
// Exactly one of partID or filename must be supplied. Parts larger than
// maxBytes are refused rather than silently truncated, since a partial
// attachment on disk is worse than none.
func ExtractAttachment(raw []byte, partID, filename string, maxBytes int64) (*ExtractedAttachment, error) {
	if partID == "" && filename == "" {
		return nil, fmt.Errorf("provide either part_id or filename")
	}

	entity, err := message.Read(strings.NewReader(string(raw)))
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, fmt.Errorf("parse message: %w", err)
	}

	var found *ExtractedAttachment
	var tooLarge int64
	var available []string

	walkErr := entity.Walk(func(path []int, part *message.Entity, partErr error) error {
		// partErr is swallowed for the same reason as in Parse: a broken part
		// elsewhere in the tree must not hide the one we were asked for.
		if partErr != nil || found != nil || part.MultipartReader() != nil {
			return nil //nolint:nilerr
		}

		contentType, ctParams, _ := part.Header.ContentType()
		disposition, dispParams, _ := part.Header.ContentDisposition()
		partFilename := dispParams["filename"]
		if partFilename == "" && ctParams != nil {
			partFilename = ctParams["name"]
		}
		partFilename = decodeWord(partFilename)

		if !isAttachment(contentType, disposition, partFilename) {
			return nil
		}

		id := imapPartID(path)
		available = append(available, fmt.Sprintf("%s (%s)", id, orPlaceholder(partFilename)))

		matches := (partID != "" && id == partID) ||
			(partID == "" && filename != "" && strings.EqualFold(partFilename, filename))
		if !matches {
			io.Copy(io.Discard, part.Body) //nolint:errcheck // draining to reach the next part
			return nil
		}

		// Read one byte past the limit so an oversized part is detected
		// rather than silently clipped.
		content, err := io.ReadAll(io.LimitReader(part.Body, maxBytes+1))
		if err != nil {
			return fmt.Errorf("read attachment body: %w", err)
		}
		if int64(len(content)) > maxBytes {
			tooLarge = int64(len(content))
			return nil
		}

		name := partFilename
		if name == "" {
			name = "attachment-" + strings.ReplaceAll(id, ".", "-")
			if exts, _ := mime.ExtensionsByType(contentType); len(exts) > 0 {
				name += exts[0]
			}
		}
		found = &ExtractedAttachment{
			PartID:      id,
			Filename:    name,
			ContentType: contentType,
			Content:     content,
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if tooLarge > 0 {
		return nil, fmt.Errorf("attachment is %d bytes, over the %d byte limit", tooLarge, maxBytes)
	}
	if found == nil {
		if len(available) == 0 {
			return nil, fmt.Errorf("message has no attachments")
		}
		return nil, fmt.Errorf("no attachment matched; available parts: %s", strings.Join(available, ", "))
	}
	return found, nil
}

// HTMLToText flattens HTML into readable plain text.
func HTMLToText(h string) string {
	// Give block boundaries a chance to become line breaks before tags are
	// stripped, otherwise paragraphs run together into one wall of text.
	replacer := strings.NewReplacer(
		"</p>", "</p>\n\n",
		"</P>", "</P>\n\n",
		"<br>", "\n", "<br/>", "\n", "<br />", "\n",
		"<BR>", "\n", "<BR/>", "\n", "<BR />", "\n",
		"</div>", "</div>\n", "</DIV>", "</DIV>\n",
		"</li>", "</li>\n", "</LI>", "</LI>\n",
		"</tr>", "</tr>\n", "</TR>", "</TR>\n",
		"</h1>", "</h1>\n\n", "</h2>", "</h2>\n\n", "</h3>", "</h3>\n\n",
	)
	stripped := textPolicy.Sanitize(replacer.Replace(h))
	return collapseBlankLines(normalizeNewlines(unescapeText(stripped)))
}

// SanitizeFilename reduces an untrusted filename to a safe basename.
//
// Message filenames are attacker-controlled and reach the filesystem in
// get_attachment, so path separators, traversal segments, and control
// characters must not survive.
func SanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(name, ".")
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = name[:200-len(ext)] + ext
	}
	return name
}

// ---- internals -------------------------------------------------------------

// isAttachment decides whether a leaf part is a file rather than message body.
func isAttachment(contentType, disposition, filename string) bool {
	if strings.EqualFold(disposition, "attachment") {
		return true
	}
	if filename != "" {
		return true
	}
	lower := strings.ToLower(contentType)
	// Anything that isn't displayable text is a file by elimination.
	return !strings.HasPrefix(lower, "text/") && !strings.HasPrefix(lower, "multipart/")
}

// imapPartID converts go-message's 0-based path to IMAP's 1-based dotted form.
// A non-multipart message has a nil path and is part "1".
func imapPartID(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = strconv.Itoa(p + 1)
	}
	return strings.Join(parts, ".")
}

func readCapped(r io.Reader, limit int64) string {
	b, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return string(b)
	}
	return string(b)
}

// truncate cuts s to at most maxChars runes, reporting whether it did.
func truncate(s string, maxChars int) (string, bool) {
	if utf8.RuneCountInString(s) <= maxChars {
		return s, false
	}
	count := 0
	for i := range s {
		if count == maxChars {
			return s[:i], true
		}
		count++
	}
	return s, false
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func unescapeText(s string) string {
	return strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&#34;", `"`,
		"&nbsp;", " ", "&hellip;", "…", "&mdash;", "—", "&ndash;", "–",
	).Replace(s)
}

func headerText(h *message.Header, key string) string {
	if v, err := h.Text(key); err == nil {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(h.Get(key))
}

func addressList(h *gomail.Header, key string) []string {
	addrs, err := h.AddressList(key)
	if err != nil || len(addrs) == 0 {
		// Fall back to the raw header so a malformed address list still
		// surfaces something useful instead of vanishing.
		if raw := strings.TrimSpace(h.Get(key)); raw != "" {
			return []string{decodeWord(raw)}
		}
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, formatAddress(a))
	}
	return out
}

func formatAddress(a *mail.Address) string {
	if a == nil {
		return ""
	}
	if a.Name == "" {
		return a.Address
	}
	return fmt.Sprintf("%s <%s>", a.Name, a.Address)
}

func joinAddresses(list []string) string {
	return strings.Join(list, ", ")
}

func splitMsgIDs(v string) []string {
	fields := strings.Fields(v)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func collectHeaders(h *message.Header) map[string]string {
	out := make(map[string]string)
	fields := h.Fields()
	for fields.Next() {
		key := fields.Key()
		if _, exists := out[key]; exists {
			continue // first occurrence wins
		}
		if v, err := fields.Text(); err == nil {
			out[key] = v
		} else {
			out[key] = fields.Value()
		}
	}
	return out
}

func decodeWord(s string) string {
	if s == "" || !strings.Contains(s, "=?") {
		return s
	}
	dec := new(mime.WordDecoder)
	if v, err := dec.DecodeHeader(s); err == nil {
		return v
	}
	return s
}

func orPlaceholder(s string) string {
	if s == "" {
		return "unnamed"
	}
	return s
}
