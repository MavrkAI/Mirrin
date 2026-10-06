// Package config loads and validates the Mirrin configuration file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/push"
)

// Config is the top-level configuration, loaded from ~/.mirrin/config.yaml.
type Config struct {
	// Name is what the user calls their twin (any name; the persona supplies the character).
	Name string `yaml:"name"`
	// Persona is the personality id (bundled: mirrin, nyra, pickoo, plain; or one in ~/.mirrin/personas).
	Persona string `yaml:"persona"`
	User    User   `yaml:"user"`
	LLM     LLM    `yaml:"llm"`

	Channels Channels `yaml:"channels"`
	Autonomy Autonomy `yaml:"autonomy"`
	Spending Spending `yaml:"spending"`
	Phone    Phone    `yaml:"phone"`
	Skills   Skills   `yaml:"skills"`

	// MCP lists Model Context Protocol servers whose tools the twin can use.
	MCP MCP `yaml:"mcp"`
	// Watch makes the twin notice calendar and inbox changes between heartbeats.
	Watch Watch `yaml:"watch"`
	// UI configures the presence screen served at /ui.
	UI   UI          `yaml:"ui"`
	Push push.Config `yaml:"push,omitempty"`
	// Usage is the model's estimated cost and your monthly budget for it (usage.go).
	Usage Usage `yaml:"usage"`
	// Retention is how long the activity log and bulky tool output are kept (usage.go).
	Retention Retention `yaml:"retention"`
	// API is the loopback control socket used by `mirrin chat`, `voice` and the tray.
	API   API   `yaml:"api"`
	Reach Reach `yaml:"reach"`
	// Tray shows a menu bar / system tray icon when running under the service.
	Tray bool `yaml:"tray"`
	// Backup configures encrypted backups (backup.go).
	Backup Backup `yaml:"backup,omitempty"`
	// Cloud is the optional paid availability layer; nothing is contacted until `mirrin cloud link`.
	Cloud Cloud `yaml:"cloud,omitempty"`

	// ProtocolsDir holds user-defined protocol YAML files (and packs/ beneath it).
	ProtocolsDir string `yaml:"protocols_dir"`
	// ToolsDir holds custom script tools (one folder each with tool.yaml).
	ToolsDir string `yaml:"tools_dir"`
	// ProtocolRegistry is the community index URL for `mirrin protocols search`.
	ProtocolRegistry string `yaml:"protocol_registry"`
	// DataDir holds the SQLite databases and session state.
	DataDir string `yaml:"data_dir"`
}

// MCP configures Model Context Protocol servers.
type MCP struct {
	Servers []MCPServer `yaml:"servers"`
}

// MCPServer is one stdio MCP server to launch.
type MCPServer struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	// Risk is the default risk for the server's tools: read, write (default) or dangerous.
	Risk string `yaml:"risk"`
	// ToolRisk overrides risk per tool name.
	ToolRisk map[string]string `yaml:"tool_risk"`
	Enabled  *bool             `yaml:"enabled"`
}

// Watch configures change detection.
type Watch struct {
	Enabled         bool `yaml:"enabled"`
	IntervalMinutes int  `yaml:"interval_minutes"`
	Calendar        bool `yaml:"calendar"`
	Inbox           bool `yaml:"inbox"`
}

// UI configures the presence screen.
type UI struct {
	// Weather coordinates for the ambient mode (Open-Meteo, no key). Empty
	// uses the city your time zone is named for.
	Latitude  float64 `yaml:"latitude"`
	Longitude float64 `yaml:"longitude"`
	// Weather shows current conditions on the ambient screen (default on).
	Weather *bool `yaml:"weather"`
	// AmbientAfterSeconds is how long after the last activity the screen goes ambient (default 60).
	AmbientAfterSeconds int `yaml:"ambient_after_seconds"`
	// Orb shows the small floating orb on the desktop (macOS; default on).
	Orb *bool `yaml:"orb"`
}

type Reach struct {
	Mode        string `yaml:"mode"`
	Listen      string `yaml:"listen,omitempty"`
	CertFile    string `yaml:"cert_file,omitempty"`
	KeyFile     string `yaml:"key_file,omitempty"`
	AdminRemote bool   `yaml:"admin_remote"`
	StepUp      string `yaml:"step_up,omitempty"`
	StayAwake   bool   `yaml:"stay_awake,omitempty"`
	// Relay mode (your own mirrin-relay; internal/reach/relay.go).
	RelayURL      string `yaml:"relay_url,omitempty"`      // wss://relay.example.com/v1/tunnel
	Hostname      string `yaml:"hostname,omitempty"`       // the public name the relay routes here
	RelayCAFile   string `yaml:"relay_ca_file,omitempty"`  // extra roots for the relay's own certificate
	ACMEDirectory string `yaml:"acme_directory,omitempty"` // default Let's Encrypt
	ACMEEmail     string `yaml:"acme_email,omitempty"`
	// Cloud mode (internal/reach/cloud.go): the free mode, tailscale or
	// files, that serves while the paid address can't (expired, unlinked).
	Fallback string `yaml:"fallback,omitempty"`
}

