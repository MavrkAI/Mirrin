package daemon

import "testing"

func TestTaskUpdatesAreSaidLikeAPerson(t *testing.T) {
	title := "Shop for men's clothing"
	cases := []struct {
		text   string
		others int
		want   string
	}{
		{title + ": Cart's open on screen, sir. Shall I go to checkout?", 0, "Cart's open on screen, sir. Shall I go to checkout?"},
		{title + ": Cart's open.", 1, "On shop for men's clothing: Cart's open."},
		{title + `: I need your OK. Pay 349.30 AUD for Uniqlo - 7 items Reply "yes 21" or "no 21".`, 0, "I need your OK to pay 349.30 AUD for Uniqlo - 7 items. Shall I go ahead?"},
		{"Approval #21 is waiting.", 0, "Approval is waiting."},
		{"No title here.", 0, "No title here."},
	}
	for _, c := range cases {
		if got := spokenUpdate(c.text, title, c.others); got != c.want {
			t.Errorf("spokenUpdate(%q)\n got %q\nwant %q", c.text, got, c.want)
		}
	}
}
