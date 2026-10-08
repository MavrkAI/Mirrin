package agent

import (
	"slices"
	"strings"
)

// BillsRunMarker is in the chat key of a run that reads a bill that has
// just arrived in the mail (daemon/bills.go). The email was written by
// someone else, so that run, and any approval it raises, may only read mail
// and set reminders: it can't pay, browse, follow a link in the email or
// send anything, whatever the email says. The marker starts with "#watch-",
// so the untrusted policy applies too (always_allow doesn't cover it).
const BillsRunMarker = "#watch-bills-"

// billsTools are the only tools a bills run may use.
var billsTools = []string{"list_emails", "read_email", "gmail_search", "gmail_read", "set_reminder", "list_reminders"}

// outOfScope reports whether tool is closed to the run in chatKey.
func outOfScope(chatKey, tool string) bool {
	return strings.Contains(chatKey, BillsRunMarker) && !slices.Contains(billsTools, tool)
}
