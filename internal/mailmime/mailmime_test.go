package mailmime

import (
	"strings"
	"testing"
)

const multipartMixed = "From: =?utf-8?B?SmFuIEtvd2Fsc2tp?= <jan@example.com>\r\n" +
	"To: Kacper <kacper@example.com>, Second <two@example.com>\r\n" +
	"Cc: cc@example.com\r\n" +
	"Subject: =?utf-8?Q?Zam=C3=B3wienie_nr_42?=\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 +0100\r\n" +
	"Message-ID: <abc123@example.com>\r\n" +
	"In-Reply-To: <parent@example.com>\r\n" +
	"References: <root@example.com> <parent@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=OUTER\r\n" +
	"\r\n" +
	"--OUTER\r\n" +
	"Content-Type: multipart/alternative; boundary=INNER\r\n" +
	"\r\n" +
	"--INNER\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Dzień dobry,\r\nto jest treść.\r\n" +
	"--INNER\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>Dzień dobry</p><script>alert(1)</script><p>to jest treść.</p>\r\n" +
	"--INNER--\r\n" +
	"--OUTER\r\n" +
	"Content-Type: application/pdf; name=\"faktura.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"faktura.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"SGVsbG8gUERG\r\n" +
	"--OUTER--\r\n"

func TestParseMultipart(t *testing.T) {
	msg, err := Parse([]byte(multipartMixed), ParseOptions{IncludeHTML: true})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if want := "Jan Kowalski <jan@example.com>"; msg.From != want {
		t.Errorf("From = %q, want %q", msg.From, want)
	}
	if want := "Zamówienie nr 42"; msg.Subject != want {
		t.Errorf("Subject = %q, want %q", msg.Subject, want)
	}
	if len(msg.To) != 2 {
		t.Errorf("To = %v, want 2 entries", msg.To)
	}
	if len(msg.References) != 2 {
		t.Errorf("References = %v, want 2 entries", msg.References)
	}
	if !strings.HasPrefix(msg.Date, "2006-01-02T15:04:05") {
		t.Errorf("Date = %q, want RFC 3339", msg.Date)
	}
	if !strings.Contains(msg.BodyText, "to jest treść.") {
		t.Errorf("BodyText = %q, missing plain part", msg.BodyText)
	}
	if strings.Contains(msg.BodyHTML, "<script") || strings.Contains(msg.BodyHTML, "alert(1)") {
		t.Errorf("BodyHTML was not sanitized: %q", msg.BodyHTML)
	}
	if !strings.Contains(msg.BodyHTML, "Dzień dobry") {
		t.Errorf("BodyHTML lost its content: %q", msg.BodyHTML)
	}
}

func TestParseAttachmentMetadataWithoutBytes(t *testing.T) {
	msg, err := Parse([]byte(multipartMixed), ParseOptions{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("Attachments = %+v, want 1", msg.Attachments)
	}
	att := msg.Attachments[0]
	if att.Filename != "faktura.pdf" {
		t.Errorf("Filename = %q, want faktura.pdf", att.Filename)
	}
	if att.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q", att.ContentType)
	}
	// "Hello PDF" is 9 bytes once base64-decoded — proving the transfer
	// encoding was undone before measuring.
	if att.Size != 9 {
		t.Errorf("Size = %d, want 9 (decoded)", att.Size)
	}
	if att.PartID != "2" {
		t.Errorf("PartID = %q, want 2", att.PartID)
	}
	// The whole point: bytes must not ride along in the parse result.
	if strings.Contains(msg.BodyText, "Hello PDF") {
		t.Error("attachment content leaked into the body")
	}
}

