package approvals

import "github.com/MavrkAI/Mirrin/internal/tools"

// SafetyFloor names built-in calls that may never run automatically.
// A configured refusal remains a refusal.
// Browser and file tools classify the particular target before policy runs.
func SafetyFloor(name string, risk tools.Risk) string {
	switch name {
	case "check_spend":
		return "payment"
	case "run_shell":
		return "shell command"
	case "browser_act":
		if risk >= tools.RiskDangerous {
			return "payment-looking browser action"
		}
	case "read_file", "write_file", "list_dir":
		if risk >= tools.RiskDangerous {
			return "sensitive file"
		}
	}
	return ""
}
