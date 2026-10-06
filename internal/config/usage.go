package config

// Usage is what the twin tells you about what its model costs. Amounts are
// estimates in US dollars, from list prices built into Mirrin unless you set
// your own here; your provider's bill is what counts.
type Usage struct {
	// MonthlyBudget is roughly what you're happy to spend on the model each
	// month, in US dollars. The twin warns at 80% and when it's reached (on
	// Health, in the menu and in `mirrin doctor`). 0 turns the warning off.
	MonthlyBudget float64 `yaml:"monthly_budget"`
	// Prices overrides or adds model prices, in US dollars per million
	// tokens, keyed by model id ("claude-opus-5") or provider/model
	// ("custom/my-model").
	Prices map[string]ModelPrice `yaml:"prices,omitempty"`
}

// ModelPrice is one model's price in US dollars per million tokens. Cache
// prices left at 0 are taken as a tenth (reads) and 1.25 times (writes) the
// input price.
type ModelPrice struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read,omitempty"`
	CacheWrite float64 `yaml:"cache_write,omitempty"`
}

// Retention is how long the twin keeps its activity log and bulky tool
// output. Facts, reminders and your conversations are never removed by it.
type Retention struct {
	// AuditDays deletes activity-log entries older than this many days.
	// 0 keeps them forever.
	AuditDays int `yaml:"audit_days"`
	// TrimAfterDays shortens, after this many days, the activity log's quotes
	// of messages and tool inputs and long tool results kept in chats.
	// 0 never trims.
	TrimAfterDays int `yaml:"trim_after_days"`
}

// Defaults for a config that doesn't say.
const (
	DefaultMonthlyBudget = 25
	DefaultAuditDays     = 90
	DefaultTrimAfterDays = 30
)
