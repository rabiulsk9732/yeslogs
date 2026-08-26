package notify

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// Alert bodies are authored as plain text, because a mail that cannot be read in
// a terminal or a pager is not much use to whoever is on call at 04:00. But the
// same text arriving as an unstyled wall in a phone mail client reads as machine
// noise, and a report nobody opens is a report nobody acts on.
//
// So the plain text stays the source of truth and this renders a second, HTML
// view of exactly the same content. Nothing is added, dropped or reworded — the
// two parts of the message say the same thing, which is what multipart/alternative
// promises and what a reader comparing them would expect.

// defaultOrg signs reports when the caller sets no organisation.
const defaultOrg = "YesLogs Operations"

// block is one chunk of an alert body: either prose or a preformatted panel.
type block struct {
	pre   bool // authored with an indent: a table, a command, a raw error
	lines []string
}

// splitBlocks groups a plain-text body into paragraphs on blank lines, and marks
// the ones whose every line is indented. Authors use that indent to mean "this
// is laid out, keep it that way" — device tables, runbook steps, raw error text
// — so it is the one piece of formatting intent the plain text already carries,
// and the only signal this needs.
func splitBlocks(body string) []block {
	var out []block
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		pre := true
		for _, l := range cur {
			if !strings.HasPrefix(l, "  ") {
				pre = false
				break
			}
		}
		out = append(out, block{pre: pre, lines: cur})
		cur = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, strings.TrimRight(line, " \t"))
	}
	flush()
	return out
}

// dedent removes the indent the author used to mark the block, while keeping any
// indentation that is real structure — numbered runbook steps, a nested error
// chain. The panel supplies its own padding, so carrying the marker indent
// through as well just pushes every line right for no reason.
func dedent(lines []string) []string {
	common := -1
	for _, l := range lines {
		n := len(l) - len(strings.TrimLeft(l, " "))
		if common < 0 || n < common {
			common = n
		}
	}
	if common <= 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l[common:]
	}
	return out
}

// isSignature reports whether a block is the trailing "— Org" sign-off, which is
// rendered as a signature rather than as body prose.
func isSignature(b block) bool {
	return !b.pre && len(b.lines) <= 2 && strings.HasPrefix(strings.TrimSpace(b.lines[0]), "—")
}

// renderHTML produces an email-safe HTML view of a plain-text alert body.
//
// Deliberately table-free, inline-styled and self-contained: mail clients strip
// <style> blocks, ignore external CSS, and several still lay out with tables
// only. Anything cleverer than this renders differently in every reader, and an
// incident report that looks broken undermines the thing it is reporting.
func renderHTML(org, subject, body string) string {
	if strings.TrimSpace(org) == "" {
		org = defaultOrg
	}
	const (
		ink    = "#1d2733"
		muted  = "#5b6b7d"
		rule   = "#e2e8ee"
		panel  = "#f5f7fa"
		accent = "#c04a2b"
	)
	var b strings.Builder
	fmt.Fprintf(&b, `<div style="margin:0;padding:24px 12px;background:#eef1f5">`+
		`<div style="max-width:680px;margin:0 auto;background:#ffffff;border:1px solid %s;border-radius:6px;overflow:hidden">`+
		`<div style="border-top:3px solid %s;padding:18px 24px 14px">`+
		`<div style="font:600 15px/1.4 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:%s">%s</div>`+
		`</div>`+
		`<div style="padding:2px 24px 8px;font:400 14px/1.65 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:%s">`,
		rule, accent, ink, html.EscapeString(subject), ink)

	var sig []string
	for _, blk := range splitBlocks(body) {
		if isSignature(blk) {
			sig = blk.lines
			continue
		}
		if blk.pre {
			// Preserve the author's layout exactly, and let it scroll rather than
			// wrap: a wrapped command or error string is one someone will retype
			// wrong.
			fmt.Fprintf(&b, `<pre style="margin:14px 0;padding:12px 14px;background:%s;border-left:3px solid %s;`+
				`border-radius:4px;font:400 12.5px/1.6 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;`+
				`color:%s;white-space:pre;overflow-x:auto">%s</pre>`,
				panel, rule, ink, html.EscapeString(strings.Join(dedent(blk.lines), "\n")))
			continue
		}
		// Prose is authored hard-wrapped for the terminal; rejoin it so the mail
		// client can wrap to the reader's own width instead of keeping 88-column
		// breaks that look wrong on a phone.
		trimmed := make([]string, len(blk.lines))
		for i, l := range blk.lines {
			trimmed[i] = strings.TrimSpace(l)
		}
		fmt.Fprintf(&b, `<p style="margin:12px 0">%s</p>`,
			emphasise(html.EscapeString(strings.Join(trimmed, " "))))
	}

	fmt.Fprintf(&b, `</div><div style="margin:6px 24px 0;border-top:1px solid %s"></div>`+
		`<div style="padding:14px 24px 20px;font:400 12.5px/1.6 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:%s">`,
		rule, muted)
	if len(sig) > 0 {
		fmt.Fprintf(&b, `<div style="color:%s;font-weight:600">%s</div>`, ink,
			html.EscapeString(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sig[0]), "—"))))
		for _, l := range sig[1:] {
			fmt.Fprintf(&b, `<div>%s</div>`, html.EscapeString(strings.TrimSpace(l)))
		}
	} else {
		fmt.Fprintf(&b, `<div style="color:%s;font-weight:600">%s</div>`, ink, html.EscapeString(org))
	}
	b.WriteString(`<div style="margin-top:4px">Automated report — natlog. Replies to this address are not monitored.</div>`)
	b.WriteString(`</div></div></div>`)
	return b.String()
}

// shouted matches the all-caps words alert bodies use to mark the thing that
// actually went wrong. They carry the weight of the sentence in plain text, and
// without this the HTML view loses that emphasis entirely.
//
// One pass with word boundaries, not repeated ReplaceAll: replacing "CANNOT" and
// then "NOT" rewrites the tags the first pass just inserted, and the reader gets
// "CAN NOT" split across two broken elements. Longest alternative first, since Go
// regexp alternation is leftmost-first rather than leftmost-longest.
var shouted = regexp.MustCompile(`\b(?:NOTHING|CANNOT|ALSO|NOT)\b`)

func emphasise(escaped string) string {
	return shouted.ReplaceAllString(escaped, `<strong>$0</strong>`)
}
