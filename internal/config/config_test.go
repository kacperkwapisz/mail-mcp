package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func loadOK(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

const minimal = `
accounts:
  - id: personal
    imap:
      host: imap.example.com
      username: me@example.com
      password: pw
`

func TestLoadAppliesDefaults(t *testing.T) {
	cfg := loadOK(t, minimal)
	acc := cfg.Accounts[0]

	if acc.IMAP.Port != 993 {
		t.Errorf("imap port = %d, want 993", acc.IMAP.Port)
	}
	if acc.IMAP.Security != SecurityTLS {
		t.Errorf("imap security = %q, want tls", acc.IMAP.Security)
	}
	if acc.SMTP.Port != 587 {
		t.Errorf("smtp port = %d, want 587", acc.SMTP.Port)
	}
	if acc.SMTP.Security != SecuritySTARTTLS {
		t.Errorf("smtp security = %q, want starttls", acc.SMTP.Security)
	}
	// SMTP inherits from IMAP when omitted — the common single-login case.
	if acc.SMTP.Host != "imap.example.com" || acc.SMTP.Username != "me@example.com" || acc.SMTP.Password != "pw" {
		t.Errorf("smtp did not inherit imap credentials: %+v", acc.SMTP)
	}
	if acc.FromAddress != "me@example.com" {
		t.Errorf("from_address = %q, want the smtp username", acc.FromAddress)
	}

	if cfg.Limits.MaxBodyChars != DefaultMaxBodyChars {
		t.Errorf("max_body_chars = %d", cfg.Limits.MaxBodyChars)
	}
	if cfg.Timeouts.SMTPSend != DefaultSMTPSend {
		t.Errorf("smtp send timeout = %s", cfg.Timeouts.SMTPSend)
	}
	if cfg.Limits.AttachmentDir == "" {
		t.Error("attachment dir should default to the temp directory")
	}
}

func TestGatesDefaultToClosed(t *testing.T) {
	cfg := loadOK(t, minimal)
	// A fresh install must not let an agent send or delete mail until the
	// operator opts in.
	if cfg.SendAllowed(cfg.Accounts[0]) {
		t.Error("sending should be disabled by default")
	}
	if cfg.DeleteAllowed(cfg.Accounts[0]) {
		t.Error("deleting should be disabled by default")
	}
}

func TestPerAccountGatesOverrideGlobal(t *testing.T) {
	cfg := loadOK(t, `
allow_send: true
allow_delete: true
accounts:
  - id: open
    imap: {host: h, username: u@e.com, password: p}
  - id: locked
    allow_send: false
    allow_delete: false
    imap: {host: h, username: u@e.com, password: p}
`)
	open, _ := cfg.Resolve("open")
	locked, _ := cfg.Resolve("locked")

	if !cfg.SendAllowed(open) || !cfg.DeleteAllowed(open) {
		t.Error("global true should apply to an account with no override")
	}
	if cfg.SendAllowed(locked) || cfg.DeleteAllowed(locked) {
		t.Error("per-account false should override global true")
	}
}

func TestExplicitSecurityOverridesPortInference(t *testing.T) {
	cfg := loadOK(t, `
accounts:
  - id: a
    imap: {host: h, port: 993, security: starttls, username: u@e.com, password: p}
    smtp: {host: h, port: 465, security: starttls, username: u@e.com, password: p}
`)
	acc := cfg.Accounts[0]
	if acc.IMAP.Security != SecuritySTARTTLS {
		t.Errorf("explicit imap security ignored: %q", acc.IMAP.Security)
	}
	if acc.SMTP.Security != SecuritySTARTTLS {
		t.Errorf("explicit smtp security ignored: %q", acc.SMTP.Security)
	}
}

func TestPortInfersSecurity(t *testing.T) {
	cfg := loadOK(t, `
accounts:
  - id: a
    imap: {host: h, port: 143, username: u@e.com, password: p}
    smtp: {host: h, port: 465, username: u@e.com, password: p}
`)
	acc := cfg.Accounts[0]
	if acc.IMAP.Security != SecuritySTARTTLS {
		t.Errorf("port 143 should imply starttls, got %q", acc.IMAP.Security)
	}
	if acc.SMTP.Security != SecurityTLS {
		t.Errorf("port 465 should imply tls, got %q", acc.SMTP.Security)
	}
}

func TestLegacyFlatKeysStillLoad(t *testing.T) {
	// Existing poke-mail configs must keep working across the rename.
	cfg := loadOK(t, `
accounts:
  - id: icloud
    imap_host: imap.mail.me.com
    imap_username: me@icloud.com
    imap_password: app-specific
    smtp_host: smtp.mail.me.com
    smtp_port: 587
    smtp_username: me@icloud.com
    smtp_password: app-specific
    from_address: me@customdomain.com
`)
	acc := cfg.Accounts[0]
	if acc.IMAP.Host != "imap.mail.me.com" || acc.IMAP.Username != "me@icloud.com" {
		t.Errorf("legacy imap keys not folded in: %+v", acc.IMAP)
	}
	if acc.SMTP.Host != "smtp.mail.me.com" || acc.SMTP.Port != 587 {
		t.Errorf("legacy smtp keys not folded in: %+v", acc.SMTP)
	}
	if acc.FromAddress != "me@customdomain.com" {
		t.Errorf("from_address = %q", acc.FromAddress)
	}
}

func TestNestedKeysWinOverLegacy(t *testing.T) {
	cfg := loadOK(t, `
accounts:
  - id: a
    imap_host: legacy.example.com
    imap_username: legacy@example.com
    imap_password: legacy
    imap:
      host: nested.example.com
      username: nested@example.com
      password: nested
`)
	if got := cfg.Accounts[0].IMAP.Host; got != "nested.example.com" {
		t.Errorf("imap host = %q, want the nested value", got)
	}
}

func TestCustomTimeouts(t *testing.T) {
	cfg := loadOK(t, `
timeouts:
  imap_connect: 5s
  imap_command: 90s
  smtp_connect: 15s
  smtp_send: 10m
accounts:
  - id: a
    imap: {host: h, username: u@e.com, password: p}
`)
	want := map[string]struct{ got, want time.Duration }{
		"imap_connect": {cfg.Timeouts.IMAPConnect, 5 * time.Second},
		"imap_command": {cfg.Timeouts.IMAPCommand, 90 * time.Second},
		"smtp_connect": {cfg.Timeouts.SMTPConnect, 15 * time.Second},
		"smtp_send":    {cfg.Timeouts.SMTPSend, 10 * time.Minute},
	}
	for name, c := range want {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", name, c.got, c.want)
		}
	}
}

