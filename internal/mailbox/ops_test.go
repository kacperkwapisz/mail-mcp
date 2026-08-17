package mailbox

import (
	"testing"

	imap "github.com/emersion/go-imap/v2"
)

func TestRoleForUsesSpecialUseAttributes(t *testing.T) {
	cases := []struct {
		attr imap.MailboxAttr
		want string
	}{
		{imap.MailboxAttrSent, "sent"},
		{imap.MailboxAttrDrafts, "drafts"},
		{imap.MailboxAttrArchive, "archive"},
		{imap.MailboxAttrTrash, "trash"},
		{imap.MailboxAttrJunk, "junk"},
		{imap.MailboxAttrAll, "all"},
	}
	for _, tc := range cases {
		// The name is deliberately meaningless: the server's declared
		// attribute must win over any name-based guess.
		got := roleFor(&imap.ListData{Mailbox: "Zzz", Attrs: []imap.MailboxAttr{tc.attr}})
		if got != tc.want {
			t.Errorf("roleFor(%s) = %q, want %q", tc.attr, got, tc.want)
		}
	}
}

func TestRoleForFallsBackToNames(t *testing.T) {
	// Servers without SPECIAL-USE are common, and their folder names are
	// often not English. Guessing wrong means creating a duplicate folder
	// next to the user's real one.
	cases := map[string]string{
		"INBOX":              "inbox",
		"inbox":              "inbox",
		"Sent":               "sent",
		"Sent Items":         "sent",
		"[Gmail]/Sent Mail":  "sent",
		"INBOX.Sent":         "sent",
		"Wysłane":            "sent",
		"Gesendete Elemente": "sent",
		"Éléments envoyés":   "sent",
		"Drafts":             "drafts",
		"Robocze":            "drafts",
		"Entwürfe":           "drafts",
		"Archive":            "archive",
		"Archiwum":           "archive",
		"Trash":              "trash",
		"Kosz":               "trash",
		"Papierkorb":         "trash",
		"Deleted Items":      "trash",
		"Junk":               "junk",
		"Spam":               "junk",
		"Projects":           "",
		"2024 Invoices":      "",
	}
	for name, want := range cases {
		if got := roleFor(&imap.ListData{Mailbox: name}); got != want {
			t.Errorf("roleFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestRoleForIgnoresNonRoleAttributes(t *testing.T) {
	got := roleFor(&imap.ListData{
		Mailbox: "Projects",
		Attrs:   []imap.MailboxAttr{imap.MailboxAttrHasChildren, imap.MailboxAttrSubscribed},
	})
	if got != "" {
		t.Errorf("roleFor = %q, want empty", got)
	}
}

func TestHasAttachments(t *testing.T) {
	textPart := &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain"}
	htmlPart := &imap.BodyStructureSinglePart{Type: "text", Subtype: "html"}
	filePart := &imap.BodyStructureSinglePart{
		Type: "application", Subtype: "pdf",
		Extended: &imap.BodyStructureSinglePartExt{
			Disposition: &imap.BodyStructureDisposition{
				Value:  "attachment",
				Params: map[string]string{"filename": "invoice.pdf"},
			},
		},
	}

	t.Run("plain message has none", func(t *testing.T) {
		if hasAttachments(textPart) {
			t.Error("a text/plain message reported attachments")
		}
	})

	t.Run("alternative bodies are not attachments", func(t *testing.T) {
		// Every formatted email has both a text and an HTML part; counting
		// those as attachments would make the flag useless.
		alt := &imap.BodyStructureMultiPart{
			Subtype:  "alternative",
			Children: []imap.BodyStructure{textPart, htmlPart},
		}
		if hasAttachments(alt) {
			t.Error("multipart/alternative reported attachments")
		}
	})

	t.Run("mixed with a file has one", func(t *testing.T) {
		mixed := &imap.BodyStructureMultiPart{
			Subtype: "mixed",
			Children: []imap.BodyStructure{
				&imap.BodyStructureMultiPart{
					Subtype:  "alternative",
					Children: []imap.BodyStructure{textPart, htmlPart},
				},
				filePart,
			},
		}
		if !hasAttachments(mixed) {
			t.Error("multipart/mixed with a PDF reported no attachments")
		}
	})
}

func TestSplitIMAPAddresses(t *testing.T) {
	addrs := []imap.Address{
		{Name: "Jan Kowalski", Mailbox: "jan", Host: "example.com"},
		{Mailbox: "plain", Host: "example.com"},
	}
	got := splitIMAPAddresses(addrs)
	want := []string{"Jan Kowalski <jan@example.com>", "plain@example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsConnectionError(t *testing.T) {
	// Only socket-level failures should recycle the pooled connection; a
	// server rejecting one command must not tear down the session.
	transport := []string{
		"use of closed network connection",
		"read: connection reset by peer",
		"write: broken pipe",
		"unexpected EOF",
		"dial tcp: i/o timeout",
	}
	for _, msg := range transport {
		if !isConnectionError(errString(msg)) {
			t.Errorf("isConnectionError(%q) = false, want true", msg)
		}
	}

	protocol := []string{
		"NO [AUTHENTICATIONFAILED] Invalid credentials",
		"BAD Command Argument Error",
		"NO Mailbox doesn't exist",
	}
	for _, msg := range protocol {
		if isConnectionError(errString(msg)) {
			t.Errorf("isConnectionError(%q) = true, want false", msg)
		}
	}

	if isConnectionError(nil) {
		t.Error("isConnectionError(nil) = true")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