// API configures the control socket.
type API struct {
	// Listen is host:port; empty disables the socket. Loopback by default.
	Listen string `yaml:"listen"`
	// Remote allows a non-loopback Listen so your other devices can reach the
	// twin. Each device signs in with its own key, given when you pair it and
	// taken back with /revoke; nothing is shared. Prefer this machine's
	// Tailscale address (100.x.y.z:7742) in api.listen, or a trusted LAN:
	// the API is plain HTTP.
	Remote bool `yaml:"remote"`
}

// Cloud configures Mirrin Cloud. It is empty by default, and nothing is
// contacted until the user runs `mirrin cloud link`.
type Cloud struct {
	// API is the control plane `mirrin cloud link` uses, as https://host[:port]
	// (a self-hosted mirrin-cloud, say). Empty means the built-in address. A
	// linked machine keeps using the one it linked with.
	API string `yaml:"api,omitempty"`
}

// Remote is a saved connection to a twin running elsewhere (~/.mirrin/remote.yaml).
type Remote struct {
	Address string `yaml:"address"`
	Token   string `yaml:"token"`
	Name    string `yaml:"name"`
}

// RemotePath is where the saved remote connection lives.
func RemotePath() string { return filepath.Join(Home(), "remote.yaml") }

// LoadRemote returns the saved remote, or nil.
func LoadRemote() *Remote {
	b, err := os.ReadFile(RemotePath())
	if err != nil {
		return nil
	}
	var r Remote
	if yaml.Unmarshal(b, &r) != nil || r.Address == "" || r.Token == "" {
		return nil
	}
	return &r
}

// SaveRemote stores a remote connection.
func SaveRemote(r Remote) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(RemotePath(), b, 0o600)
}

// User describes the person the agent works for.
type User struct {
	Name string `yaml:"name"`
	// Honorific is how the twin addresses the user ("sir", "ma'am", their
	// name). Empty is the persona's own way: "sir" for Mirrin, the user's
	// first name for the others.
	Honorific string `yaml:"honorific"`
	Timezone  string `yaml:"timezone"`
	// About is free-form context the agent should always know.
	About string `yaml:"about"`
	// QuietHours are when what can wait (the watcher's news, an idea, a
	// tip, a task's result) is held until morning: HH:MM-HH:MM, or off.
	// Empty is push.quiet_hours, else 22:00-07:00.
	QuietHours string `yaml:"quiet_hours,omitempty"`
}

