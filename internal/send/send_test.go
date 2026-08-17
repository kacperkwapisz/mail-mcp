package send

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
)

func testAccount() *config.Account {
	return &config.Account{
		ID:          "test",
		FromAddress: "me@example.com",
		SMTP:        config.Endpoint{Host: "smtp.example.com", Port: 587, Username: "me@example.com", Password: "pw"},
	}
}

func buildRaw(t *testing.T, comp *Composition) string {
	t.Helper()
	msg, err := Build(testAccount(), comp)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var buf bytes.Buffer
	if _, err := msg.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return buf.String()
}

func TestBuildTextOnly(t *testing.T) {
	raw := buildRaw(t, &Composition{
		To:       []string{"you@example.com"},
		Subject:  "Hello",
		BodyText: "Plain body",
	})
	if !strings.Contains(raw, "Subject: Hello") {
		t.Errorf("missing subject:\n%s", raw)
	}
	if !strings.Contains(raw, "Plain body") {
		t.Errorf("missing body:\n%s", raw)
	}
	if !strings.Contains(raw, "Message-ID:") {
		t.Errorf("missing generated Message-ID:\n%s", raw)
	}
	if !strings.Contains(raw, "Date:") {
		t.Errorf("missing Date header:\n%s", raw)
	}
}

func TestBuildMultipartAlternative(t *testing.T) {
	raw := buildRaw(t, &Composition{
		To:       []string{"you@example.com"},
		Subject:  "Both bodies",
		BodyText: "Plain version",
		BodyHTML: "<p>HTML version</p>",
	})
	if !strings.Contains(raw, "multipart/alternative") {
		t.Errorf("expected multipart/alternative:\n%s", raw)
	}
	for _, want := range []string{"Plain version", "HTML version", "text/plain", "text/html"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in:\n%s", want, raw)
		}
	}
}

func TestBuildHTMLOnlyStillSendsTextFallback(t *testing.T) {
	raw := buildRaw(t, &Composition{
		To:       []string{"you@example.com"},
		Subject:  "HTML only",
		BodyHTML: "<p>Rich</p>",
	})
	if !strings.Contains(raw, "text/plain") {
		t.Errorf("html-only send should still carry a text/plain part:\n%s", raw)
	}
}

func TestBuildEncodesNonASCIISubject(t *testing.T) {
	raw := buildRaw(t, &Composition{
		To:       []string{"you@example.com"},
		Subject:  "Zamówienie — ñ",
		BodyText: "body",
	})
	if strings.Contains(raw, "Subject: Zamówienie — ñ") {
		t.Errorf("non-ASCII subject was sent raw, must be RFC 2047 encoded:\n%s", raw)
	}
	if !strings.Contains(strings.ToLower(raw), "=?utf-8?") {
		t.Errorf("expected RFC 2047 encoded subject:\n%s", raw)
	}
}

func TestBuildThreadingHeaders(t *testing.T) {
	raw := buildRaw(t, &Composition{
		To:         []string{"you@example.com"},
		Subject:    "Re: Original",
		BodyText:   "reply",
		InReplyTo:  "parent@example.com", // no angle brackets on purpose
		References: []string{"<root@example.com>", "parent@example.com"},
	})
	if !strings.Contains(raw, "In-Reply-To: <parent@example.com>") {
		t.Errorf("In-Reply-To not normalized to angle brackets:\n%s", raw)
	}
	if !strings.Contains(raw, "References: <root@example.com> <parent@example.com>") {
		t.Errorf("References not normalized:\n%s", raw)
	}
}

func TestBuildWithAttachment(t *testing.T) {
	raw := buildRaw(t, &Composition{
		To:       []string{"you@example.com"},
		Subject:  "With file",
		BodyText: "see attached",
		Attachments: []Attachment{
			{Filename: "report.pdf", Content: []byte("%PDF-1.4 fake")},
		},
	})
	if !strings.Contains(raw, "multipart/mixed") {
		t.Errorf("expected multipart/mixed:\n%s", raw)
	}
	if !strings.Contains(raw, "report.pdf") {
		t.Errorf("attachment filename missing:\n%s", raw)
	}
	if !strings.Contains(raw, "application/pdf") {
		t.Errorf("content type not inferred from extension:\n%s", raw)
	}
}

