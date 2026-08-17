package mailbox

import (
	"fmt"
	"io"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
	"github.com/kacperkwapisz/mail-mcp/internal/msgid"
)

// Session is a handle to an authenticated IMAP connection, valid only for
// the duration of a Pool.Do callback.
type Session struct {
	conn    *pooledConn
	pool    *Pool
	account *config.Account
}

// Folder describes one mailbox folder.
type Folder struct {
	Name       string   `json:"name" jsonschema:"folder name as used by other tools"`
	Delimiter  string   `json:"delimiter" jsonschema:"hierarchy separator, e.g. / or ."`
	Attributes []string `json:"attributes" jsonschema:"IMAP mailbox attributes"`
	// Role is the normalized special-use role: inbox, sent, drafts,
	// archive, trash, junk, all, or "" for an ordinary folder.
	Role string `json:"role" jsonschema:"normalized role: inbox, sent, drafts, archive, trash, junk, all, or empty"`
}

// Summary is the lightweight view of a message returned by searches.
type Summary struct {
	MessageID      string   `json:"message_id" jsonschema:"opaque handle; pass to read_email, move_email, and other per-message tools"`
	Mailbox        string   `json:"mailbox" jsonschema:"folder the message lives in"`
	From           string   `json:"from" jsonschema:"sender address"`
	To             []string `json:"to,omitempty" jsonschema:"primary recipients"`
	Subject        string   `json:"subject" jsonschema:"decoded subject line"`
	Date           string   `json:"date" jsonschema:"RFC 3339 timestamp"`
	Flags          []string `json:"flags" jsonschema:"raw IMAP flags"`
	Seen           bool     `json:"seen" jsonschema:"true when the message has been read"`
	Flagged        bool     `json:"flagged" jsonschema:"true when the message is starred/flagged"`
	Answered       bool     `json:"answered" jsonschema:"true when the message has been replied to"`
	Size           int64    `json:"size_bytes" jsonschema:"message size in bytes"`
	HasAttachments bool     `json:"has_attachments" jsonschema:"true when the message carries non-inline attachments"`
}