// LLM configures the model gateway.
type LLM struct {
	// Provider is anthropic, openai, gemini, ollama or openai-compatible.
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// APIKeyEnv / APIKey apply to the active provider (legacy; prefer Providers).
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"api_key"`
	// BaseURL overrides the endpoint (required for openai-compatible).
	BaseURL string `yaml:"base_url"`
	// Providers holds per-provider keys, endpoints and last-used models so you can switch freely.
	Providers map[string]ProviderConfig `yaml:"providers"`
	MaxTokens int                       `yaml:"max_tokens"`
	// Effort is one of low, medium, high, xhigh, max.
	Effort string `yaml:"effort"`
	// VoiceEffort is the effort for spoken conversations, where speed matters most (default low).
	VoiceEffort string `yaml:"voice_effort"`
	// VoiceModel optionally uses a faster model of the same provider for spoken replies.
	VoiceModel string `yaml:"voice_model"`
	// HistoryTurns is how many prior messages are replayed per conversation.
	HistoryTurns int `yaml:"history_turns"`
}

// UnmarshalYAML reads the llm section, taking a blank max_tokens
// (max_tokens: "", as a settings form or a hand edit can leave it) as unset,
// so the default applies instead of the whole config failing to load.
func (l *LLM) UnmarshalYAML(n *yaml.Node) error {
	type plain LLM
	if n.Kind == yaml.MappingNode {
		cp := *n
		cp.Content = make([]*yaml.Node, 0, len(n.Content))
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "max_tokens" && v.Kind == yaml.ScalarNode && strings.TrimSpace(v.Value) == "" {
				continue
			}
			cp.Content = append(cp.Content, k, v)
		}
		n = &cp
	}
	return n.Decode((*plain)(l))
}

// ProviderConfig is one provider's settings.
type ProviderConfig struct {
	APIKey    string `yaml:"api_key"`
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
}

// Channels configures how the user reaches the agent.
type Channels struct {
	WhatsApp   WhatsApp   `yaml:"whatsapp"`
	Telegram   Telegram   `yaml:"telegram"`
	IMessage   IMessage   `yaml:"imessage"`
	Discord    Discord    `yaml:"discord"`
	Slack      Slack      `yaml:"slack"`
	Signal     Signal     `yaml:"signal"`
	Matrix     Matrix     `yaml:"matrix"`
	Mattermost Mattermost `yaml:"mattermost"`
	IRC        IRC        `yaml:"irc"`
	Zulip      Zulip      `yaml:"zulip"`
	Mail       Mail       `yaml:"mail"`
	CLI        CLI        `yaml:"cli"`
	Voice      Voice      `yaml:"voice"`
}

// Discord bot channel: a bot application with the Message Content intent.
type Discord struct {
	Enabled  bool   `yaml:"enabled"`
	Token    string `yaml:"token"`
	TokenEnv string `yaml:"token_env"`
	// Owner is your Discord user id (Settings → Advanced → Developer Mode, then Copy User ID) or username.
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
	// Channels lists server channel ids to also answer in (DMs always work). Empty = DMs only.
	Channels []string `yaml:"channels"`
}

// Slack app channel over Socket Mode (no public URL needed).
type Slack struct {
	Enabled bool `yaml:"enabled"`
	// BotToken is the xoxb- token; AppToken is the xapp- token with connections:write.
	BotToken    string `yaml:"bot_token"`
	BotTokenEnv string `yaml:"bot_token_env"`
	AppToken    string `yaml:"app_token"`
	AppTokenEnv string `yaml:"app_token_env"`
	// Owner is your Slack member id (U…) or @handle.
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
}

// Signal channel through signal-cli (https://github.com/AsamK/signal-cli).
type Signal struct {
	Enabled bool `yaml:"enabled"`
	// Account is the number signal-cli is registered as (E.164), e.g. the twin's own number.
	Account string `yaml:"account"`
	// Owner is your number (E.164).
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
	// Bin is the signal-cli executable; HTTP is a running `signal-cli daemon --http` address
	// to use instead of spawning one (e.g. http://127.0.0.1:8080).
	Bin  string `yaml:"bin"`
	HTTP string `yaml:"http"`
}

// Matrix channel (unencrypted rooms) using the client-server API directly.
type Matrix struct {
	Enabled bool `yaml:"enabled"`
	// Homeserver is the base URL, e.g. https://matrix.org.
	Homeserver string `yaml:"homeserver"`
	// UserID is the twin's account (@mirrin:matrix.org); AccessToken authenticates it.
	UserID         string `yaml:"user_id"`
	AccessToken    string `yaml:"access_token"`
	AccessTokenEnv string `yaml:"access_token_env"`
	// Owner is your Matrix id (@you:matrix.org).
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
}

// Mattermost bot channel (REST + websocket).
type Mattermost struct {
	Enabled  bool   `yaml:"enabled"`
	URL      string `yaml:"url"`
	Token    string `yaml:"token"`
	TokenEnv string `yaml:"token_env"`
	// Owner is your username or user id.
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
}

// IRC channel: the twin sits on a server and answers private messages.
type IRC struct {
	Enabled bool   `yaml:"enabled"`
	Server  string `yaml:"server"` // host:port
	TLS     bool   `yaml:"tls"`
	Nick    string `yaml:"nick"`
	// Password is a server password or SASL PLAIN password (NickServ).
	Password    string `yaml:"password"`
	PasswordEnv string `yaml:"password_env"`
	// Owner is your services (NickServ) account, or a nick!user@host mask
	// (* and ? allowed). A bare nick proves nothing: anyone can take it.
	Owner         string   `yaml:"owner"`
	ReplyToOthers bool     `yaml:"reply_to_others"`
	Channels      []string `yaml:"channels"` // channels to join and answer when addressed by nick
}

// Zulip bot channel (REST long-poll event queue).
type Zulip struct {
	Enabled   bool   `yaml:"enabled"`
	Site      string `yaml:"site"`  // https://yourorg.zulipchat.com
	Email     string `yaml:"email"` // bot email
	APIKey    string `yaml:"api_key"`
	APIKeyEnv string `yaml:"api_key_env"`
	// Owner is your Zulip email.
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
}

// Mail channel: the owner emails the twin and gets an email back. Uses the
// email skill's mailbox (skills.email) for IMAP and SMTP.
type Mail struct {
	Enabled bool `yaml:"enabled"`
	// Owner is your email address; only mail from it is answered unless reply_to_others.
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
	// Subject tag marks conversations, e.g. "[twin]". Empty = answer any mail from owner.
	SubjectTag string `yaml:"subject_tag"`
	// PollSeconds between IMAP checks (default 30).
	PollSeconds int `yaml:"poll_seconds"`
	// SMTPHost overrides the guess from the IMAP host (imap.x → smtp.x); SMTPPort defaults to 587.
	SMTPHost string `yaml:"smtp_host"`
	SMTPPort int    `yaml:"smtp_port"`
}

// Telegram bot channel settings.
type Telegram struct {
	Enabled  bool   `yaml:"enabled"`
	Token    string `yaml:"token"`
	TokenEnv string `yaml:"token_env"`
	// Owner is your Telegram user id (numeric, from @userinfobot) or @username.
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
}

// IMessage channel settings (macOS only).
type IMessage struct {
	Enabled bool `yaml:"enabled"`
	// Owner is your phone number (E.164) or iMessage email.
	Owner         string `yaml:"owner"`
	ReplyToOthers bool   `yaml:"reply_to_others"`
}

// Voice channel settings: local speech in (whisper.cpp) and speech out.
type Voice struct {
	Enabled bool `yaml:"enabled"`
	// Mode is "push" (press Enter to talk) or "wake" (always listening for the wake word).
	Mode     string `yaml:"mode"`
	WakeWord string `yaml:"wake_word"`
	// WhisperBin is the whisper.cpp CLI (whisper-cli); WhisperModel is a ggml model file.
	WhisperBin   string `yaml:"whisper_bin"`
	WhisperModel string `yaml:"whisper_model"`
	Language     string `yaml:"language"`
	// Vocabulary lists names and words whisper should expect (people, places, products).
	Vocabulary []string `yaml:"vocabulary"`
	// RecordCommand overrides the sox recorder; it must write 16 kHz mono WAV to $OUT.
	RecordCommand string `yaml:"record_command"`
	MaxSeconds    int    `yaml:"max_seconds"`
	// TTSCommand overrides the OS synthesiser; it receives the text on stdin and in $TEXT.
	TTSCommand string `yaml:"tts_command"`
	// Voice is the synthesiser voice name (e.g. "Daniel" on macOS or in ElevenLabs); Rate is words per minute.
	Voice string `yaml:"voice"`
	Rate  int    `yaml:"rate"`
	// WakeEngine is auto, openwakeword (on-device detector) or transcribe (match the transcript).
	WakeEngine string `yaml:"wake_engine"`
	// WakeModel is an openWakeWord model name or .onnx path (default: the persona's, such as hey_mirrin.onnx, when it is in KokoroDir).
	WakeModel     string  `yaml:"wake_model"`
	WakeThreshold float64 `yaml:"wake_threshold"`
	// Engine picks the speech-out backend: auto, kokoro, elevenlabs, system or command.
	Engine string `yaml:"engine"`
	// KokoroDir is where `mirrin voice setup` installs the offline neural voice.
	KokoroDir string `yaml:"kokoro_dir"`
	// Speed scales speech rate for kokoro (1.0 normal).
	Speed float64 `yaml:"speed"`
	// Pitch shifts the kokoro voice up or down, in cents (100 is a semitone):
	// a persona's character voice (the penguin's is +520). 0 leaves it be.
	Pitch int `yaml:"pitch,omitempty"`
	// TalkOver sets how readily talking over the twin (without its name) interrupts it:
	// off | low | normal | high. Saying the wake word always interrupts. Default normal.
	TalkOver string `yaml:"talk_over"`
	// ChimeSound is an audio file played on wake (default: a built-in soft blip). Try /System/Library/Sounds/Tink.aiff.
	ChimeSound string `yaml:"chime_sound"`
	// Acknowledge plays a tiny spoken "Mm-hm?" the instant the wake word is heard (default on).
	Acknowledge *bool `yaml:"acknowledge"`
	// FollowupSeconds is how long the twin keeps listening after replying, so you can talk back.
	FollowupSeconds int `yaml:"followup_seconds"`
	// ElevenLabsAPIKey (or ELEVENLABS_API_KEY) enables natural cloud speech; ElevenLabsModel defaults to eleven_flash_v2_5.
	ElevenLabsAPIKey string `yaml:"elevenlabs_api_key"`
	ElevenLabsModel  string `yaml:"elevenlabs_model"`
}

// WhatsApp channel settings.
type WhatsApp struct {
	Enabled bool `yaml:"enabled"`
	// Owner is the phone number (E.164, e.g. +61400000000) allowed to command the agent.
	Owner string `yaml:"owner"`
	// ReplyToOthers lets the agent answer messages from people other than the owner.
	ReplyToOthers bool `yaml:"reply_to_others"`
}

// CLI channel settings.
type CLI struct {
	Enabled bool `yaml:"enabled"`
}

// Autonomy controls which tool risk levels run without approval.
// Each value is "auto", "ask" or "never".
// Phone lets the twin text and call through Twilio.
type Phone struct {
	Enabled      bool   `yaml:"enabled"`
	AccountSID   string `yaml:"account_sid"`
	AuthToken    string `yaml:"auth_token"`
	AuthTokenEnv string `yaml:"auth_token_env"`
	// From is the Twilio number the twin calls and texts from (E.164).
	From string `yaml:"from"`
	// Voice is a Twilio/Polly voice, e.g. Polly.Olivia-Neural (AU), Polly.Brian-Neural (UK), Polly.Joanna-Neural (US).
	Voice    string `yaml:"voice"`
	Language string `yaml:"language"`
	// PublicURL is where Twilio can reach this daemon (https://…, e.g. a Tailscale funnel or ngrok to the API port).
	// With it, calls are two-way conversations; without it the twin speaks its line and hangs up.
	PublicURL string `yaml:"public_url"`
}

// Spending caps what the twin may pay for on your behalf.
type Spending struct {
	Currency string `yaml:"currency"`
	// PerActionLimit: a single payment above this always needs your explicit yes, whatever the autonomy setting.
	PerActionLimit float64 `yaml:"per_action_limit"`
	// MonthlyLimit: once this month's recorded spend reaches it, no more payments until you raise it.
	MonthlyLimit float64 `yaml:"monthly_limit"`
}

type Autonomy struct {
	Read      string `yaml:"read"`
	Write     string `yaml:"write"`
	Dangerous string `yaml:"dangerous"`
	// AlwaysAllow lists tool names that never need approval.
	AlwaysAllow []string `yaml:"always_allow"`
	// AlwaysAsk lists tool names that always need approval.
	AlwaysAsk []string `yaml:"always_ask"`
	// ApprovalTTL is how long a request waits for your answer before it
	// lapses and the twin asks afresh (default 72h).
	ApprovalTTL Duration `yaml:"approval_ttl"`
}

// DefaultApprovalTTL is how long a request waits for an answer unless
// autonomy.approval_ttl says otherwise.
const DefaultApprovalTTL = 72 * time.Hour

// TTL is how long a request waits for an answer: ApprovalTTL, or the
// default when it is unset.
func (a Autonomy) TTL() time.Duration {
	if a.ApprovalTTL <= 0 {
		return DefaultApprovalTTL
	}
	return time.Duration(a.ApprovalTTL)
}

// Duration is a length of time written as people write it in config.yaml:
// 72h, 90m, 1h30m.
type Duration time.Duration

// String writes the duration without trailing zero units ("72h", not "72h0m0s").
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// MarshalYAML writes the duration as String does.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML reads a duration such as 72h or 90m. A blank one
// (approval_ttl: "", as a settings form or a hand edit can leave it) is
// unset, so the default applies instead of the whole config failing to load.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	raw := strings.TrimSpace(n.Value)
	if raw == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%q is not a duration (write it like 72h or 90m)", n.Value)
	}
	*d = Duration(v)
	return nil
}

// Skills toggles built-in integrations.
type Skills struct {
	Calendar Calendar `yaml:"calendar"`
	// Gmail and Drive use the same Google sign-in as Calendar (Accounts page).
	Gmail     Toggle    `yaml:"gmail"`
	Drive     Toggle    `yaml:"drive"`
	Email     Email     `yaml:"email"`
	Browser   Browser   `yaml:"browser"`
	System    System    `yaml:"system"`
	Web       Web       `yaml:"web"`
	Reminders Reminders `yaml:"reminders"`
}

// Toggle is a skill with nothing to configure but on/off.
type Toggle struct {
	Enabled bool `yaml:"enabled"`
}

// Calendar is Google Calendar via OAuth.
type Calendar struct {
	Enabled         bool   `yaml:"enabled"`
	CredentialsFile string `yaml:"credentials_file"`
	TokenFile       string `yaml:"token_file"`
	CalendarID      string `yaml:"calendar_id"`
}

// Email is generic IMAP/SMTP.
type Email struct {
	Enabled     bool   `yaml:"enabled"`
	IMAPHost    string `yaml:"imap_host"`
	IMAPPort    int    `yaml:"imap_port"`
	SMTPHost    string `yaml:"smtp_host"`
	SMTPPort    int    `yaml:"smtp_port"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
	Password    string `yaml:"password"`
	FromName    string `yaml:"from_name"`
	// AuthServers are the receiving servers trusted to vouch for senders:
	// the name at the start of their Authentication-Results header, e.g.
	// mx.google.com (a parent domain covers the servers under it). Each
	// entry vouches for one header, so list every server that writes one.
	// Empty means a guess from imap_host.
	AuthServers []string `yaml:"auth_servers,omitempty"`
}

