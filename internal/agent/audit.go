package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The audit log is kept long and read by anyone with the machine, so the
// memory tools leave only ids and sizes there: what the owner told the twin
// to keep (or to forget) is not copied into it. Other tools keep a short
// excerpt of their input.

// privateTools are the tools whose arguments and results are memory content.
var privateTools = map[string]bool{"remember": true, "remember_sensitive": true, "recall": true, "forget": true}

// reFactID finds the ids in a memory tool's result, and only there: recall
// lists "#ID [subject] content" per line, remember says "remembered (#ID"
// and forget "forgot #ID". A "#1234" inside a fact's content is not an id.
var reFactID = regexp.MustCompile(`(?m)^(?:#(\d+) \[|remembered \(#(\d+)|forgot #(\d+))`)

// auditInput is what the audit log keeps of a call: an excerpt of the
// input, or for a memory tool the ids (from its result) and lengths.
func auditInput(tool string, input json.RawMessage, result string, n int) string {
	if !privateTools[tool] {
		return truncate(string(input), n)
	}
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	var parts []string
	for _, k := range []string{"fact", "query"} {
		if s, ok := in[k].(string); ok {
			parts = append(parts, fmt.Sprintf("%s=%d chars", k, utf8.RuneCountInString(s)))
		}
	}
	if id, ok := in["id"].(float64); ok {
		parts = append(parts, fmt.Sprintf("id=#%d", int64(id)))
	}
	var ids []string
	for _, m := range reFactID.FindAllStringSubmatch(result, -1) {
		ids = append(ids, "#"+m[1]+m[2]+m[3])
	}
	if len(ids) > 0 {
		parts = append(parts, "facts="+strings.Join(ids, ","))
	}
	return strings.Join(parts, " ")
}

// auditSummary is what the audit log keeps of an approval's summary.
func auditSummary(tool, summary string, input json.RawMessage) string {
	if privateTools[tool] {
		return tool + " " + auditInput(tool, input, "", 0)
	}
	return truncate(summary, 300)
}
