// Package config loads and validates mail-mcp server configuration.
//
// Configuration lives in a YAML file (default: config.yml) so credentials
// never reach the agent — tools only ever receive an opaque account_id.
// A small number of environment variables override transport-level settings.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Security describes how a connection is protected.
type Security string

const (
	// SecurityTLS dials directly over TLS (IMAP 993, SMTP 465).
	SecurityTLS Security = "tls"
	// SecuritySTARTTLS connects in plaintext then upgrades (IMAP 143, SMTP 587).
	SecuritySTARTTLS Security = "starttls"
	// SecurityPlain never encrypts. Only for trusted local relays.
	SecurityPlain Security = "plain"
)

// ParseSecurity normalizes a user-supplied security mode.
func ParseSecurity(v string) (Security, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "tls", "ssl", "implicit":
		return SecurityTLS, nil
	case "starttls":
		return SecuritySTARTTLS, nil
	case "plain", "none", "insecure":
		return SecurityPlain, nil
	default:
		return "", fmt.Errorf("unsupported security mode %q (want tls, starttls, or plain)", v)
	}
}

// Endpoint is a host/port pair with credentials.
type Endpoint struct {
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`
	Security Security `yaml:"security"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
}

// Account is a single mailbox the server can act on.
type Account struct {
	ID   string   `yaml:"id"`
	IMAP Endpoint `yaml:"imap"`
	SMTP Endpoint `yaml:"smtp"`

	// FromAddress overrides the From header. Defaults to the SMTP username,
	// which matters for providers where the login differs from the sending
	// identity (iCloud custom domains, Google Workspace aliases).
	FromAddress string `yaml:"from_address"`
	// FromName is the optional display name on outgoing mail.
	FromName string `yaml:"from_name"`

	// AllowSend / AllowDelete override the global gates for this account.
	AllowSend   *bool `yaml:"allow_send"`
	AllowDelete *bool `yaml:"allow_delete"`

	// SaveSent controls whether outgoing mail is APPENDed to the Sent
	// folder. Nil means "decide from the provider" — see ShouldSaveSent.
	SaveSent *bool `yaml:"save_sent"`

	// ---- legacy flat keys (poke-mail v1 config), normalized on load ----
	LegacyIMAPHost string `yaml:"imap_host"`
	LegacyIMAPPort int    `yaml:"imap_port"`
	LegacyIMAPUser string `yaml:"imap_username"`
	LegacyIMAPPass string `yaml:"imap_password"`
	LegacySMTPHost string `yaml:"smtp_host"`
	LegacySMTPPort int    `yaml:"smtp_port"`
	LegacySMTPUser string `yaml:"smtp_username"`
	LegacySMTPPass string `yaml:"smtp_password"`
}

// Limits bound resource usage so a single tool call cannot exhaust the
// agent's context window or the server's memory.
type Limits struct {
	// MaxBodyChars truncates each returned body part.
	MaxBodyChars int `yaml:"max_body_chars"`
	// MaxSearchResults caps the page size of search_emails.
	MaxSearchResults int `yaml:"max_search_results"`
	// MaxAttachmentBytes rejects downloads above this size.
	MaxAttachmentBytes int64 `yaml:"max_attachment_bytes"`
	// AttachmentDir is where get_attachment writes files.
	// Empty means the system temp directory.
	AttachmentDir string `yaml:"attachment_dir"`
}

// Timeouts bound every network operation.
type Timeouts struct {
	IMAPConnect time.Duration
	IMAPCommand time.Duration
	SMTPConnect time.Duration
	SMTPSend    time.Duration
}

type rawTimeouts struct {
	IMAPConnect string `yaml:"imap_connect"`
	IMAPCommand string `yaml:"imap_command"`
	SMTPConnect string `yaml:"smtp_connect"`
	SMTPSend    string `yaml:"smtp_send"`
}

// Config is the fully resolved server configuration.
type Config struct {
	AllowSend   bool        `yaml:"allow_send"`
	AllowDelete bool        `yaml:"allow_delete"`
	Limits      Limits      `yaml:"limits"`
	Timeouts    Timeouts    `yaml:"-"`
	RawTimeouts rawTimeouts `yaml:"timeouts"`
	Accounts    []*Account  `yaml:"accounts"`

	// IdleConnTTL is how long a pooled IMAP connection may sit unused
	// before it is closed.
	IdleConnTTL time.Duration `yaml:"-"`
}