// Browser is headless Chrome via the DevTools protocol.
type Browser struct {
	Enabled bool `yaml:"enabled"`
	// Headless hides the window. browser_signin always shows it so you can log in.
	Headless bool `yaml:"headless"`
	// IdleMinutes before the browser is closed (default 10); logins persist in the profile.
	IdleMinutes int `yaml:"idle_minutes"`
	// KeepScreenshotsDays is how long page screenshots are kept (default 14;
	// -1 keeps them until the size cap). Clearing a conversation removes its own.
	KeepScreenshotsDays int `yaml:"keep_screenshots_days,omitempty"`
	// ScreenshotsMaxMB caps the space screenshots take, oldest removed first
	// (default 200; -1 for no cap).
	ScreenshotsMaxMB int `yaml:"screenshots_max_mb,omitempty"`
	// HandOver is where the owner takes a page over: "screen" (default), in
	// the presence screen's live view, or "window", a separate Chrome window.
	HandOver string `yaml:"handover,omitempty"`
}

// System exposes the local machine (files, shell).
type System struct {
	Enabled     bool     `yaml:"enabled"`
	AllowedDirs []string `yaml:"allowed_dirs"`
	AllowShell  bool     `yaml:"allow_shell"`
}

// Web exposes URL fetching.
type Web struct {
	Enabled bool `yaml:"enabled"`
	// AllowHosts lets fetch_url and the browser reach hosts on your own
	// network (a name, an address or a range like 192.168.1.0/24).
	// Everything private or local is refused otherwise.
	AllowHosts []string `yaml:"allow_hosts,omitempty"`
}

