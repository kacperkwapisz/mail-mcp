// Package mailbox provides pooled, timeout-bounded IMAP access.
//
// IMAP is a stateful protocol: a connection has one selected mailbox at a
// time, and command results depend on that state. Rather than reconnecting
// per tool call (a TLS handshake plus LOGIN every time), the pool keeps one
// authenticated connection per account and serializes operations on it.
package mailbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"strings"
	"sync"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
)

// Version is stamped into the IMAP ID exchange and get_server_info.
// Overwritten from main at startup.
var Version = "dev"

// clientID identifies us to the server via RFC 2971 ID. Some providers
// (notably Netease 163/126) reject clients that never identify themselves.
// Built lazily so it picks up the Version set by main.
func clientID() *imap.IDData {
	return &imap.IDData{Name: "mail-mcp", Version: Version, Vendor: "mail-mcp"}
}

// Pool holds one IMAP connection per account.
type Pool struct {
	cfg    *config.Config
	logger *slog.Logger

	mu    sync.Mutex
	conns map[string]*pooledConn
}

// pooledConn is a single authenticated connection plus its state.
//
// The mutex serializes command execution: go-imap permits concurrent use but
// gives no ordering guarantees, and SELECT-dependent commands need ordering.
type pooledConn struct {
	mu       sync.Mutex
	client   *imapclient.Client
	lastUsed time.Time
	dead     bool

	selected            string
	selectedUIDValidity uint32
	selectedReadOnly    bool
}

// NewPool creates an empty pool. Connections are established lazily.
func NewPool(cfg *config.Config, logger *slog.Logger) *Pool {
	return &Pool{
		cfg:    cfg,
		logger: logger,
		conns:  make(map[string]*pooledConn),
	}
}

// Close tears down every pooled connection.
func (p *Pool) Close() {
	p.mu.Lock()
	conns := make([]*pooledConn, 0, len(p.conns))
	for _, c := range p.conns {
		conns = append(conns, c)
	}
	p.conns = make(map[string]*pooledConn)
	p.mu.Unlock()

	for _, c := range conns {
		c.mu.Lock()
		if c.client != nil {
			_ = c.client.Logout().Wait()
			_ = c.client.Close()
		}
		c.mu.Unlock()
	}
}

// Do runs fn against an authenticated session for acc.
//
// Operations on one account are serialized. The whole callback is bounded by
// the configured command timeout; on expiry the connection is force-closed,
// which is the only way to unblock a stuck IMAP command, and the next call
// reconnects.
func (p *Pool) Do(ctx context.Context, acc *config.Account, fn func(*Session) error) error {
	pc := p.connFor(acc)

	pc.mu.Lock()
	defer pc.mu.Unlock()

	if err := p.ensureAlive(pc, acc); err != nil {
		return err
	}

	sess := &Session{conn: pc, pool: p, account: acc}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeouts.IMAPCommand)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			// A panic inside a mail parser must not take down the server.
			if r := recover(); r != nil {
				done <- fmt.Errorf("internal error during IMAP operation: %v", r)
			}
		}()
		done <- fn(sess)
	}()

	select {
	case err := <-done:
		pc.lastUsed = time.Now()
		if err != nil && isConnectionError(err) {
			p.discard(pc, acc)
		}
		return err
	case <-ctx.Done():
		// The in-flight command owns the socket; closing it is what makes
		// the blocked read return so the goroutine can exit.
		p.discard(pc, acc)
		return fmt.Errorf("IMAP operation timed out after %s", p.cfg.Timeouts.IMAPCommand)
	}
}

func (p *Pool) connFor(acc *config.Account) *pooledConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	pc, ok := p.conns[acc.ID]
	if !ok {
		pc = &pooledConn{}
		p.conns[acc.ID] = pc
	}
	return pc
}

// ensureAlive reconnects when the pooled connection is missing, marked dead,
// or has gone stale while idle.
//
// Caller must hold pc.mu.
func (p *Pool) ensureAlive(pc *pooledConn, acc *config.Account) error {
	if pc.client != nil && !pc.dead {
		if time.Since(pc.lastUsed) < p.cfg.IdleConnTTL && pc.client.State() >= imap.ConnStateAuthenticated {
			return nil
		}
		// A server may have dropped an idle connection without telling us.
		// NOOP is the cheapest way to find out before a real command fails.
		if err := pc.client.Noop().Wait(); err == nil {
			return nil
		}
		p.logger.Debug("pooled IMAP connection went stale, reconnecting", "account", acc.ID)
		p.closeConn(pc)
	}

	client, err := dial(acc, p.cfg.Timeouts.IMAPConnect)
	if err != nil {
		return err
	}
	pc.client = client
	pc.dead = false
	pc.selected = ""
	pc.selectedUIDValidity = 0
	pc.lastUsed = time.Now()
	p.logger.Debug("opened IMAP connection", "account", acc.ID, "host", acc.IMAP.Host)
	return nil
}

// discard marks a connection unusable and closes it.
// Caller must hold pc.mu.
func (p *Pool) discard(pc *pooledConn, acc *config.Account) {
	p.logger.Debug("discarding IMAP connection", "account", acc.ID)
	p.closeConn(pc)
	pc.dead = true
}

func (p *Pool) closeConn(pc *pooledConn) {
	if pc.client != nil {
		_ = pc.client.Close()
	}
	pc.client = nil
	pc.selected = ""
	pc.selectedUIDValidity = 0
}

// Verify opens a throwaway connection to check host, TLS, and credentials
// without disturbing the pooled one.
func Verify(acc *config.Account, timeout time.Duration) error {
	client, err := dial(acc, timeout)
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck // best-effort teardown
	return client.Logout().Wait()
}

func dial(acc *config.Account, timeout time.Duration) (*imapclient.Client, error) {
	addr := net.JoinHostPort(acc.IMAP.Host, fmt.Sprint(acc.IMAP.Port))
	opts := &imapclient.Options{
		Dialer: &net.Dialer{Timeout: timeout},
		// Subjects and display names are frequently encoded in legacy
		// charsets; without this decoder they come back as mojibake.
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
	}

	var (
		client *imapclient.Client
		err    error
	)
	switch acc.IMAP.Security {
	case config.SecurityTLS:
		client, err = imapclient.DialTLS(addr, opts)
	case config.SecuritySTARTTLS:
		client, err = imapclient.DialStartTLS(addr, opts)
	case config.SecurityPlain:
		client, err = imapclient.DialInsecure(addr, opts)
	default:
		return nil, fmt.Errorf("account %q: unsupported imap security %q", acc.ID, acc.IMAP.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}

	if err := client.Login(acc.IMAP.Username, acc.IMAP.Password).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("IMAP login failed for %s: %w", acc.IMAP.Username, err)
	}

	// Best effort: servers that lack ID must not fail an otherwise good login.
	if client.Caps().Has(imap.CapID) {
		_, _ = client.ID(clientID()).Wait()
	}
	return client, nil
}

// isConnectionError reports whether an error means the socket is unusable,
// as opposed to the server rejecting one command.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"use of closed network connection",
		"connection reset",
		"broken pipe",
		"eof",
		"i/o timeout",
		"connection closed",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