func TestLoadRejectsInvalidConfigs(t *testing.T) {
	cases := map[string]string{
		"no accounts":       `allow_send: true`,
		"missing imap host": "accounts:\n  - id: a\n    imap: {username: u@e.com, password: p}",
		"missing username":  "accounts:\n  - id: a\n    imap: {host: h, password: p}",
		"missing password":  "accounts:\n  - id: a\n    imap: {host: h, username: u}",
		"duplicate ids":     "accounts:\n  - id: a\n    imap: {host: h, username: u@e.com, password: p}\n  - id: A\n    imap: {host: h, username: u@e.com, password: p}",
		"bad port":          "accounts:\n  - id: a\n    imap: {host: h, port: 99999, username: u@e.com, password: p}",
		"bad security":      "accounts:\n  - id: a\n    imap: {host: h, security: carrier-pigeon, username: u@e.com, password: p}",
		"bad timeout":       "timeouts: {imap_connect: soon}\naccounts:\n  - id: a\n    imap: {host: h, username: u@e.com, password: p}",
		"negative timeout":  "timeouts: {imap_connect: -5s}\naccounts:\n  - id: a\n    imap: {host: h, username: u@e.com, password: p}",
		"from not an email": "accounts:\n  - id: a\n    from_address: notanemail\n    imap: {host: h, username: u@e.com, password: p}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Errorf("Load accepted an invalid config (%s)", name)
			}
		})
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Fatal("Load succeeded for a missing file")
	}
}

func TestResolve(t *testing.T) {
	cfg := loadOK(t, `
accounts:
  - id: personal
    from_address: me@personal.com
    imap: {host: h, username: login@personal.com, password: p}
  - id: work
    from_address: me@work.com
    imap: {host: h, username: login@work.com, password: p}
`)

	cases := map[string]string{
		"personal":           "personal",
		"PERSONAL":           "personal",
		"  personal  ":       "personal",
		"me@personal.com":    "personal",
		"ME@PERSONAL.COM":    "personal",
		"login@personal.com": "personal",
		"work":               "work",
		"me@work.com":        "work",
	}
	for input, want := range cases {
		acc, err := cfg.Resolve(input)
		if err != nil {
			t.Errorf("Resolve(%q): %v", input, err)
			continue
		}
		if acc.ID != want {
			t.Errorf("Resolve(%q) = %q, want %q", input, acc.ID, want)
		}
	}
}