// Reminders exposes timed reminders.
type Reminders struct {
	Enabled bool `yaml:"enabled"`
}

// Path returns the config file path.
func Path() string { return filepath.Join(Home(), "config.yaml") }

// defaultSpending is the spending caps a new twin starts with: 100 a
// payment and 500 a month in the local currency, scaled to round amounts
// worth about as much where a unit is worth far less (10 000 yen, not 100).
// The scale follows the currency, not the region.
func defaultSpending(loc locale) Spending {
	return Spending{Currency: loc.Currency, PerActionLimit: 100, MonthlyLimit: 500}.scaledFor(loc.Currency)
}

// keepUnnamedCaps sets the spending caps a config file doesn't name back to
// the unscaled 100 and 500. Default() scales them to the region's currency
// for a new install, which setup saves; a file without them was written by
// hand or before they were scaled, and ran with 100 and 500 in whatever
// currency it names. A cap is where the twin's trust ends, so an update, or
// a currency set to USD in Japan, never raises it unasked.
func (c *Config) keepUnnamedCaps(raw []byte) {
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil {
		return
	}
	named := map[string]bool{}
	namedPaths(&root, "", named)
	base := defaultSpending(fallbackLocale) // US dollars: unscaled
	if !named["spending.per_action_limit"] {
		c.Spending.PerActionLimit = base.PerActionLimit
	}
	if !named["spending.monthly_limit"] {
		c.Spending.MonthlyLimit = base.MonthlyLimit
	}
}