// Select makes mailbox current and returns its UIDVALIDITY.
//
// Re-selecting the same mailbox in the same mode is a no-op, which keeps
// multi-step operations cheap.
func (s *Session) Select(mailbox string, readOnly bool) (uint32, error) {
	if s.conn.selected == mailbox && s.conn.selectedReadOnly == readOnly && s.conn.selectedUIDValidity != 0 {
		return s.conn.selectedUIDValidity, nil
	}
	data, err := s.conn.client.Select(mailbox, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
	if err != nil {
		return 0, fmt.Errorf("select folder %q: %w", mailbox, err)
	}
	s.conn.selected = mailbox
	s.conn.selectedReadOnly = readOnly
	s.conn.selectedUIDValidity = data.UIDValidity
	return data.UIDValidity, nil
}

// SelectFor selects the mailbox referenced by id and verifies UIDVALIDITY.
//
// The verification is the whole point of the opaque message id: if the server
// renumbered the mailbox, the UID in the handle now points at a different
// message (or nothing), so acting on it would hit the wrong mail.
func (s *Session) SelectFor(id msgid.ID, readOnly bool) error {
	uidValidity, err := s.Select(id.Mailbox, readOnly)
	if err != nil {
		return err
	}
	if uidValidity != id.UIDValidity {
		return fmt.Errorf(
			"this message_id is stale: folder %q was renumbered (uidvalidity %d, handle has %d). Search again to get fresh ids",
			id.Mailbox, uidValidity, id.UIDValidity)
	}
	return nil
}

// ListMailboxes returns every folder visible to the account.
func (s *Session) ListMailboxes() ([]Folder, error) {
	entries, err := s.conn.client.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	out := make([]Folder, 0, len(entries))
	for _, e := range entries {
		attrs := make([]string, 0, len(e.Attrs))
		for _, a := range e.Attrs {
			attrs = append(attrs, string(a))
		}
		delim := ""
		if e.Delim != 0 {
			delim = string(e.Delim)
		}
		out = append(out, Folder{
			Name:       e.Mailbox,
			Delimiter:  delim,
			Attributes: attrs,
			Role:       roleFor(e),
		})
	}
	return out, nil
}

// roleFor normalizes a folder's purpose, preferring the server's SPECIAL-USE
// attributes and falling back to name matching for servers that lack them.
func roleFor(e *imap.ListData) string {
	for _, a := range e.Attrs {
		switch a {
		case imap.MailboxAttrSent:
			return "sent"
		case imap.MailboxAttrDrafts:
			return "drafts"
		case imap.MailboxAttrArchive:
			return "archive"
		case imap.MailboxAttrTrash:
			return "trash"
		case imap.MailboxAttrJunk:
			return "junk"
		case imap.MailboxAttrAll:
			return "all"
		}
	}
	if strings.EqualFold(e.Mailbox, "INBOX") {
		return "inbox"
	}
	return roleFromName(e.Mailbox)
}

// localizedFolderNames maps normalized folder names to roles for servers
// that do not advertise SPECIAL-USE. Non-English names are common enough
// that ignoring them means creating a duplicate "Sent" folder next to the
// user's real one.
var localizedFolderNames = map[string]string{
	"sent": "sent", "sent items": "sent", "sent mail": "sent", "sent messages": "sent",
	"enviado": "sent", "enviados": "sent", "elementos enviados": "sent",
	"itens enviados": "sent", "envoyés": "sent", "éléments envoyés": "sent",
	"gesendet": "sent", "gesendete elemente": "sent", "posta inviata": "sent",
	"verzonden": "sent", "wysłane": "sent", "wyslane": "sent", "skickat": "sent",

	"drafts": "drafts", "draft": "drafts", "borradores": "drafts", "brouillons": "drafts",
	"entwürfe": "drafts", "rascunhos": "drafts", "bozze": "drafts", "robocze": "drafts",
	"concepten": "drafts", "utkast": "drafts",

	"archive": "archive", "archives": "archive", "archivo": "archive",
	"archiv": "archive", "archief": "archive", "archiwum": "archive",
	"all mail": "archive",

	"trash": "trash", "deleted": "trash", "deleted items": "trash", "deleted messages": "trash",
	"papelera": "trash", "corbeille": "trash", "papierkorb": "trash",
	"cestino": "trash", "lixeira": "trash", "kosz": "trash", "prullenbak": "trash",

	"junk": "junk", "spam": "junk", "bulk mail": "junk", "correo no deseado": "junk",
	"pourriel": "junk", "indésirables": "junk", "niechciane": "junk",
}

func roleFromName(name string) string {
	lower := strings.ToLower(name)
	if role, ok := localizedFolderNames[lower]; ok {
		return role
	}
	// Handle hierarchies like "[Gmail]/Sent Mail" or "INBOX.Drafts".
	for _, sep := range []string{"/", ".", "\\"} {
		if idx := strings.LastIndex(lower, sep); idx >= 0 && idx+1 < len(lower) {
			if role, ok := localizedFolderNames[lower[idx+1:]]; ok {
				return role
			}
		}
	}
	return ""
}

// FolderForRole finds the folder serving the given role.
//
// Returns "" when the account has no such folder, letting the caller decide
// whether to create one.
func (s *Session) FolderForRole(role string) (string, error) {
	boxes, err := s.ListMailboxes()
	if err != nil {
		return "", err
	}
	for _, b := range boxes {
		if b.Role == role {
			return b.Name, nil
		}
	}
	return "", nil
}

// SearchOptions describes a message search. Empty fields are ignored.
type SearchOptions struct {
	From    string
	To      string
	Subject string
	Body    string
	Text    string
	Since   time.Time
	Before  time.Time
	Unseen  bool
	Seen    bool
	Flagged bool
}

// Search returns matching UIDs, oldest first.
func (s *Session) Search(opts SearchOptions) ([]imap.UID, error) {
	criteria := &imap.SearchCriteria{}
	addHeader := func(key, value string) {
		if value != "" {
			criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: key, Value: value})
		}
	}
	addHeader("From", opts.From)
	addHeader("To", opts.To)
	addHeader("Subject", opts.Subject)
	if opts.Body != "" {
		criteria.Body = append(criteria.Body, opts.Body)
	}
	if opts.Text != "" {
		criteria.Text = append(criteria.Text, opts.Text)
	}
	if !opts.Since.IsZero() {
		criteria.Since = opts.Since
	}
	if !opts.Before.IsZero() {
		criteria.Before = opts.Before
	}
	if opts.Unseen {
		criteria.NotFlag = append(criteria.NotFlag, imap.FlagSeen)
	}
	if opts.Seen {
		criteria.Flag = append(criteria.Flag, imap.FlagSeen)
	}
	if opts.Flagged {
		criteria.Flag = append(criteria.Flag, imap.FlagFlagged)
	}

	data, err := s.conn.client.UIDSearch(criteria, &imap.SearchOptions{ReturnAll: true}).Wait()
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return data.AllUIDs(), nil
}

