package msgid

import "testing"

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name        string
		account     string
		mailbox     string
		uidValidity uint32
		uid         uint32
	}{
		{"simple", "icloud", "INBOX", 1, 42},
		{"gmail special folder", "work", "[Gmail]/All Mail", 123456789, 1},
		{"dots in name", "a.b", "INBOX.Archive.2024", 7, 99},
		{"colon in name", "acct", "Projects:Active", 7, 99},
		{"non-ascii", "konto", "Wysłane", 42, 4294967295},
		{"slash separator", "acct", "INBOX/Sub/Deep", 3, 7},
		{"email as id", "me@example.com", "INBOX", 2, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(Encode(tc.account, tc.mailbox, tc.uidValidity, tc.uid))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got.Account != tc.account {
				t.Errorf("account = %q, want %q", got.Account, tc.account)
			}
			if got.Mailbox != tc.mailbox {
				t.Errorf("mailbox = %q, want %q", got.Mailbox, tc.mailbox)
			}
			if got.UIDValidity != tc.uidValidity {
				t.Errorf("uidvalidity = %d, want %d", got.UIDValidity, tc.uidValidity)
			}
			if got.UID != tc.uid {
				t.Errorf("uid = %d, want %d", got.UID, tc.uid)
			}
		})
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"",
		"INBOX",
		"SU5CT1g.1",
		"YQ.SU5CT1g.notanumber.1",
		"YQ.SU5CT1g.1.notanumber",
		"YQ.SU5CT1g.1.0",   // uid 0 is never valid
		"YQ..1.1",          // empty mailbox
		".SU5CT1g.1.1",     // empty account
		"YQ.!!!bad!!!.1.1", // not base64
		"YQ.SU5CT1g.-1.1",
		"YQ.SU5CT1g.1.99999999999999999999", // overflows uint32
		"YQ.SU5CT1g.1.1.extra",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", s)
		}
	}
}

func TestEncodeIsStable(t *testing.T) {
	// Agents persist these handles across turns; changing the encoding would
	// silently invalidate every cached id.
	if got, want := Encode("acct", "INBOX", 1, 42), "YWNjdA.SU5CT1g.1.42"; got != want {
		t.Errorf("Encode = %q, want %q", got, want)
	}
}

func TestHandleCannotBeReusedAcrossAccounts(t *testing.T) {
	// The account is part of the token precisely so an agent cannot take a
	// handle from one mailbox and apply it to another, where the same UID
	// would be valid but point at a different message.
	a := Encode("personal", "INBOX", 1, 42)
	b := Encode("work", "INBOX", 1, 42)
	if a == b {
		t.Fatal("handles from different accounts collided")
	}
	parsed, err := Parse(a)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Account != "personal" {
		t.Errorf("account = %q, want personal", parsed.Account)
	}
}

func TestStringMatchesEncode(t *testing.T) {
	id := ID{Account: "a", Mailbox: "INBOX", UIDValidity: 9, UID: 3}
	if id.String() != Encode(id.Account, id.Mailbox, id.UIDValidity, id.UID) {
		t.Error("String() disagrees with Encode()")
	}
}