// Default returns a sensible starting configuration. Money and phone defaults
// follow the country the system says the user is in.
func Default() *Config {
	home := Home()
	loc := localeFor(Region())
	return &Config{
		Name:    "Mirrin",
		Persona: "mirrin",
		User:    User{Name: "", Honorific: "", Timezone: "Local"},
		LLM: LLM{
			Provider:  "anthropic",
			Model:     "claude-opus-5",
			APIKeyEnv: "ANTHROPIC_API_KEY",
			Providers: map[string]ProviderConfig{
				"anthropic": {APIKeyEnv: "ANTHROPIC_API_KEY", Model: "claude-opus-5"},
				"openai":    {APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-4.1"},
				"gemini":    {APIKeyEnv: "GEMINI_API_KEY", Model: "gemini-2.5-pro"},
				"ollama":    {BaseURL: "http://127.0.0.1:11434/v1", Model: "llama3.1"},
			},
			MaxTokens:    16000,
			Effort:       "high",
			VoiceEffort:  "low",
			HistoryTurns: 40,
		},
		Channels: Channels{
			WhatsApp:   WhatsApp{Enabled: false}, // enabled only by the owner or setup
			Telegram:   Telegram{TokenEnv: "TELEGRAM_BOT_TOKEN"},
			Discord:    Discord{TokenEnv: "DISCORD_BOT_TOKEN"},
			Slack:      Slack{BotTokenEnv: "SLACK_BOT_TOKEN", AppTokenEnv: "SLACK_APP_TOKEN"},
			Signal:     Signal{Bin: "signal-cli"},
			Matrix:     Matrix{Homeserver: "https://matrix.org", AccessTokenEnv: "MATRIX_ACCESS_TOKEN"},
			Mattermost: Mattermost{TokenEnv: "MATTERMOST_TOKEN"},
			IRC:        IRC{Server: "irc.libera.chat:6697", TLS: true, PasswordEnv: "IRC_PASSWORD"},
			Zulip:      Zulip{APIKeyEnv: "ZULIP_API_KEY"},
			Mail:       Mail{SubjectTag: "", PollSeconds: 30, SMTPPort: 587},
			CLI:        CLI{Enabled: true},
			Voice: Voice{
				Mode:            "push",
				WakeWord:        "mirrin",
				WhisperBin:      "whisper-cli",
				WhisperModel:    filepath.Join(home, "models", "ggml-base.en.bin"),
				MaxSeconds:      30,
				FollowupSeconds: 6,
				Engine:          "auto",
				WakeEngine:      "auto",
				WakeThreshold:   0.25,
				KokoroDir:       filepath.Join(home, "tts"),
				Voice:           "bm_george",
			},
		},
		Reach:    Reach{Mode: "off", StepUp: "dangerous"},
		Autonomy: Autonomy{Read: "auto", Write: "ask", Dangerous: "ask", ApprovalTTL: Duration(DefaultApprovalTTL)},
		Spending: defaultSpending(loc),
		Phone:    Phone{AuthTokenEnv: "TWILIO_AUTH_TOKEN", Voice: loc.PhoneVoice, Language: loc.PhoneLang},
		Watch:    Watch{Enabled: true, IntervalMinutes: 5, Calendar: true, Inbox: true},
		API:      API{Listen: "127.0.0.1:7742"},
		UI:       UI{AmbientAfterSeconds: 60},
		Tray:     true,
		Skills: Skills{
			Calendar: Calendar{
				CredentialsFile: filepath.Join(home, "google-credentials.json"),
				TokenFile:       filepath.Join(home, "google-token.json"),
				CalendarID:      "primary",
			},
			Email:     Email{IMAPPort: 993, SMTPPort: 587, PasswordEnv: "MIRRIN_EMAIL_PASSWORD"},
			Browser:   Browser{Enabled: true, Headless: true},
			System:    System{Enabled: true, AllowedDirs: []string{"~"}, AllowShell: true},
			Web:       Web{Enabled: true},
			Reminders: Reminders{Enabled: true},
		},
		ProtocolsDir: filepath.Join(home, "protocols"),
		ToolsDir:     filepath.Join(home, "tools"),
		DataDir:      filepath.Join(home, "data"),

		Usage:     Usage{MonthlyBudget: DefaultMonthlyBudget},
		Retention: Retention{AuditDays: DefaultAuditDays, TrimAfterDays: DefaultTrimAfterDays},
	}
}

// Load reads the config file, applying defaults for anything missing.
func Load() (*Config, error) {
	cfg := Default()
	b, err := os.ReadFile(Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s (run `mirrin init`)", Path())
		}
		return nil, err
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(), err)
	}
	cfg.keepUnnamedCaps(b)
	cfg.upgrade(b) // an earlier release's full dump: improved defaults apply (layer.go)
	cfg.expand()
	return cfg, cfg.Validate()
}