func TestResolveRejectsUnknownAndEmpty(t *testing.T) {
	cfg := loadOK(t, minimal)
	for _, input := range []string{"", "   ", "nonexistent"} {
		if _, err := cfg.Resolve(input); err == nil {
			t.Errorf("Resolve(%q) succeeded", input)
		} else if !strings.Contains(err.Error(), "personal") {
			t.Errorf("Resolve(%q) error should list valid ids: %v", input, err)
		}
	}
}

func TestResolveAmbiguousAddressIsAnError(t *testing.T) {
	// Two accounts sharing an address must not silently resolve to whichever
	// happens to be first — that would send mail from the wrong mailbox.
	cfg := loadOK(t, `
accounts:
  - id: first
    from_address: shared@example.com
    imap: {host: h, username: a@example.com, password: p}
  - id: second
    from_address: shared@example.com
    imap: {host: h, username: b@example.com, password: p}
`)
	_, err := cfg.Resolve("shared@example.com")
	if err == nil {
		t.Fatal("ambiguous address resolved instead of erroring")
	}
	if !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
		t.Errorf("error should name both candidates: %v", err)
	}
	// The unique ids must still work.
	if _, err := cfg.Resolve("first"); err != nil {
		t.Errorf("Resolve by id failed: %v", err)
	}
}

func TestShouldSaveSentProviderDefaults(t *testing.T) {
	// Gmail and Zoho file their own copy on submission; appending another
	// duplicates it (Zoho) or wastes a round trip (Gmail). Everyone else
	// loses the copy entirely unless we append.
	cases := map[string]bool{
		"smtp.gmail.com":      false,
		"smtp.googlemail.com": false,
		"smtp.zoho.com":       false,
		"smtp.zoho.eu":        false,
		"smtp.mail.me.com":    true,
		"smtp.office365.com":  true,
		"mail.mydomain.com":   true,
	}
	for host, want := range cases {
		cfg := loadOK(t, "accounts:\n  - id: a\n    imap: {host: h, username: u@e.com, password: p}\n    smtp: {host: "+host+", username: u@e.com, password: p}")
		if got := cfg.ShouldSaveSent(cfg.Accounts[0]); got != want {
			t.Errorf("ShouldSaveSent(%s) = %v, want %v", host, got, want)
		}
	}
}

func TestShouldSaveSentExplicitOverridesProvider(t *testing.T) {
	cfg := loadOK(t, `
accounts:
  - id: gmail-forced-on
    save_sent: true
    imap: {host: h, username: u@e.com, password: p}
    smtp: {host: smtp.gmail.com, username: u@e.com, password: p}
  - id: generic-forced-off
    save_sent: false
    imap: {host: h, username: u@e.com, password: p}
    smtp: {host: mail.example.com, username: u@e.com, password: p}
`)
	on, _ := cfg.Resolve("gmail-forced-on")
	off, _ := cfg.Resolve("generic-forced-off")
	if !cfg.ShouldSaveSent(on) {
		t.Error("explicit save_sent: true should beat the Gmail default")
	}
	if cfg.ShouldSaveSent(off) {
		t.Error("explicit save_sent: false should beat the generic default")
	}
}

func TestParseSecurity(t *testing.T) {
	valid := map[string]Security{
		"tls": SecurityTLS, "TLS": SecurityTLS, "ssl": SecurityTLS, " implicit ": SecurityTLS,
		"starttls": SecuritySTARTTLS, "STARTTLS": SecuritySTARTTLS,
		"plain": SecurityPlain, "none": SecurityPlain, "insecure": SecurityPlain,
	}
	for in, want := range valid {
		got, err := ParseSecurity(in)
		if err != nil {
			t.Errorf("ParseSecurity(%q): %v", in, err)
		} else if got != want {
			t.Errorf("ParseSecurity(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "tsl", "secure", "yes"} {
		if _, err := ParseSecurity(in); err == nil {
			t.Errorf("ParseSecurity(%q) succeeded", in)
		}
	}
}

func TestAccountIDs(t *testing.T) {
	cfg := loadOK(t, `
accounts:
  - id: one
    imap: {host: h, username: u@e.com, password: p}
  - id: two
    imap: {host: h, username: u@e.com, password: p}
`)
	got := cfg.AccountIDs()
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("AccountIDs = %v", got)
	}
}

func TestMissingIDGetsGenerated(t *testing.T) {
	cfg := loadOK(t, `
accounts:
  - imap: {host: h, username: u@e.com, password: p}
  - imap: {host: h, username: v@e.com, password: p}
`)
	if cfg.Accounts[0].ID != "account-0" || cfg.Accounts[1].ID != "account-1" {
		t.Errorf("generated ids = %v", cfg.AccountIDs())
	}
}
