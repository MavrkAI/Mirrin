package approvals

import "testing"

func TestParseReply(t *testing.T) {
	cases := []struct {
		text    string
		ok      bool
		approve bool
		id      int64
	}{
		// What the old exact-match rule already took.
		{"yes", true, true, 0},
		{"yes 12", true, true, 12},
		{"no 12", true, false, 12},
		{"y", true, true, 0},
		{"n", true, false, 0},
		{"cancel", true, false, 0},
		{"approve #7", true, true, 7},
		// Natural replies it dropped on the floor.
		{"Yes.", true, true, 0},
		{"Yes, please.", true, true, 0},
		{"Okay.", true, true, 0},
		{"No.", true, false, 0},
		{"Sure", true, true, 0},
		{"ok, go ahead", true, true, 0},
		{"Yeah, sure!", true, true, 0},
		{"go for it", true, true, 0},
		{"Do it.", true, true, 0},
		{"no thanks", true, false, 0},
		{"Don't.", true, false, 0},
		{"don’t do it", true, false, 0},
		{"nope", true, false, 0},
		{"👍", true, true, 0},
		{"👍🏽", true, true, 0},
		{"✅ 12", true, true, 12},
		{"❌", true, false, 0},
		{"Yes #12 please", true, true, 12},
		{"yes to 12", true, true, 12},
		{"yes # 12", true, true, 12},
		{"YES 12!", true, true, 12},
		{"yes12", true, true, 12},
		{"y12", true, true, 12},
		{"no12", true, false, 12},
		{"N4", true, false, 4},
		{"ok #3", true, true, 3},
		// Not decisions: they carry more than a yes or a no.
		{"", false, false, 0},
		{"12", false, false, 0},
		{"please", false, false, 0},
		{"thanks", false, false, 0},
		{"yes, but tomorrow", false, false, 0},
		{"no, I meant the other one", false, false, 0},
		{"yes no", false, false, 0},
		{"yes 12 13", false, false, 0},
		{"yes 0", false, false, 0},
		{"room12", false, false, 0},
		{"no2seats", false, false, 0},
		{"what?", false, false, 0},
		{"stop nudging", false, false, 0},
		{"go away", false, false, 0},
		{"ok ok ok ok", false, false, 0},
	}
	for _, c := range cases {
		r, ok := ParseReply(c.text)
		if ok != c.ok || (ok && (r.Approve != c.approve || r.ID != c.id)) {
			t.Errorf("ParseReply(%q) = %+v, %v; want approve=%v id=%d ok=%v", c.text, r, ok, c.approve, c.id, c.ok)
		}
	}
}

func TestParseReplyIgnoresTheTwinsName(t *testing.T) {
	if r, ok := ParseReply("Yes, Mirrin.", "Mirrin"); !ok || !r.Approve {
		t.Fatalf("got %+v %v", r, ok)
	}
	if _, ok := ParseReply("Yes, Mirrin."); ok {
		t.Fatal("an unknown word should make it more than a decision")
	}
}