// Default values applied when the config omits them.
const (
	DefaultMaxBodyChars       = 50_000
	DefaultMaxSearchResults   = 100
	DefaultMaxAttachmentBytes = 25 << 20 // 25 MiB
	DefaultIMAPConnect        = 30 * time.Second
	DefaultIMAPCommand        = 60 * time.Second
	DefaultSMTPConnect        = 30 * time.Second
	DefaultSMTPSend           = 5 * time.Minute
	DefaultIdleConnTTL        = 5 * time.Minute
)

// Load reads, normalizes, and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(false)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalize() error {
	if c.Limits.MaxBodyChars <= 0 {
		c.Limits.MaxBodyChars = DefaultMaxBodyChars
	}
	if c.Limits.MaxSearchResults <= 0 {
		c.Limits.MaxSearchResults = DefaultMaxSearchResults
	}
	if c.Limits.MaxAttachmentBytes <= 0 {
		c.Limits.MaxAttachmentBytes = DefaultMaxAttachmentBytes
	}
	if c.Limits.AttachmentDir == "" {
		c.Limits.AttachmentDir = os.TempDir()
	}

	var err error
	if c.Timeouts.IMAPConnect, err = parseDuration(c.RawTimeouts.IMAPConnect, DefaultIMAPConnect); err != nil {
		return fmt.Errorf("timeouts.imap_connect: %w", err)
	}
	if c.Timeouts.IMAPCommand, err = parseDuration(c.RawTimeouts.IMAPCommand, DefaultIMAPCommand); err != nil {
		return fmt.Errorf("timeouts.imap_command: %w", err)
	}
	if c.Timeouts.SMTPConnect, err = parseDuration(c.RawTimeouts.SMTPConnect, DefaultSMTPConnect); err != nil {
		return fmt.Errorf("timeouts.smtp_connect: %w", err)
	}
	if c.Timeouts.SMTPSend, err = parseDuration(c.RawTimeouts.SMTPSend, DefaultSMTPSend); err != nil {
		return fmt.Errorf("timeouts.smtp_send: %w", err)
	}
	c.IdleConnTTL = DefaultIdleConnTTL

	for i, acc := range c.Accounts {
		if err := acc.normalize(i); err != nil {
			return err
		}
	}
	return nil
}

func parseDuration(v string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", v)
	}
	return d, nil
}

func (a *Account) normalize(index int) error {
	// Fold legacy flat keys into the nested form. Nested wins if both exist.
	if a.IMAP.Host == "" {
		a.IMAP.Host = a.LegacyIMAPHost
	}
	if a.IMAP.Port == 0 {
		a.IMAP.Port = a.LegacyIMAPPort
	}
	if a.IMAP.Username == "" {
		a.IMAP.Username = a.LegacyIMAPUser
	}
	if a.IMAP.Password == "" {
		a.IMAP.Password = a.LegacyIMAPPass
	}
	if a.SMTP.Host == "" {
		a.SMTP.Host = a.LegacySMTPHost
	}
	if a.SMTP.Port == 0 {
		a.SMTP.Port = a.LegacySMTPPort
	}
	if a.SMTP.Username == "" {
		a.SMTP.Username = a.LegacySMTPUser
	}
	if a.SMTP.Password == "" {
		a.SMTP.Password = a.LegacySMTPPass
	}

	if a.ID == "" {
		a.ID = fmt.Sprintf("account-%d", index)
	}

	// SMTP falls back to IMAP credentials — the common case for providers
	// that use one login for both.
	if a.SMTP.Host == "" {
		a.SMTP.Host = a.IMAP.Host
	}
	if a.SMTP.Username == "" {
		a.SMTP.Username = a.IMAP.Username
	}
	if a.SMTP.Password == "" {
		a.SMTP.Password = a.IMAP.Password
	}

	if a.IMAP.Port == 0 {
		a.IMAP.Port = 993
	}
	if a.SMTP.Port == 0 {
		a.SMTP.Port = 587
	}

	// Infer security from the port when unset, but let an explicit value win.
	var err error
	if a.IMAP.Security == "" {
		a.IMAP.Security = defaultIMAPSecurity(a.IMAP.Port)
	} else if a.IMAP.Security, err = ParseSecurity(string(a.IMAP.Security)); err != nil {
		return fmt.Errorf("account %q imap.security: %w", a.ID, err)
	}
	if a.SMTP.Security == "" {
		a.SMTP.Security = defaultSMTPSecurity(a.SMTP.Port)
	} else if a.SMTP.Security, err = ParseSecurity(string(a.SMTP.Security)); err != nil {
		return fmt.Errorf("account %q smtp.security: %w", a.ID, err)
	}

	if a.FromAddress == "" {
		a.FromAddress = a.SMTP.Username
	}
	return nil
}