func TestParseHTMLOnlyDerivesText(t *testing.T) {
	raw := "Subject: HTML only\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>First line</p><p>Second line</p>"
	msg, err := Parse([]byte(raw), ParseOptions{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(msg.BodyText, "First line") || !strings.Contains(msg.BodyText, "Second line") {
		t.Errorf("BodyText = %q, want both paragraphs", msg.BodyText)
	}
	if strings.Contains(msg.BodyText, "<p>") {
		t.Errorf("BodyText still contains markup: %q", msg.BodyText)
	}
	// Paragraph boundaries must survive as line breaks, not run together.
	if strings.Contains(msg.BodyText, "First lineSecond line") {
		t.Errorf("BodyText lost paragraph separation: %q", msg.BodyText)
	}
}

func TestParseHTMLOmittedByDefault(t *testing.T) {
	msg, err := Parse([]byte(multipartMixed), ParseOptions{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.BodyHTML != "" {
		t.Errorf("BodyHTML = %q, want empty when IncludeHTML is false", msg.BodyHTML)
	}
}

func TestParseTruncatesBody(t *testing.T) {
	raw := "Subject: Long\r\nContent-Type: text/plain\r\n\r\n" + strings.Repeat("x", 5000)
	msg, err := Parse([]byte(raw), ParseOptions{MaxBodyChars: 100})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len([]rune(msg.BodyText)) != 100 {
		t.Errorf("BodyText length = %d, want 100", len([]rune(msg.BodyText)))
	}
	if !msg.BodyTextTrunc {
		t.Error("BodyTextTrunc = false, want true")
	}
}

func TestExtractAttachmentByPartID(t *testing.T) {
	att, err := ExtractAttachment([]byte(multipartMixed), "2", "", 1<<20)
	if err != nil {
		t.Fatalf("ExtractAttachment: %v", err)
	}
	if string(att.Content) != "Hello PDF" {
		t.Errorf("Content = %q, want %q", att.Content, "Hello PDF")
	}
	if att.Filename != "faktura.pdf" {
		t.Errorf("Filename = %q", att.Filename)
	}
}

func TestExtractAttachmentByFilename(t *testing.T) {
	att, err := ExtractAttachment([]byte(multipartMixed), "", "FAKTURA.PDF", 1<<20)
	if err != nil {
		t.Fatalf("ExtractAttachment: %v", err)
	}
	if string(att.Content) != "Hello PDF" {
		t.Errorf("Content = %q", att.Content)
	}
}

func TestExtractAttachmentRefusesOversize(t *testing.T) {
	_, err := ExtractAttachment([]byte(multipartMixed), "2", "", 4)
	if err == nil {
		t.Fatal("expected an error for an oversized attachment")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Errorf("error = %v, want a size-limit message", err)
	}
}

func TestExtractAttachmentUnknownSelectorListsOptions(t *testing.T) {
	_, err := ExtractAttachment([]byte(multipartMixed), "99", "", 1<<20)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "faktura.pdf") {
		t.Errorf("error = %v, want it to list available parts", err)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":              "report.pdf",
		"../../etc/passwd":        "passwd",
		`..\..\windows\system32`:  "system32",
		"/absolute/path/file.txt": "file.txt",
		"":                        "attachment",
		"..":                      "attachment",
		".":                       "attachment",
		".hidden":                 "hidden",
		"with\x00null.txt":        "with_null.txt",
		"a:b*c?.txt":              "a_b_c_.txt",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilenameBoundsLength(t *testing.T) {
	got := SanitizeFilename(strings.Repeat("a", 500) + ".pdf")
	if len(got) > 200 {
		t.Errorf("length = %d, want <= 200", len(got))
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Errorf("extension lost: %q", got)
	}
}

func TestParseMalformedMessageDoesNotPanic(t *testing.T) {
	for _, raw := range []string{
		"",
		"garbage",
		"Subject: no body\r\n",
		"Content-Type: multipart/mixed; boundary=X\r\n\r\n--X\r\nbroken",
	} {
		if _, err := Parse([]byte(raw), ParseOptions{}); err != nil {
			t.Logf("Parse(%q) returned error (acceptable): %v", raw, err)
		}
	}
}