// Save writes the config file: only the settings the owner set, over the
// built-in defaults (layer.go), so later releases' better defaults reach
// them. What they set stays set, even where it matches a default.
func (c *Config) Save() error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	b, err := c.layered()
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), b, 0o600)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		home, err := userHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (c *Config) expand() {
	c.ProtocolsDir = expandHome(c.ProtocolsDir)
	if c.ToolsDir == "" {
		c.ToolsDir = filepath.Join(Home(), "tools")
	}
	c.ToolsDir = expandHome(c.ToolsDir)
	c.DataDir = expandHome(c.DataDir)
	c.Channels.Voice.WhisperModel = expandHome(c.Channels.Voice.WhisperModel)
	c.Channels.Voice.KokoroDir = expandHome(c.Channels.Voice.KokoroDir)
	c.Skills.Calendar.CredentialsFile = expandHome(c.Skills.Calendar.CredentialsFile)
	c.Skills.Calendar.TokenFile = expandHome(c.Skills.Calendar.TokenFile)
	for i, d := range c.Skills.System.AllowedDirs {
		c.Skills.System.AllowedDirs[i] = expandHome(d)
	}
}

// Validate checks the configuration for obvious mistakes.
func (c *Config) Validate() error {
	for _, v := range []string{c.Autonomy.Read, c.Autonomy.Write, c.Autonomy.Dangerous} {
		switch v {
		case "auto", "ask", "never":
		default:
			return fmt.Errorf("autonomy levels must be auto, ask or never (got %q)", v)
		}
	}
	if c.LLM.Model == "" {
		return errors.New("llm.model is required")
	}
	if c.Autonomy.ApprovalTTL < 0 {
		return fmt.Errorf("autonomy.approval_ttl must be a positive duration such as 72h (got %s)", time.Duration(c.Autonomy.ApprovalTTL))
	}
	if c.Channels.WhatsApp.Enabled && c.Channels.WhatsApp.Owner == "" {
		return errors.New("channels.whatsapp.owner is required when WhatsApp is enabled")
	}
	if c.Channels.Telegram.Enabled && c.Channels.Telegram.Owner == "" {
		return errors.New("channels.telegram.owner is required when Telegram is enabled")
	}
	if c.Channels.IMessage.Enabled && c.Channels.IMessage.Owner == "" {
		return errors.New("channels.imessage.owner is required when iMessage is enabled")
	}
	for _, ch := range []struct {
		name    string
		enabled bool
		owner   string
	}{
		{"discord", c.Channels.Discord.Enabled, c.Channels.Discord.Owner},
		{"slack", c.Channels.Slack.Enabled, c.Channels.Slack.Owner},
		{"signal", c.Channels.Signal.Enabled, c.Channels.Signal.Owner},
		{"matrix", c.Channels.Matrix.Enabled, c.Channels.Matrix.Owner},
		{"mattermost", c.Channels.Mattermost.Enabled, c.Channels.Mattermost.Owner},
		{"irc", c.Channels.IRC.Enabled, c.Channels.IRC.Owner},
		{"zulip", c.Channels.Zulip.Enabled, c.Channels.Zulip.Owner},
		{"mail", c.Channels.Mail.Enabled, c.Channels.Mail.Owner},
	} {
		if ch.enabled && ch.owner == "" {
			return fmt.Errorf("channels.%s.owner is required when %s is enabled", ch.name, ch.name)
		}
	}
	if c.Channels.Signal.Enabled && c.Channels.Signal.Account == "" {
		return errors.New("channels.signal.account is required (the number signal-cli is registered as)")
	}
	if c.Channels.Matrix.Enabled && c.Channels.Matrix.UserID == "" {
		return errors.New("channels.matrix.user_id is required")
	}
	if c.Channels.Mattermost.Enabled && c.Channels.Mattermost.URL == "" {
		return errors.New("channels.mattermost.url is required")
	}
	if c.Channels.Zulip.Enabled && (c.Channels.Zulip.Site == "" || c.Channels.Zulip.Email == "") {
		return errors.New("channels.zulip.site and channels.zulip.email are required")
	}
	if c.Channels.Mail.Enabled && !c.Skills.Email.Enabled {
		return errors.New("channels.mail needs skills.email configured (it uses that mailbox)")
	}
	switch strings.ToLower(strings.TrimSpace(c.Reach.StepUp)) {
	case "", "dangerous", "write", "all":
	default:
		return fmt.Errorf("reach.step_up must be dangerous, write or all (got %q): approving a dangerous action from another device always needs Face ID or a passkey", c.Reach.StepUp)
	}
	if q := strings.TrimSpace(c.User.QuietHours); q != "" && !strings.EqualFold(q, "off") {
		from, to, ok := strings.Cut(strings.ReplaceAll(q, " ", ""), "-")
		_, e1 := time.Parse("15:04", from)
		_, e2 := time.Parse("15:04", to)
		if !ok || e1 != nil || e2 != nil {
			return fmt.Errorf("user.quiet_hours must be HH:MM-HH:MM, such as 22:00-07:00, or off (got %q)", c.User.QuietHours)
		}
	}
	return nil
}

