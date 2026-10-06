package daemon

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/memory"
)

// A bare call's card lists its arguments as labelled lines, whole; a
// summary the tool wrote in words needs none.
func TestABareApprovalShowsItsDetails(t *testing.T) {
	ap := memory.Approval{Tool: "send_sms", Summary: "send_sms(body=See you at 6, to=+61400000000)",
		Input: []byte(`{"to":"+61400000000","body":"See you at 6","retry_count":2,"empty":"","extra":{"a":1}}`)}
	got := approvalDetails(ap)
	want := []ApprovalDetail{{"Body", "See you at 6"}, {"Extra", `{"a":1}`}, {"Retry count", "2"}, {"To", "+61400000000"}}
	if len(got) != len(want) {
		t.Fatalf("details %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("details %v, want %v", got, want)
		}
	}
	ap.Summary = "Pay 389.00 AUD for Jetstar"
	if d := approvalDetails(ap); d != nil {
		t.Fatalf("worded summary got details %v", d)
	}
}