// FetchSummaries returns metadata for the given UIDs in the selected mailbox.
func (s *Session) FetchSummaries(account, mailbox string, uidValidity uint32, uids []imap.UID) ([]Summary, error) {
	if len(uids) == 0 {
		return []Summary{}, nil
	}
	messages, err := s.conn.client.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		Flags:         true,
		RFC822Size:    true,
		BodyStructure: &imap.FetchItemBodyStructure{},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch message summaries: %w", err)
	}

	out := make([]Summary, 0, len(messages))
	for _, m := range messages {
		out = append(out, summarize(m, account, mailbox, uidValidity))
	}
	return out, nil
}

func summarize(m *imapclient.FetchMessageBuffer, account, mailbox string, uidValidity uint32) Summary {
	sum := Summary{
		MessageID: msgid.Encode(account, mailbox, uidValidity, uint32(m.UID)),
		Mailbox:   mailbox,
		Size:      m.RFC822Size,
		Flags:     make([]string, 0, len(m.Flags)),
	}
	for _, f := range m.Flags {
		sum.Flags = append(sum.Flags, string(f))
		switch f {
		case imap.FlagSeen:
			sum.Seen = true
		case imap.FlagFlagged:
			sum.Flagged = true
		case imap.FlagAnswered:
			sum.Answered = true
		}
	}
	if env := m.Envelope; env != nil {
		sum.Subject = env.Subject
		sum.From = formatIMAPAddresses(env.From)
		sum.To = splitIMAPAddresses(env.To)
		if !env.Date.IsZero() {
			sum.Date = env.Date.Format(time.RFC3339)
		}
	}
	if m.BodyStructure != nil {
		sum.HasAttachments = hasAttachments(m.BodyStructure)
	}
	return sum
}

// hasAttachments reports whether the structure contains a part a human would
// call a file, ignoring the alternative text/html bodies every message has.
func hasAttachments(bs imap.BodyStructure) bool {
	found := false
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		if found {
			return false
		}
		single, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		if disp := single.Disposition(); disp != nil && strings.EqualFold(disp.Value, "attachment") {
			found = true
			return false
		}
		if single.Filename() != "" {
			found = true
			return false
		}
		return true
	})
	return found
}

func formatIMAPAddresses(addrs []imap.Address) string {
	return strings.Join(splitIMAPAddresses(addrs), ", ")
}

func splitIMAPAddresses(addrs []imap.Address) []string {
	out := make([]string, 0, len(addrs))
	for i := range addrs {
		a := &addrs[i]
		if a.IsGroupStart() || a.IsGroupEnd() {
			continue
		}
		addr := a.Addr()
		if a.Name != "" {
			addr = fmt.Sprintf("%s <%s>", a.Name, addr)
		}
		out = append(out, addr)
	}
	return out
}

// FetchRaw returns the complete RFC 822 bytes of one message.
//
// peek avoids setting \Seen, so reading a message through an agent does not
// silently mark it read in the user's own client.
func (s *Session) FetchRaw(uid imap.UID, peek bool) ([]byte, error) {
	section := &imap.FetchItemBodySection{Peek: peek}
	messages, err := s.conn.client.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch message body: %w", err)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("message not found (uid %d)", uid)
	}
	body := messages[0].FindBodySection(section)
	if body == nil {
		return nil, fmt.Errorf("server returned no body for uid %d", uid)
	}
	return body, nil
}

// StoreFlags adds or removes flags on one message.
func (s *Session) StoreFlags(uid imap.UID, flags []imap.Flag, add bool) error {
	op := imap.StoreFlagsDel
	if add {
		op = imap.StoreFlagsAdd
	}
	cmd := s.conn.client.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
		Op:     op,
		Silent: true,
		Flags:  flags,
	}, nil)
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("update flags: %w", err)
	}
	return nil
}