func TestBuildFromDisplayName(t *testing.T) {
	acc := testAccount()
	acc.FromName = "Kacper Kwapisz"
	msg, err := Build(acc, &Composition{
		To: []string{"you@example.com"}, Subject: "Hi", BodyText: "body",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var buf bytes.Buffer
	if _, err := msg.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !strings.Contains(buf.String(), "Kacper Kwapisz") {
		t.Errorf("display name missing:\n%s", buf.String())
	}
}

func TestBuildStripsCDATAArtifacts(t *testing.T) {
	raw := buildRaw(t, &Composition{
		To: []string{"you@example.com"}, Subject: "Zoho",
		BodyText: "clean text ]]> tail",
	})
	if strings.Contains(raw, "]]>") {
		t.Errorf("CDATA artifact survived:\n%s", raw)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	base := func() *Composition {
		return &Composition{To: []string{"you@example.com"}, Subject: "s", BodyText: "b"}
	}
	cases := map[string]func(*Composition){
		"no recipients":       func(c *Composition) { c.To = nil },
		"empty subject":       func(c *Composition) { c.Subject = "  " },
		"no body":             func(c *Composition) { c.BodyText = ""; c.BodyHTML = "" },
		"invalid address":     func(c *Composition) { c.To = []string{"not-an-email"} },
		"subject newline":     func(c *Composition) { c.Subject = "a\r\nBcc: victim@x.com" },
		"header injection":    func(c *Composition) { c.To = []string{"a@b.com\r\nBcc: v@x.com"} },
		"too many recipients": func(c *Composition) { c.To = make([]string, MaxRecipients+1) },
		"oversized subject":   func(c *Composition) { c.Subject = strings.Repeat("x", MaxSubjectLen+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base()
			mutate(c)
			if err := Validate(c); err == nil {
				t.Errorf("Validate accepted %s", name)
			}
		})
	}
}

func TestValidateAcceptsGoodInput(t *testing.T) {
	for _, c := range []*Composition{
		{To: []string{"a@b.com"}, Subject: "s", BodyText: "b"},
		{To: []string{"Name <a@b.com>"}, Subject: "s", BodyHTML: "<p>b</p>"},
		{To: []string{"a@b.com"}, Cc: []string{"c@d.com"}, Bcc: []string{"e@f.com"}, Subject: "s", BodyText: "b"},
	} {
		if err := Validate(c); err != nil {
			t.Errorf("Validate rejected a valid composition: %v", err)
		}
	}
}

func TestValidateNoWrapperLeak(t *testing.T) {
	leaks := []string{
		"Hello</body_text><parameter name=\"body_html\"><p>Hi</p>",
		"text <BODY_TEXT> more",
		"<invoke name=\"smtp_send\">",
		"</function_calls>",
	}
	for _, v := range leaks {
		if err := ValidateNoWrapperLeak("body_text", v); err == nil {
			t.Errorf("wrapper leak not caught: %q", v)
		}
	}

	// Legitimate technical content must still pass — a developer discussing
	// XML or an HTML email with a <p> tag is not leaking a tool call.
	safe := []string{
		"",
		"Here is the <parameter> element of the schema.",
		"<p>Normal HTML email</p>",
		"Use the invoke() method on line 40.",
		"See <body> and </body> in the template.",
	}
	for _, v := range safe {
		if err := ValidateNoWrapperLeak("body_html", v); err != nil {
			t.Errorf("false positive on %q: %v", v, err)
		}
	}
}

func TestGuessContentType(t *testing.T) {
	cases := map[string]string{
		"a.pdf": "application/pdf", "b.PNG": "image/png", "c.docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"d.unknown": "application/octet-stream", "noext": "application/octet-stream",
	}
	for in, want := range cases {
		if got := GuessContentType(in); got != want {
			t.Errorf("GuessContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRawIsAppendable(t *testing.T) {
	// The Sent-folder copy is these exact bytes, so they must be a complete,
	// well-formed MIME message rather than a reconstruction.
	raw := buildRaw(t, &Composition{
		To: []string{"you@example.com"}, Cc: []string{"cc@example.com"},
		Subject: "Archive me", BodyText: "text", BodyHTML: "<p>html</p>",
	})
	for _, want := range []string{"MIME-Version: 1.0", "Content-Type: multipart/alternative", "boundary=", "Cc:", "To:", "From:"} {
		if !strings.Contains(raw, want) {
			t.Errorf("serialized message missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "Bcc:") {
		t.Error("Bcc must not appear in the serialized message")
	}
}
