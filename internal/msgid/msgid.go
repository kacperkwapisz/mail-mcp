// Package msgid encodes a stable, opaque handle for an IMAP message.
//
// A bare UID is not a safe identifier for three reasons: it is only
// meaningful within one mailbox, only while UIDVALIDITY is unchanged, and
// only for one account. If a server renumbers a mailbox, a stored UID
// silently points at a different message — so an agent acting on a cached
// "uid 42" could archive or delete the wrong mail.
//
// A message ID therefore carries all four parts. Every operation re-checks
// UIDVALIDITY after selecting the mailbox, and because the account is baked
// in, an agent cannot pair a handle from one mailbox with another account.
package msgid

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// ID identifies exactly one message.
type ID struct {
	Account     string
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// Encode renders the ID as an opaque token.
//
// Account and mailbox are base64url-encoded so names containing dots,
// colons, slashes, or non-ASCII characters need no escaping.
func Encode(account, mailbox string, uidValidity, uid uint32) string {
	return fmt.Sprintf("%s.%s.%d.%d",
		base64.RawURLEncoding.EncodeToString([]byte(account)),
		base64.RawURLEncoding.EncodeToString([]byte(mailbox)),
		uidValidity,
		uid,
	)
}

// String implements fmt.Stringer.
func (id ID) String() string { return Encode(id.Account, id.Mailbox, id.UIDValidity, id.UID) }

// Parse decodes a token produced by Encode.
func Parse(s string) (ID, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 4 {
		return ID{}, malformed(s)
	}

	account, err := decodeSegment(parts[0])
	if err != nil {
		return ID{}, malformed(s)
	}
	mailbox, err := decodeSegment(parts[1])
	if err != nil {
		return ID{}, malformed(s)
	}
	uidValidity, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return ID{}, malformed(s)
	}
	uid, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil || uid == 0 {
		return ID{}, malformed(s)
	}

	return ID{
		Account:     account,
		Mailbox:     mailbox,
		UIDValidity: uint32(uidValidity),
		UID:         uint32(uid),
	}, nil
}

func decodeSegment(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", fmt.Errorf("empty segment")
	}
	return string(b), nil
}

func malformed(s string) error {
	return fmt.Errorf("malformed message_id %q: pass a value returned by search_emails, unmodified", s)
}