// Move relocates a message, using the MOVE extension when available and
// falling back to COPY + \Deleted + EXPUNGE otherwise.
//
// Returns the new UID when the server reports one (via COPYUID), else 0.
func (s *Session) Move(uid imap.UID, dest string) (imap.UID, error) {
	if s.conn.client.Caps().Has(imap.CapMove) {
		data, err := s.conn.client.Move(imap.UIDSetNum(uid), dest).Wait()
		if err != nil {
			return 0, fmt.Errorf("move to %q: %w", dest, err)
		}
		if data != nil {
			if set, ok := data.DestUIDs.(imap.UIDSet); ok {
				if uids, ok := set.Nums(); ok && len(uids) > 0 {
					return uids[0], nil
				}
			}
		}
		return 0, nil
	}

	copyData, err := s.conn.client.Copy(imap.UIDSetNum(uid), dest).Wait()
	if err != nil {
		return 0, fmt.Errorf("copy to %q: %w", dest, err)
	}
	if err := s.StoreFlags(uid, []imap.Flag{imap.FlagDeleted}, true); err != nil {
		return 0, err
	}
	// UID EXPUNGE removes only this message; a bare EXPUNGE would also purge
	// anything else the user had already flagged \Deleted.
	if s.conn.client.Caps().Has(imap.CapUIDPlus) {
		if err := s.conn.client.UIDExpunge(imap.UIDSetNum(uid)).Close(); err != nil {
			return 0, fmt.Errorf("expunge after copy: %w", err)
		}
	} else if err := s.conn.client.Expunge().Close(); err != nil {
		return 0, fmt.Errorf("expunge after copy: %w", err)
	}

	if copyData != nil && copyData.DestUIDs != nil {
		if uids, ok := copyData.DestUIDs.Nums(); ok && len(uids) > 0 {
			return uids[0], nil
		}
	}
	return 0, nil
}

// Append writes a raw message into a folder, creating the folder if needed.
func (s *Session) Append(mailbox string, raw []byte, flags []imap.Flag) error {
	if err := s.EnsureMailbox(mailbox); err != nil {
		return err
	}
	cmd := s.conn.client.Append(mailbox, int64(len(raw)), &imap.AppendOptions{
		Flags: flags,
		Time:  time.Now(),
	})
	if _, err := io.Copy(cmd, strings.NewReader(string(raw))); err != nil {
		_ = cmd.Close()
		return fmt.Errorf("write message to %q: %w", mailbox, err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("append to %q: %w", mailbox, err)
	}
	if _, err := cmd.Wait(); err != nil {
		return fmt.Errorf("append to %q: %w", mailbox, err)
	}
	// The append invalidated our cached view of the selected mailbox.
	if s.conn.selected == mailbox {
		s.conn.selected = ""
		s.conn.selectedUIDValidity = 0
	}
	return nil
}

// EnsureMailbox creates a folder when it does not already exist.
func (s *Session) EnsureMailbox(name string) error {
	boxes, err := s.ListMailboxes()
	if err != nil {
		return err
	}
	for _, b := range boxes {
		if b.Name == name {
			return nil
		}
	}
	return s.CreateMailbox(name)
}

// CreateMailbox creates a folder.
func (s *Session) CreateMailbox(name string) error {
	if err := s.conn.client.Create(name, nil).Wait(); err != nil {
		return fmt.Errorf("create folder %q: %w", name, err)
	}
	return nil
}

// RenameMailbox renames a folder.
func (s *Session) RenameMailbox(from, to string) error {
	if err := s.conn.client.Rename(from, to, nil).Wait(); err != nil {
		return fmt.Errorf("rename folder %q to %q: %w", from, to, err)
	}
	if s.conn.selected == from {
		s.conn.selected = ""
		s.conn.selectedUIDValidity = 0
	}
	return nil
}

// DeleteMailbox removes a folder.
func (s *Session) DeleteMailbox(name string) error {
	if s.conn.selected == name {
		if err := s.conn.client.Unselect().Wait(); err != nil {
			return fmt.Errorf("unselect %q before delete: %w", name, err)
		}
		s.conn.selected = ""
		s.conn.selectedUIDValidity = 0
	}
	if err := s.conn.client.Delete(name).Wait(); err != nil {
		return fmt.Errorf("delete folder %q: %w", name, err)
	}
	return nil
}

// MessageCount reports EXISTS for the selected mailbox.
func (s *Session) MessageCount() uint32 {
	if mbox := s.conn.client.Mailbox(); mbox != nil {
		return mbox.NumMessages
	}
	return 0
}