func defaultIMAPSecurity(port int) Security {
	if port == 143 {
		return SecuritySTARTTLS
	}
	return SecurityTLS
}

func defaultSMTPSecurity(port int) Security {
	if port == 465 {
		return SecurityTLS
	}
	return SecuritySTARTTLS
}

func (c *Config) validate() error {
	if len(c.Accounts) == 0 {
		return fmt.Errorf("no accounts configured")
	}
	seen := make(map[string]bool, len(c.Accounts))
	for _, a := range c.Accounts {
		key := strings.ToLower(a.ID)
		if seen[key] {
			return fmt.Errorf("duplicate account id %q", a.ID)
		}
		seen[key] = true

		if a.IMAP.Host == "" {
			return fmt.Errorf("account %q: imap.host is required", a.ID)
		}
		if a.IMAP.Username == "" {
			return fmt.Errorf("account %q: imap.username is required", a.ID)
		}
		if a.IMAP.Password == "" {
			return fmt.Errorf("account %q: imap.password is required", a.ID)
		}
		if a.IMAP.Port < 1 || a.IMAP.Port > 65535 {
			return fmt.Errorf("account %q: imap.port %d out of range", a.ID, a.IMAP.Port)
		}
		if a.SMTP.Port < 1 || a.SMTP.Port > 65535 {
			return fmt.Errorf("account %q: smtp.port %d out of range", a.ID, a.SMTP.Port)
		}
		if !strings.Contains(a.FromAddress, "@") {
			return fmt.Errorf("account %q: from_address %q is not an email address", a.ID, a.FromAddress)
		}
	}
	return nil
}

// Resolve finds an account by id or by any of its email addresses.
//
// Matching by address is a convenience for agents that only know the user's
// email. Ambiguous matches are an error rather than a silent first-wins pick.
func (c *Config) Resolve(accountID string) (*Account, error) {
	needle := strings.ToLower(strings.TrimSpace(accountID))
	if needle == "" {
		return nil, fmt.Errorf("account_id is required; available: %s", strings.Join(c.AccountIDs(), ", "))
	}
	for _, a := range c.Accounts {
		if strings.ToLower(a.ID) == needle {
			return a, nil
		}
	}

	var matches []*Account
	for _, a := range c.Accounts {
		for _, addr := range []string{a.FromAddress, a.IMAP.Username, a.SMTP.Username} {
			if addr != "" && strings.EqualFold(addr, needle) {
				matches = append(matches, a)
				break
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("unknown account_id %q; available: %s", accountID, strings.Join(c.AccountIDs(), ", "))
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return nil, fmt.Errorf("account_id %q matches multiple accounts (%s); pass the unique id instead",
			accountID, strings.Join(ids, ", "))
	}
}

// AccountIDs lists every configured account id.
func (c *Config) AccountIDs() []string {
	ids := make([]string, len(c.Accounts))
	for i, a := range c.Accounts {
		ids[i] = a.ID
	}
	return ids
}

// SendAllowed reports whether the account may send mail.
func (c *Config) SendAllowed(a *Account) bool {
	if a.AllowSend != nil {
		return *a.AllowSend
	}
	return c.AllowSend
}

// DeleteAllowed reports whether the account may delete mail or folders.
func (c *Config) DeleteAllowed(a *Account) bool {
	if a.AllowDelete != nil {
		return *a.AllowDelete
	}
	return c.AllowDelete
}

// ShouldSaveSent reports whether a copy of outgoing mail should be APPENDed
// to the IMAP Sent folder.
//
// Providers differ: Gmail and Zoho save a server-side copy on SMTP
// submission, so an APPEND is redundant (Gmail dedupes by Message-ID; Zoho
// does not, and yields a visible duplicate). iCloud, Office 365, and generic
// relays do not save anything, so without an APPEND the sent copy is lost.
func (c *Config) ShouldSaveSent(a *Account) bool {
	if a.SaveSent != nil {
		return *a.SaveSent
	}
	return !providerAutoSavesSent(a.SMTP.Host)
}

func providerAutoSavesSent(host string) bool {
	h := strings.ToLower(host)
	for _, needle := range []string{"gmail", "googlemail", "zoho"} {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
