package mailtext

import (
	"strings"
	"testing"
)

func TestDecodeUndoesTransferEncodingsAndCharsets(t *testing.T) {
	cases := []struct {
		name, data, enc, cs, want string
	}{
		{"qp utf-8", "Caf=C3=A9 at 10 =E2=80=94 see you=\r\n there", "quoted-printable", "utf-8", "Café at 10 — see you there"},
		{"qp latin-1", "Gr=FC=DFe aus M=FCnchen", "quoted-printable", "iso-8859-1", "Grüße aus München"},
		{"qp stray equals kept", "2+2=4 and a=b\n", "quoted-printable", "utf-8", "2+2=4 and a=b\n"},
		{"base64 with line breaks", "SGVsbG8s\r\nIHdvcmxk\r\nIQ==\r\n", "base64", "", "Hello, world!"},
		{"base64 cut short", "SGVsbG8sIHdvcmxkIQ", "base64", "", "Hello, world!"},
		{"windows-1252 smart quotes", "\x93Hi\x94 \x96 caf\xe9", "8bit", "windows-1252", "“Hi” – café"},
		{"mislabelled latin-1 as utf-8", "caf\xe9", "8bit", "utf-8", "café"},
		{"iso-2022-jp", "\x1b$B$3$s$K$A$O\x1b(B", "7bit", "iso-2022-jp", "こんにちは"},
		{"unknown charset keeps utf-8", "héllo", "8bit", "x-made-up", "héllo"},
	}
	for _, c := range cases {
		if got := Decode([]byte(c.data), c.enc, c.cs); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHTMLTextReadsLikeTheEmail(t *testing.T) {
	in := `<html><head><style>p{color:red}</style><title>x</title></head><body>
<div style="display:none;max-height:0">Preview text you never see &zwnj;&nbsp;&zwnj;&nbsp;</div>
<!--[if mso]><table><tr><td>outlook only</td></tr></table><![endif]-->
<p>Hi Sam,</p><p>Your flight JQ&nbsp;501 moved to <b>10:40</b> &mdash; gate 7.</p>
<ul><li>Bags: 20&#8239;kg</li><li>Seat: 14C</li></ul>
<img src="https://track.example/pixel.gif" width="1" height="1">
<p>Thanks &amp; safe travels<br>Jetstar</p></body></html>`
	got := HTMLText(in)
	for _, want := range []string{"Hi Sam,", "Your flight JQ 501 moved to 10:40 — gate 7.", "• Bags: 20", "• Seat: 14C", "Thanks & safe travels\nJetstar"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Preview text", "outlook only", "color:red", "<", "&" + "zwnj;", "pixel"} {
		if strings.Contains(got, bad) {
			t.Errorf("kept %q in:\n%s", bad, got)
		}
	}
}

func TestCleanDropsQuotedHistoryAndSignatures(t *testing.T) {
	cases := []struct {
		name, in, want    string
		quoted, signature int
	}{
		{"gmail reply", "Thursday works.\n\nOn Mon, 3 Oct 2026 at 10:02, Sarah Smith <sarah@example.com> wrote:\n> Lunch this week?\n> S", "Thursday works.", 3, 0},
		{"wrapped attribution", "Yes please.\n\nOn Mon, 3 Oct 2026 at 10:02, Sarah Smith <\nsarah@example.com> wrote:\n\n> Want the blue one?", "Yes please.", 3, 0},
		{"outlook reply", "Approved.\n\n________________________________\nFrom: Finance <finance@example.com>\nSent: Monday, 3 October 2026 9:00 AM\nTo: Me <me@example.com>\nSubject: Invoice 4411\n\nPlease approve.", "Approved.", 6, 0},
		{"outlook header block without rule", "See below.\n\nFrom: Jo <jo@example.com>\nSent: Monday\nTo: Me\nSubject: Hi\n\nOld text", "See below.", 5, 0},
		{"original message", "Done!\n-----Original Message-----\nFrom: x\nplease do it", "Done!", 3, 0},
		{"german", "Passt.\n\nAm 03.10.2026 um 10:02 schrieb Anna <anna@example.de>:\n> Morgen?", "Passt.", 2, 0},
		{"french", "D'accord.\n\nLe lun. 3 oct. 2026 à 10:02, Anne <anne@example.fr> a écrit :\n> Demain ?", "D'accord.", 2, 0},
		{"trailing quote", "Sounds good.\n\n> Can you do 3pm?\n> Thanks", "Sounds good.", 2, 0},
		{"signature", "Running 5 min late.\n\n-- \nSam Smith\nHead of Things\n+61 400 000 000", "Running 5 min late.", 0, 3},
		{"short signature under a bare dash", "See you soon.\n--\nSam Smith\n+61 400 000 000", "See you soon.", 0, 2},
		{"device line", "On my way\n\nSent from my iPhone", "On my way", 0, 1},
		{"inline reply keeps both", "> Can you do Tuesday?\nYes, after 2.\n> And bring the forms?\nWill do.", "> Can you do Tuesday?\nYes, after 2.\n> And bring the forms?\nWill do.", 0, 0},
		{"forward kept whole", "FYI, see below.\n\n---------- Forwarded message ---------\nFrom: Jetstar <noreply@jetstar.com>\nDate: Mon, 3 Oct 2026\nSubject: Flight change\nTo: <me@example.com>\n\nYour flight moved to 10:40.", "FYI, see below.\n\n---------- Forwarded message ---------\nFrom: Jetstar <noreply@jetstar.com>\nDate: Mon, 3 Oct 2026\nSubject: Flight change\nTo: <me@example.com>\n\nYour flight moved to 10:40.", 0, 0},
		{"only a quote stays", "On Mon, Sarah wrote:\n> hello", "On Mon, Sarah wrote:\n> hello", 0, 0},
		{"sentence starting with On is kept", "On Tuesday I'll bring the forms I wrote:\nthe tax ones.\nThanks", "On Tuesday I'll bring the forms I wrote:\nthe tax ones.\nThanks", 0, 0},

		// Answers typed between the quoted lines, under Gmail's attribution:
		// the answers are the reply. Only the quote left at the end goes.
		{"inline reply under an attribution",
			"See my answers below.\n\nOn Mon, 3 Oct 2026 at 10:02, Sarah <s@x.com> wrote:\n> Can you do Thursday?\nYes, Thursday works.\n> And bring the forms?\nWill do.\n> Thanks!\n> Sarah",
			"See my answers below.\n\nOn Mon, 3 Oct 2026 at 10:02, Sarah <s@x.com> wrote:\n> Can you do Thursday?\nYes, Thursday works.\n> And bring the forms?\nWill do.", 2, 0},
		// Ordinary sentences that start with "On" and end in "wrote" are
		// writing, not an attribution.
		{"sentence ending in wrote",
			"Thanks for the notes.\n\nOn reflection I think we should keep what I wrote\nThe budget is 5k and the date stays.",
			"Thanks for the notes.\n\nOn reflection I think we should keep what I wrote\nThe budget is 5k and the date stays.", 0, 0},
		{"sentence quoting someone",
			"We talked it over.\nOn Monday the plan changed, as Sarah wrote:\n> Let's move it to Friday.\nSo Friday it is, 10am.",
			"We talked it over.\nOn Monday the plan changed, as Sarah wrote:\n> Let's move it to Friday.\nSo Friday it is, 10am.", 0, 0},
		{"sentence quoting someone, with a time in it",
			"We talked it over.\nOn Monday at 10 the plan changed, as Sarah wrote:\n> Let's move it to Friday.\nSo Friday it is.",
			"We talked it over.\nOn Monday at 10 the plan changed, as Sarah wrote:\n> Let's move it to Friday.\nSo Friday it is.", 0, 0},
		{"quote introduced by the writer, at the end",
			"We talked it over.\nOn Monday the plan changed, as Sarah wrote:\n> Let's move it to Friday.",
			"We talked it over.\nOn Monday the plan changed, as Sarah wrote:\n> Let's move it to Friday.", 0, 0},
		// A bare "--" is also a dash people write; a list under it stays.
		{"list under a dash", "Here are the steps:\n--\n1. do x\n2. do y\n\nthanks", "Here are the steps:\n--\n1. do x\n2. do y\n\nthanks", 0, 0},
		{"writing under a dash", "Quick update.\n--\nThe venue moved to the bigger room on level two.\nBring your badge.", "Quick update.\n--\nThe venue moved to the bigger room on level two.\nBring your badge.", 0, 0},
	}
	for _, c := range cases {
		got, quoted, signature := Clean(c.in)
		if got != c.want || quoted != c.quoted || signature != c.signature {
			t.Errorf("%s: got %q (quoted %d, signature %d), want %q (quoted %d, signature %d)", c.name, got, quoted, signature, c.want, c.quoted, c.signature)
		}
	}
	if !IsForward("Fwd: Flight change") || !IsForward("FW: invoice") || IsForward("Re: lunch") {
		t.Error("IsForward")
	}
}

// Whatever is left out is said: the reader always knows to ask for the rest.
func TestRenderSaysWhenTheSignatureIsLeftOut(t *testing.T) {
	out := Render("Running late.\n\n-- \nSam Smith\nHead of Things", nil, false, 0)
	if !strings.Contains(out, "Running late.") || strings.Contains(out, "Head of Things") || !strings.Contains(out, "[signature left out; read again with full: true to see it]") {
		t.Errorf("signature dropped silently:\n%s", out)
	}
	out = Render("Yes.\n\n-- \nSam\n\nOn Mon, 3 Oct 2026, Jo <jo@example.com> wrote:\n> ok?", nil, false, 0)
	if !strings.Contains(out, "earlier messages in this thread left out (2 lines), and the signature") {
		t.Errorf("both left out, not both said:\n%s", out)
	}
	if out := Render("Just this.", nil, false, 0); strings.Contains(out, "[") {
		t.Errorf("a note with nothing left out: %s", out)
	}
}

func TestRenderNamesAttachmentsAndSaysWhatWasLeftOut(t *testing.T) {
	out := Render("Invoice attached.\n\nOn Mon, Jo <jo@example.com> wrote:\n> send it\n> please", []Attachment{
		{Name: "invoice-4411.pdf", Type: "application/pdf", Size: 245_000},
		{Name: "photo.jpg", Type: "image/jpeg", Size: 3_400_000},
	}, false, 20000)
	for _, want := range []string{"Invoice attached.", "earlier messages in this thread left out (3 lines)", "full: true", "Attachments: invoice-4411.pdf (PDF, 239 KB); photo.jpg (image, 3.2 MB)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "send it") {
		t.Errorf("quoted history kept:\n%s", out)
	}
	if full := Render("Invoice attached.\n\nOn Mon, Jo <jo@example.com> wrote:\n> send it", nil, true, 0); !strings.Contains(full, "> send it") {
		t.Errorf("full lost the thread: %s", full)
	}
	if got := Truncate("héllo wörld", 2); got != "h\n…[truncated]" {
		t.Errorf("truncate split a character: %q", got)
	}
}

func TestHeaderAndFilenameDecoding(t *testing.T) {
	if got := Header("=?ISO-8859-1?Q?Gr=FC=DFe?= aus =?UTF-8?B?TcO8bmNoZW4=?="); got != "Grüße aus München" {
		t.Errorf("header: %q", got)
	}
	if got := Header("=?windows-1251?B?z/Do4uXy?="); got != "Привет" {
		t.Errorf("cyrillic header: %q", got)
	}
	if got := Filename(map[string]string{"filename*": "UTF-8''Rechnung%20M%C3%A4rz.pdf"}); got != "Rechnung März.pdf" {
		t.Errorf("rfc 2231 name: %q", got)
	}
	if got := Filename(map[string]string{"name": "=?UTF-8?Q?Re=C3=A7u.pdf?="}); got != "Reçu.pdf" {
		t.Errorf("encoded name: %q", got)
	}
}