// APIKey resolves the active provider's API key from config or environment.
func (c *Config) APIKey() string {
	return c.ProviderKey(c.LLM.Provider)
}

// ProviderKey resolves a provider's key: providers map first, then the legacy
// llm.api_key fields (which belong to whichever provider they were written for).
func (c *Config) ProviderKey(provider string) string {
	if p, ok := c.LLM.Providers[provider]; ok {
		if p.APIKey != "" {
			return p.APIKey
		}
		if v := Secret(p.APIKeyEnv); v != "" {
			return v
		}
	}
	legacyFor := c.LLM.Provider
	if c.LLM.APIKeyEnv != "" && strings.HasPrefix(c.LLM.APIKeyEnv, "ANTHROPIC") {
		legacyFor = "anthropic"
	}
	if provider == legacyFor {
		if c.LLM.APIKey != "" {
			return c.LLM.APIKey
		}
		if c.LLM.APIKeyEnv != "" {
			return Secret(c.LLM.APIKeyEnv)
		}
	}
	return Secret(defaultKeyEnv[provider])
}

// defaultKeyEnv is where each hosted provider's key is found when the config
// names no variable (llm.DefaultKeyEnv, which config can't import).
var defaultKeyEnv = map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY", "gemini": "GEMINI_API_KEY"}

// DefaultKeyEnv is the variable a hosted provider's key is read from when
// the config names none ("" for a provider without one).
func DefaultKeyEnv(provider string) string { return defaultKeyEnv[provider] }

// ProviderBaseURL resolves a provider's endpoint override.
func (c *Config) ProviderBaseURL(provider string) string {
	if p, ok := c.LLM.Providers[provider]; ok && p.BaseURL != "" {
		return p.BaseURL
	}
	if provider == c.LLM.Provider {
		return c.LLM.BaseURL
	}
	return ""
}

// ProviderModel returns the model to use when switching to provider.
func (c *Config) ProviderModel(provider string) string {
	if provider == c.LLM.Provider && c.LLM.Model != "" {
		return c.LLM.Model
	}
	if p, ok := c.LLM.Providers[provider]; ok && p.Model != "" {
		return p.Model
	}
	return ""
}

// TelegramToken resolves the bot token from config or environment.
func (c *Config) TelegramToken() string {
	if c.Channels.Telegram.Token != "" {
		return c.Channels.Telegram.Token
	}
	return Secret(c.Channels.Telegram.TokenEnv)
}

// secret returns the literal value, else the named secret.
func secret(value, env string) string {
	if value != "" {
		return value
	}
	return Secret(env)
}

// DiscordToken resolves the Discord bot token.
func (c *Config) DiscordToken() string {
	return secret(c.Channels.Discord.Token, c.Channels.Discord.TokenEnv)
}

// SlackTokens resolves the bot (xoxb) and app (xapp) tokens.
func (c *Config) SlackTokens() (bot, app string) {
	return secret(c.Channels.Slack.BotToken, c.Channels.Slack.BotTokenEnv), secret(c.Channels.Slack.AppToken, c.Channels.Slack.AppTokenEnv)
}

// MatrixToken resolves the Matrix access token.
func (c *Config) MatrixToken() string {
	return secret(c.Channels.Matrix.AccessToken, c.Channels.Matrix.AccessTokenEnv)
}

// MattermostToken resolves the Mattermost bot token.
func (c *Config) MattermostToken() string {
	return secret(c.Channels.Mattermost.Token, c.Channels.Mattermost.TokenEnv)
}

// IRCPassword resolves the IRC server/SASL password.
func (c *Config) IRCPassword() string {
	return secret(c.Channels.IRC.Password, c.Channels.IRC.PasswordEnv)
}

// ZulipKey resolves the Zulip bot API key.
func (c *Config) ZulipKey() string {
	return secret(c.Channels.Zulip.APIKey, c.Channels.Zulip.APIKeyEnv)
}

// EmailPassword resolves the mailbox password from config or environment.
func (c *Config) EmailPassword() string {
	if c.Skills.Email.Password != "" {
		return c.Skills.Email.Password
	}
	return Secret(c.Skills.Email.PasswordEnv)
}
