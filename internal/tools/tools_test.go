package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/msgid"
)

const testConfig = `
allow_send: true
allow_delete: false
accounts:
  - id: personal
    from_address: me@example.com
    from_name: Test User
    imap:
      host: imap.example.com
      username: me@example.com
      password: secret-imap-password
    smtp:
      host: smtp.example.com
      username: me@example.com
      password: secret-smtp-password
  - id: work
    imap:
      host: imap.work.example
      username: me@work.example
      password: another-secret
    allow_send: false
`

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(testConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// connect wires an in-memory client to a fully registered server.
func connect(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := mailbox.NewPool(cfg, logger)
	t.Cleanup(pool.Close)

	srv := mcp.NewServer(&mcp.Implementation{Name: "mail-mcp", Version: "test"},
		&mcp.ServerOptions{Instructions: Instructions})
	New(cfg, pool, logger, "test").Register(srv)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	go func() { _ = srv.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestAllToolsRegister(t *testing.T) {
	session := connect(t, loadTestConfig(t))

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	got := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}

	want := []string{
		"list_accounts", "verify_account", "get_server_info",
		"search_emails", "read_email", "get_attachment",
		"send_email", "reply_email", "forward_email", "create_draft",
		"archive_email", "move_email", "mark_email", "delete_email",
		"list_folders", "create_folder", "rename_folder", "delete_folder",
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("tool %q was not registered", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("registered %d tools, expected %d", len(got), len(want))
	}

	for name, tool := range got {
		if tool.Description == "" {
			t.Errorf("tool %q has no description", name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", name)
		}
	}
}

// schemaFor marshals a tool's input schema for assertion.
func schemaFor(t *testing.T, session *mcp.ClientSession, name string) map[string]any {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("marshal schema: %v", err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("unmarshal schema: %v", err)
			}
			return out
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func requiredFields(schema map[string]any) map[string]bool {
	out := map[string]bool{}
	if list, ok := schema["required"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func properties(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties: %v", schema)
	}
	return props
}

func TestPerMessageToolsTakeNoAccountID(t *testing.T) {
	session := connect(t, loadTestConfig(t))

	// The handle already names the account. Accepting a separate account_id
	// would let a caller aim an operation at the wrong mailbox, where the
	// same UID is valid but points at a different message.
	for _, name := range []string{
		"read_email", "get_attachment", "reply_email", "forward_email",
		"archive_email", "move_email", "mark_email", "delete_email",
	} {
		props := properties(t, schemaFor(t, session, name))
		if _, has := props["account_id"]; has {
			t.Errorf("%s exposes account_id; the message_id already carries the account", name)
		}
		if _, has := props["message_id"]; !has {
			t.Errorf("%s is missing message_id", name)
		}
	}
}

func TestRequiredFields(t *testing.T) {
	session := connect(t, loadTestConfig(t))

	cases := map[string][]string{
		"search_emails": {"account_id"},
		"read_email":    {"message_id"},
		"send_email":    {"account_id", "to", "subject"},
		"reply_email":   {"message_id"},
		"forward_email": {"message_id", "to"},
		"move_email":    {"message_id", "to_folder"},
		"mark_email":    {"message_id", "action"},
		"delete_email":  {"message_id", "confirm"},
		"delete_folder": {"account_id", "name", "confirm"},
		"create_folder": {"account_id", "name"},
	}
	for tool, want := range cases {
		required := requiredFields(schemaFor(t, session, tool))
		for _, field := range want {
			if !required[field] {
				t.Errorf("%s: %q should be required, required set is %v", tool, field, required)
			}
		}
	}
}

func TestOptionalFieldsAreNotRequired(t *testing.T) {
	session := connect(t, loadTestConfig(t))

	// A model that has to supply every field to make a basic call will make
	// worse calls; defaults belong on the server.
	cases := map[string][]string{
		"search_emails": {"folder", "limit", "offset", "from", "subject", "unseen"},
		"read_email":    {"include_html", "include_headers", "mark_as_read"},
		"send_email":    {"cc", "bcc", "body_text", "body_html", "attachments", "reply_to"},
		"reply_email":   {"reply_all", "quote_original", "body_text", "body_html"},
	}
	for tool, fields := range cases {
		schema := schemaFor(t, session, tool)
		required := requiredFields(schema)
		props := properties(t, schema)
		for _, f := range fields {
			if _, exists := props[f]; !exists {
				t.Errorf("%s: expected property %q to exist", tool, f)
			}
			if required[f] {
				t.Errorf("%s: %q should be optional", tool, f)
			}
		}
	}
}

func TestSchemaDescriptionsExist(t *testing.T) {
	session := connect(t, loadTestConfig(t))

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		raw, _ := json.Marshal(tool.InputSchema)
		var schema map[string]any
		_ = json.Unmarshal(raw, &schema)
		props, _ := schema["properties"].(map[string]any)
		for name, p := range props {
			prop, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if desc, _ := prop["description"].(string); strings.TrimSpace(desc) == "" {
				t.Errorf("%s.%s has no description", tool.Name, name)
			}
		}
	}
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestListAccountsNeverLeaksCredentials(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	res := callTool(t, session, "list_accounts", map[string]any{})
	if res.IsError {
		t.Fatalf("list_accounts failed: %s", resultText(res))
	}

	body := resultText(res)
	// The whole reason this server exists is that the agent must never see
	// how to connect to the mailbox itself.
	for _, secret := range []string{"secret-imap-password", "secret-smtp-password", "another-secret", "password"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(secret)) {
			t.Errorf("list_accounts leaked %q in: %s", secret, body)
		}
	}
	for _, want := range []string{"personal", "work", "me@example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("list_accounts omitted %q: %s", want, body)
		}
	}
}

func TestGetServerInfoNeverLeaksCredentials(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	body := resultText(callTool(t, session, "get_server_info", map[string]any{}))
	for _, secret := range []string{"secret-imap-password", "secret-smtp-password", "another-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("get_server_info leaked %q", secret)
		}
	}
	if !strings.Contains(body, "test") {
		t.Errorf("get_server_info omitted the version: %s", body)
	}
}

func TestSendBlockedWhenAccountDisallowsIt(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	res := callTool(t, session, "send_email", map[string]any{
		"account_id": "work", // allow_send: false
		"to":         []string{"someone@example.com"},
		"subject":    "Should not send",
		"body_text":  "nope",
	})
	if !res.IsError {
		t.Fatal("send_email succeeded on an account with sending disabled")
	}
	if !strings.Contains(resultText(res), "disabled") {
		t.Errorf("unhelpful error: %s", resultText(res))
	}
}

func TestDeleteRequiresConfirmation(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	handle := msgid.Encode("personal", "INBOX", 1, 5)

	res := callTool(t, session, "delete_email", map[string]any{
		"message_id": handle,
		"confirm":    false,
	})
	if !res.IsError {
		t.Fatal("delete_email proceeded without confirmation")
	}
	if !strings.Contains(resultText(res), "confirm") {
		t.Errorf("error should mention confirmation: %s", resultText(res))
	}
}

func TestDeleteBlockedWhenAccountDisallowsIt(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	// Confirmed, but the config has allow_delete: false globally.
	res := callTool(t, session, "delete_email", map[string]any{
		"message_id": msgid.Encode("personal", "INBOX", 1, 5),
		"confirm":    true,
	})
	if !res.IsError {
		t.Fatal("delete_email succeeded despite allow_delete being false")
	}
	if !strings.Contains(resultText(res), "disabled") {
		t.Errorf("unhelpful error: %s", resultText(res))
	}
}

func TestUnknownAccountIsRejectedClearly(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	res := callTool(t, session, "search_emails", map[string]any{"account_id": "nope"})
	if !res.IsError {
		t.Fatal("search_emails accepted an unknown account")
	}
	body := resultText(res)
	if !strings.Contains(body, "nope") || !strings.Contains(body, "personal") {
		t.Errorf("error should name the bad id and list valid ones: %s", body)
	}
}

func TestMalformedMessageIDIsRejected(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	for _, handle := range []string{"", "garbage", "INBOX:1:2", "YQ.SU5CT1g.1"} {
		res := callTool(t, session, "read_email", map[string]any{"message_id": handle})
		if !res.IsError {
			t.Errorf("read_email accepted malformed handle %q", handle)
		}
	}
}

func TestMessageIDForUnconfiguredAccountIsRejected(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	// A well-formed handle naming an account that no longer exists must be
	// refused rather than silently falling back to some other mailbox.
	res := callTool(t, session, "read_email", map[string]any{
		"message_id": msgid.Encode("deleted-account", "INBOX", 1, 5),
	})
	if !res.IsError {
		t.Fatal("read_email accepted a handle for an unconfigured account")
	}
	if !strings.Contains(resultText(res), "deleted-account") {
		t.Errorf("error should name the missing account: %s", resultText(res))
	}
}

func TestMarkEmailRejectsUnknownAction(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	res := callTool(t, session, "mark_email", map[string]any{
		"message_id": msgid.Encode("personal", "INBOX", 1, 5),
		"action":     "incinerate",
	})
	if !res.IsError {
		t.Fatal("mark_email accepted an unknown action")
	}
	if !strings.Contains(resultText(res), "incinerate") {
		t.Errorf("error should echo the bad action: %s", resultText(res))
	}
}

func TestSendRejectsWrapperLeakBeforeConnecting(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	// smtp.example.com does not resolve, so reaching the network would be a
	// different error. Getting the validation error proves the check runs
	// before any connection is attempted.
	res := callTool(t, session, "send_email", map[string]any{
		"account_id": "personal",
		"to":         []string{"someone@example.com"},
		"subject":    "Leak test",
		"body_text":  `Hi</body_text><parameter name="body_html"><p>Hi</p>`,
	})
	if !res.IsError {
		t.Fatal("send_email accepted leaked tool-call syntax")
	}
	if !strings.Contains(resultText(res), "separate") {
		t.Errorf("error should explain the two-field rule: %s", resultText(res))
	}
}

func TestSendValidatesRecipientsBeforeConnecting(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	res := callTool(t, session, "send_email", map[string]any{
		"account_id": "personal",
		"to":         []string{"not-an-address"},
		"subject":    "Bad recipient",
		"body_text":  "hi",
	})
	if !res.IsError {
		t.Fatal("send_email accepted an invalid recipient")
	}
	if !strings.Contains(resultText(res), "not-an-address") {
		t.Errorf("error should quote the bad address: %s", resultText(res))
	}
}

func TestSearchRejectsContradictoryFilters(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	res := callTool(t, session, "search_emails", map[string]any{
		"account_id": "personal",
		"seen":       true,
		"unseen":     true,
	})
	if !res.IsError {
		t.Fatal("search_emails accepted seen and unseen together")
	}
}

func TestSearchRejectsBadDates(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	for _, field := range []string{"since", "before"} {
		res := callTool(t, session, "search_emails", map[string]any{
			"account_id": "personal",
			field:        "last tuesday",
		})
		if !res.IsError {
			t.Errorf("search_emails accepted %s=%q", field, "last tuesday")
		}
	}
}

func TestFolderToolsRefuseInbox(t *testing.T) {
	session := connect(t, loadTestConfig(t))
	res := callTool(t, session, "rename_folder", map[string]any{
		"account_id": "personal",
		"old_name":   "INBOX",
		"new_name":   "Something",
	})
	if !res.IsError {
		t.Fatal("rename_folder accepted INBOX")
	}
	if !strings.Contains(resultText(res), "INBOX") {
		t.Errorf("error should name INBOX: %s", resultText(res))
	}
}

func TestInstructionsCoverTheCriticalRules(t *testing.T) {
	// These are the behaviours that go wrong in practice, so if someone
	// trims the instructions the tests should notice.
	for _, needle := range []string{
		"list_accounts",
		"body_text and body_html",
		"never concatenate",
		"approval",
		"confirm: true",
		"get_attachment",
	} {
		if !strings.Contains(strings.ToLower(Instructions), strings.ToLower(needle)) {
			t.Errorf("instructions no longer mention %q", needle)
		}
	}
}
