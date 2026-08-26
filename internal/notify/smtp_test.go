package notify

import (
	"strings"
	"testing"
)

func TestSplitRecipients(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a@x.io", []string{"a@x.io"}},
		{"a@x.io, b@x.io", []string{"a@x.io", "b@x.io"}},
		{"a@x.io\nb@x.io\r\nc@x.io", []string{"a@x.io", "b@x.io", "c@x.io"}},
		{" a@x.io ; b@x.io ", []string{"a@x.io", "b@x.io"}},
		{"a@x.io, a@x.io, b@x.io", []string{"a@x.io", "b@x.io"}}, // dedupe
		{"  ,  ,  ", nil},
	}
	for _, c := range cases {
		got := SplitRecipients(c.in)
		if len(got) != len(c.want) {
			t.Errorf("SplitRecipients(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitRecipients(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestBuildMessageHeaderInjection(t *testing.T) {
	// A device name carrying CRLF must not be able to inject headers (e.g. Bcc).
	msg := string(buildMessage(
		"from@x.io\r\nBcc: evil@x.io",
		[]string{"a@x.io\r\nBcc: evil2@x.io"},
		"DEVICE DOWN\r\nBcc: evil3@x.io",
		"body line",
		"",
	))
	headerPart := msg
	if i := strings.Index(msg, "\r\n\r\n"); i >= 0 {
		headerPart = msg[:i]
	}
	// The real security property: no CRLF can introduce a NEW header line. The
	// injected "Bcc:" must survive only as inert text within a value, never at a
	// line start.
	if strings.Contains(msg, "\nBcc:") || strings.Contains(msg, "\rBcc:") {
		t.Errorf("header injection: Bcc smuggled as a header line\n%s", msg)
	}
	// Exactly 6 header lines (From,To,Subject,Date,MIME-Version,Content-Type) =
	// 5 internal CRLFs in the header block — no injected extras. The multipart
	// Content-Type counts as the sixth; part headers live below the blank line.
	if n := strings.Count(headerPart, "\r\n"); n != 5 {
		t.Errorf("unexpected header line count: %d CRLFs in header block\n%s", n, headerPart)
	}
}

func TestBuildMessageStructure(t *testing.T) {
	msg := string(buildMessage("from@x.io", []string{"a@x.io", "b@x.io"}, "Subject line", "line1\nline2", ""))
	for _, want := range []string{
		"From: from@x.io\r\n",
		"To: a@x.io, b@x.io\r\n",
		"Subject: Subject line\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: multipart/alternative; boundary=\"" + mimeBoundary + "\"\r\n",
		"\r\n\r\n", // blank line separating headers from the first part
		"Content-Type: text/plain; charset=UTF-8\r\n",
		"line1\r\nline2",
		"Content-Type: text/html; charset=UTF-8\r\n",
		"--" + mimeBoundary + "--\r\n", // closing delimiter
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n--- full ---\n%s", want, msg)
		}
	}
}

// A reader that shows the last displayable part must land on the HTML one, and
// a text-only reader must still get the entire report rather than markup. That
// is the whole contract of multipart/alternative, and it depends on part order.
func TestPlainTextPartComesBeforeHTML(t *testing.T) {
	msg := string(buildMessage("f@x.io", []string{"a@x.io"}, "Subj", "hello world", ""))
	plain := strings.Index(msg, "Content-Type: text/plain")
	htmlAt := strings.Index(msg, "Content-Type: text/html")
	if plain < 0 || htmlAt < 0 {
		t.Fatalf("both parts must be present (plain=%d html=%d)", plain, htmlAt)
	}
	if plain > htmlAt {
		t.Error("plain text must come first so HTML wins for clients that can show it")
	}
	if !strings.Contains(msg, "hello world") {
		t.Error("plain part lost the body")
	}
}

// A CRLF smuggled through a device name must not break the rendered heading or
// place attacker-controlled text where a title belongs. It cannot inject a
// header from inside a MIME part, but it must still not survive as a line break.
func TestHTMLTitleIsSanitised(t *testing.T) {
	msg := string(buildMessage("f@x.io", []string{"a@x.io"},
		"DEVICE DOWN\r\nBcc: evil@x.io", "body", ""))
	i := strings.Index(msg, "Content-Type: text/html")
	if i < 0 {
		t.Fatal("no html part")
	}
	if strings.Contains(msg[i:], "\r\nBcc:") || strings.Contains(msg[i:], "\nBcc:") {
		t.Errorf("subject CRLF survived into the HTML part\n%s", msg[i:])
	}
}
