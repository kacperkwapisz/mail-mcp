package tools

import (
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/mailmime"
	"github.com/kacperkwapisz/mail-mcp/internal/msgid"
)

func uids(n ...int) []imap.UID {
	out := make([]imap.UID, len(n))
	for i, v := range n {
		out[i] = imap.UID(v)
	}
	return out
}

func TestPaginateNewestFirst(t *testing.T) {
	// IMAP hands back UIDs oldest-first; agents want newest-first.
	all := uids(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)

	cases := []struct {
		name   string
		offset int
		limit  int
		want   []imap.UID
	}{
		{"first page", 0, 3, uids(10, 9, 8)},
		{"second page", 3, 3, uids(7, 6, 5)},
		{"last partial page", 9, 3, uids(1)},
		{"limit exceeds remainder", 0, 100, uids(10, 9, 8, 7, 6, 5, 4, 3, 2, 1)},
		{"offset past the end", 20, 3, nil},
		{"offset exactly at the end", 10, 3, nil},
		{"single", 0, 1, uids(10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := paginateNewestFirst(all, tc.offset, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPaginateEmptyResult(t *testing.T) {
	if got := paginateNewestFirst(nil, 0, 10); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestPagesDoNotOverlapOrSkip(t *testing.T) {
	all := uids(1, 2, 3, 4, 5, 6, 7)
	seen := map[imap.UID]int{}
	for offset := 0; offset < len(all); offset += 3 {
		for _, uid := range paginateNewestFirst(all, offset, 3) {
			seen[uid]++
		}
	}
	for _, uid := range all {
		if seen[uid] != 1 {
			t.Errorf("uid %d appeared %d times across pages, want exactly 1", uid, seen[uid])
		}
	}
}

func TestSortByUIDDesc(t *testing.T) {
	// Servers may return FETCH results in any order; the response must still
	// come back newest-first.
	order := uids(30, 20, 10)
	summaries := []mailbox.Summary{
		{MessageID: msgid.Encode("a", "INBOX", 1, 10)},
		{MessageID: msgid.Encode("a", "INBOX", 1, 30)},
		{MessageID: msgid.Encode("a", "INBOX", 1, 20)},
	}
	sortByUIDDesc(summaries, order)

	want := []string{
		msgid.Encode("a", "INBOX", 1, 30),
		msgid.Encode("a", "INBOX", 1, 20),
		msgid.Encode("a", "INBOX", 1, 10),
	}
	for i, w := range want {
		if summaries[i].MessageID != w {
			t.Errorf("[%d] = %q, want %q", i, summaries[i].MessageID, w)
		}
	}
}

func TestParseDate(t *testing.T) {
	if got, err := parseDate("", "since"); err != nil || !got.IsZero() {
		t.Errorf("empty should yield a zero time, got %v %v", got, err)
	}
	got, err := parseDate("2024-01-31", "since")
	if err != nil {
		t.Fatalf("parseDate: %v", err)
	}
	if got.Year() != 2024 || got.Month() != 1 || got.Day() != 31 {
		t.Errorf("parsed %v", got)
	}
	for _, bad := range []string{"31-01-2024", "yesterday", "2024/01/31", "2024-13-01"} {
		if _, err := parseDate(bad, "since"); err == nil {
			t.Errorf("parseDate(%q) succeeded", bad)
		}
	}
}

func TestExtractEmail(t *testing.T) {
	cases := map[string]string{
		"plain@example.com":              "plain@example.com",
		"Name <named@example.com>":       "named@example.com",
		`"Doe, John" <john@example.com>`: "john@example.com",
		"  spaced@example.com  ":         "spaced@example.com",
		"Weird <a@b.com> trailing":       "a@b.com",
	}
	for in, want := range cases {
		if got := extractEmail(in); got != want {
			t.Errorf("extractEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSelf(t *testing.T) {
	acc := &config.Account{
		FromAddress: "me@custom.com",
		SMTP:        config.Endpoint{Username: "login@icloud.com"},
		IMAP:        config.Endpoint{Username: "login@icloud.com"},
	}
	// A reply-all must not copy the user on their own message, including via
	// whichever identity they happen to log in with.
	for _, addr := range []string{
		"me@custom.com",
		"Me <me@custom.com>",
		"ME@CUSTOM.COM",
		"login@icloud.com",
		"Kacper <login@icloud.com>",
	} {
		if !isSelf(addr, acc) {
			t.Errorf("isSelf(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"someone@else.com", "", "notme@custom.com.evil.com"} {
		if isSelf(addr, acc) {
			t.Errorf("isSelf(%q) = true, want false", addr)
		}
	}
}

func TestDedupeAddresses(t *testing.T) {
	got := dedupeAddresses([]string{
		"a@example.com",
		"A@EXAMPLE.COM",
		"Name <a@example.com>",
		"b@example.com",
		"",
	})
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 unique addresses", got)
	}
	if got[0] != "a@example.com" || got[1] != "b@example.com" {
		t.Errorf("got %v", got)
	}
}

func TestContainsAddress(t *testing.T) {
	list := []string{"Name <a@example.com>", "b@example.com"}
	for _, needle := range []string{"a@example.com", "A@Example.com", "Other <b@example.com>"} {
		if !containsAddress(list, needle) {
			t.Errorf("containsAddress(%q) = false", needle)
		}
	}
	if containsAddress(list, "c@example.com") {
		t.Error("containsAddress matched an absent address")
	}
}

func TestHasPrefixFold(t *testing.T) {
	cases := map[string]bool{
		"Re: hello":  true,
		"RE: hello":  true,
		"re: hello":  true,
		"Reply":      false,
		"":           false,
		"R":          false,
		"Fwd: hello": false,
	}
	for in, want := range cases {
		if got := hasPrefixFold(in, "re:"); got != want {
			t.Errorf("hasPrefixFold(%q, \"re:\") = %v, want %v", in, got, want)
		}
	}
}

func TestBuildReplyDerivesEverythingFromTheOriginal(t *testing.T) {
	acc := &config.Account{
		ID: "a", FromAddress: "me@example.com",
		SMTP: config.Endpoint{Username: "me@example.com"},
	}
	original := &mailmime.Message{
		From:       "Sender <sender@example.com>",
		To:         []string{"me@example.com", "colleague@example.com"},
		Cc:         []string{"boss@example.com"},
		Subject:    "Quarterly report",
		MessageID:  "<original@example.com>",
		References: []string{"<root@example.com>"},
		BodyText:   "Original body",
		Date:       "2024-01-31T10:00:00Z",
	}

	t.Run("reply to sender only", func(t *testing.T) {
		no := false
		comp, err := buildReply(acc, original, nil, &replyInput{BodyText: "My reply", ReplyAll: &no})
		if err != nil {
			t.Fatalf("buildReply: %v", err)
		}
		if len(comp.To) != 1 || comp.To[0] != original.From {
			t.Errorf("To = %v, want just the sender", comp.To)
		}
		if len(comp.Cc) != 0 {
			t.Errorf("Cc = %v, want empty", comp.Cc)
		}
		if comp.Subject != "Re: Quarterly report" {
			t.Errorf("Subject = %q", comp.Subject)
		}
		if comp.InReplyTo != "<original@example.com>" {
			t.Errorf("InReplyTo = %q", comp.InReplyTo)
		}
		// The chain must be the original's references plus the original.
		if len(comp.References) != 2 || comp.References[1] != "<original@example.com>" {
			t.Errorf("References = %v", comp.References)
		}
	})

	t.Run("reply all excludes self", func(t *testing.T) {
		yes := true
		comp, err := buildReply(acc, original, nil, &replyInput{BodyText: "My reply", ReplyAll: &yes})
		if err != nil {
			t.Fatalf("buildReply: %v", err)
		}
		for _, addr := range append(comp.To, comp.Cc...) {
			if isSelf(addr, acc) {
				t.Errorf("reply-all copied the sender's own address: %v", comp.Cc)
			}
		}
		if !containsAddress(comp.Cc, "colleague@example.com") {
			t.Errorf("Cc = %v, want the other original recipient", comp.Cc)
		}
		if !containsAddress(comp.Cc, "boss@example.com") {
			t.Errorf("Cc = %v, want the original Cc", comp.Cc)
		}
	})

	t.Run("does not double the Re prefix", func(t *testing.T) {
		already := *original
		already.Subject = "Re: Quarterly report"
		comp, err := buildReply(acc, &already, nil, &replyInput{BodyText: "reply"})
		if err != nil {
			t.Fatalf("buildReply: %v", err)
		}
		if comp.Subject != "Re: Quarterly report" {
			t.Errorf("Subject = %q, want no second Re:", comp.Subject)
		}
	})

	t.Run("honours Reply-To over From", func(t *testing.T) {
		withReplyTo := *original
		withReplyTo.ReplyTo = []string{"replies@example.com"}
		comp, err := buildReply(acc, &withReplyTo, nil, &replyInput{BodyText: "reply"})
		if err != nil {
			t.Fatalf("buildReply: %v", err)
		}
		if !containsAddress(comp.To, "replies@example.com") {
			t.Errorf("To = %v, want the Reply-To address", comp.To)
		}
	})

	t.Run("quotes by default and can be turned off", func(t *testing.T) {
		quoted, err := buildReply(acc, original, nil, &replyInput{BodyText: "My reply"})
		if err != nil {
			t.Fatalf("buildReply: %v", err)
		}
		if !contains([]string{quoted.BodyText}, quoted.BodyText) || len(quoted.BodyText) <= len("My reply") {
			t.Errorf("expected the original to be quoted, got %q", quoted.BodyText)
		}

		no := false
		bare, err := buildReply(acc, original, nil, &replyInput{BodyText: "My reply", QuoteOriginal: &no})
		if err != nil {
			t.Fatalf("buildReply: %v", err)
		}
		if bare.BodyText != "My reply" {
			t.Errorf("BodyText = %q, want no quote block", bare.BodyText)
		}
	})
}

func TestBuildReplyFailsWithoutASender(t *testing.T) {
	acc := &config.Account{ID: "a", FromAddress: "me@example.com"}
	_, err := buildReply(acc, &mailmime.Message{Subject: "No sender"}, nil, &replyInput{BodyText: "hi"})
	if err == nil {
		t.Fatal("buildReply succeeded with no address to reply to")
	}
}

func TestClamp(t *testing.T) {
	cases := []struct{ v, min, max, want int }{
		{5, 1, 10, 5},
		{0, 1, 10, 1},
		{100, 1, 10, 10},
		{1, 1, 10, 1},
		{10, 1, 10, 10},
	}
	for _, c := range cases {
		if got := clamp(c.v, c.min, c.max); got != c.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d", c.v, c.min, c.max, got, c.want)
		}
	}
}

func TestBoolOr(t *testing.T) {
	yes, no := true, false
	if !boolOr(nil, true) {
		t.Error("nil should yield the default")
	}
	if boolOr(nil, false) {
		t.Error("nil should yield the default")
	}
	if !boolOr(&yes, false) {
		t.Error("explicit true should win")
	}
	if boolOr(&no, true) {
		t.Error("explicit false should win over a true default")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "value", "other"); got != "value" {
		t.Errorf("got %q", got)
	}
	if got := firstNonEmpty("", "   "); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestPluralize(t *testing.T) {
	if got := pluralize(1, "folder", "folders"); got != "1 folder" {
		t.Errorf("got %q", got)
	}
	if got := pluralize(0, "folder", "folders"); got != "0 folders" {
		t.Errorf("got %q", got)
	}
	if got := pluralize(3, "folder", "folders"); got != "3 folders" {
		t.Errorf("got %q", got)
	}
}
