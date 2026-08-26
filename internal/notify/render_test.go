package notify

import (
	"strings"
	"testing"
)

const sampleBody = `Dataplane "natlog-1" is receiving flows but NOTHING is being written to ClickHouse.

Over the last 15 minutes: decoded 41234 flows, inserted 0, failed batches 3.

Last insert error (2026-08-26 04:24:17 IST):
  Code: 252. DB::Exception: Too many parts

Check: systemctl status natlog clickhouse-server

— YesLogs Operations`

// The two views must carry the same content. An HTML part that quietly drops a
// section makes the formatted view a lie for anyone who only reads that one.
func TestHTMLKeepsEveryFactFromThePlainText(t *testing.T) {
	got := renderHTML("", "Ingest stalled", sampleBody)
	for _, want := range []string{
		"natlog-1", "41234", "failed batches 3",
		"2026-08-26 04:24:17 IST", "Too many parts",
		"systemctl status natlog clickhouse-server",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("HTML view lost %q", want)
		}
	}
}

// An indented block is the author saying "this is laid out". Reflowing a command
// or a raw error is how someone ends up retyping it wrong.
func TestIndentedBlocksStayPreformatted(t *testing.T) {
	got := renderHTML("", "s", "intro line\n\n  step one\n  step two\n")
	if !strings.Contains(got, "<pre") {
		t.Fatal("indented block was not rendered preformatted")
	}
	if !strings.Contains(got, "step one\nstep two") {
		t.Error("preformatted block lost its line breaks")
	}
}

// Prose is authored hard-wrapped for an 80-column terminal. Keeping those breaks
// in HTML makes it read as ragged on a phone, so it is rejoined.
func TestProseIsRejoined(t *testing.T) {
	got := renderHTML("", "s", "a sentence that was\nwrapped by the author")
	if !strings.Contains(got, "a sentence that was wrapped by the author") {
		t.Errorf("prose was not rejoined:\n%s", got)
	}
}

// The body's own sign-off wins; the Org default is only for bodies without one.
func TestSignatureFromBodyWins(t *testing.T) {
	got := renderHTML("Ignored Org", "s", "text\n\n— YesLogs Operations")
	if !strings.Contains(got, "YesLogs Operations") {
		t.Error("body sign-off missing")
	}
	if strings.Contains(got, "Ignored Org") {
		t.Error("Org overrode the body's own sign-off")
	}
	if strings.Contains(got, "— YesLogs") {
		t.Error("the em dash marker should not be rendered in the signature block")
	}
}

func TestDefaultOrgWhenBodyHasNoSignature(t *testing.T) {
	if got := renderHTML("", "s", "just text"); !strings.Contains(got, defaultOrg) {
		t.Errorf("expected default org %q\n%s", defaultOrg, got)
	}
}

// Content is attacker-influenced (device names, raw ClickHouse errors). None of
// it may reach the reader as live markup.
func TestBodyIsEscaped(t *testing.T) {
	got := renderHTML("", "s", `device <script>alert(1)</script> & "quoted"`)
	if strings.Contains(got, "<script>") {
		t.Error("body markup was not escaped")
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Error("escaped form missing")
	}
}

// Replacing shouted words one at a time rewrites the markup an earlier pass just
// inserted: CANNOT becomes CAN<strong>NOT</strong>, which a reader sees as
// "CAN NOT" — the opposite of the sentence's meaning.
func TestShoutedWordsAreNotNested(t *testing.T) {
	got := renderHTML("", "s", "those records CANNOT answer a lawful request")
	if !strings.Contains(got, "<strong>CANNOT</strong>") {
		t.Errorf("CANNOT not emphasised as one word:\n%s", got)
	}
	if strings.Contains(got, "CAN<strong>") || strings.Contains(got, "<strong>CAN<") {
		t.Errorf("CANNOT was split by a nested replacement:\n%s", got)
	}
}

// Word boundaries matter: NOT inside another word must be left alone.
func TestEmphasisRespectsWordBoundaries(t *testing.T) {
	got := renderHTML("", "s", "NOTIFICATION and ANOTHER and NOTHING")
	if strings.Contains(got, "<strong>NOT</strong>IFICATION") || strings.Contains(got, "A<strong>NOT</strong>HER") {
		t.Errorf("emphasis leaked into a longer word:\n%s", got)
	}
	if !strings.Contains(got, "<strong>NOTHING</strong>") {
		t.Error("NOTHING should still be emphasised")
	}
}
