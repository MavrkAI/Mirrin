package config

import "strings"

// Field is one thing a connector needs from the user.
type Field struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Value       string `json:"value,omitempty"` // current value; secrets are masked
}

// Step is one setup instruction, optionally with a link.
type Step struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
	Link string `json:"link,omitempty"` // link label
}

// Connector describes a channel the user can switch on from the Channels page.
type Connector struct {
	Name    string  `json:"name"`
	Label   string  `json:"label"`
	Blurb   string  `json:"blurb"`
	Steps   []Step  `json:"steps"`
	Fields  []Field `json:"fields"`
	Enabled bool    `json:"enabled"`
	// Manual is set when the page can only show status (pairing happens in a terminal).
	Manual string `json:"manual,omitempty"`

	get func(*Config, string) string
	set func(*Config, string, string)
	on  func(*Config) *bool
}

func mask(s string) string {
	if len(s) <= 6 {
		if s == "" {
			return ""
		}
		return "••••"
	}
	return s[:3] + "••••" + s[len(s)-3:]
}

// Connectors lists every switchable channel with its current values from c.
func (c *Config) Connectors() []Connector {
	list := connectorSpecs()
	for i := range list {
		k := &list[i]
		k.Enabled = *k.on(c)
		for j := range k.Fields {
			f := &k.Fields[j]
			v := k.get(c, f.Key)
			if f.Secret {
				v = mask(v)
			}
			f.Value = v
		}
	}
	return list
}

// ConnectorByName finds a connector spec.
func ConnectorByName(name string) (Connector, bool) {
	for _, k := range connectorSpecs() {
		if k.Name == name {
			return k, true
		}
	}
	return Connector{}, false
}

// Apply writes the submitted fields into c and sets enabled. Masked secrets
// (unchanged from what the page showed) are left as they were.
func (k Connector) Apply(c *Config, fields map[string]string, enabled bool) {
	for _, f := range k.Fields {
		v, ok := fields[f.Key]
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if f.Secret && (v == "" || v == mask(k.get(c, f.Key))) {
			continue
		}
		k.set(c, f.Key, v)
	}
	*k.on(c) = enabled
}

// MissingRequired names required fields that are still empty.
func (k Connector) MissingRequired(c *Config) []string {
	var out []string
	for _, f := range k.Fields {
		if f.Required && k.get(c, f.Key) == "" {
			out = append(out, f.Label)
		}
	}
	return out
}

func connectorSpecs() []Connector {
	owner := func(label, ph, help string) Field {
		return Field{Key: "owner", Label: label, Placeholder: ph, Help: help, Required: true}
	}
	return []Connector{
		{
			Name: "telegram", Label: "Telegram", Blurb: "A bot you message from your phone. Two minutes, no server.",
			Steps: []Step{
				{Text: "Open BotFather, send /newbot, pick a name, copy the token.", URL: "https://t.me/BotFather", Link: "Open BotFather"},
				{Text: "Send /start to @userinfobot to see your numeric id.", URL: "https://t.me/userinfobot", Link: "Open userinfobot"},
			},
			Fields: []Field{
				{Key: "token", Label: "Bot token", Placeholder: "123456:ABC…", Secret: true, Required: true},
				owner("Your Telegram id", "123456789 or @you", ""),
			},
			get: func(c *Config, k string) string {
				switch k {
				case "token":
					return c.Channels.Telegram.Token
				case "owner":
					return c.Channels.Telegram.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "token":
					c.Channels.Telegram.Token = v
				case "owner":
					c.Channels.Telegram.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.Telegram.Enabled },
		},
		{
			Name: "discord", Label: "Discord", Blurb: "A bot you can DM, and @mention in your servers.",
			Steps: []Step{
				{Text: "New Application → Bot → Reset Token. Under Privileged Gateway Intents switch on Message Content.", URL: "https://discord.com/developers/applications?new_application=true", Link: "Create the app"},
				{Text: "Paste the token below; an invite link appears once it's connected. Open it to add the bot to any server you own (needed even for DMs).", URL: "", Link: ""},
				{Text: "Your id: Discord Settings → Advanced → Developer Mode, then right-click your name → Copy User ID.", URL: "", Link: ""},
			},
			Fields: []Field{
				{Key: "token", Label: "Bot token", Placeholder: "MTIz…", Secret: true, Required: true},
				owner("Your Discord user id", "123456789012345678", "or your username"),
			},
			get: func(c *Config, k string) string {
				switch k {
				case "token":
					return c.Channels.Discord.Token
				case "owner":
					return c.Channels.Discord.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "token":
					c.Channels.Discord.Token = v
				case "owner":
					c.Channels.Discord.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.Discord.Enabled },
		},
		{
			Name: "slack", Label: "Slack", Blurb: "An app in your workspace, with no public server needed. The link below creates it ready to go.",
			Steps: []Step{
				{Text: "Create the app from the prepared manifest (scopes, Socket Mode and events are already set). Pick your workspace and click Create.", URL: "SLACK_MANIFEST", Link: "Create the Slack app"},
				{Text: "Basic Information → App-Level Tokens → Generate (scope connections:write). That's the xapp- token.", URL: "", Link: ""},
				{Text: "Install App → Install to Workspace. That gives the xoxb- bot token.", URL: "", Link: ""},
				{Text: "Your member id: click your profile → ⋯ → Copy member ID.", URL: "", Link: ""},
			},
			Fields: []Field{
				{Key: "bot_token", Label: "Bot token (xoxb-)", Placeholder: "xoxb-…", Secret: true, Required: true},
				{Key: "app_token", Label: "App-level token (xapp-)", Placeholder: "xapp-…", Secret: true, Required: true},
				owner("Your member id", "U0123ABCD", "or @handle"),
			},
			get: func(c *Config, k string) string {
				switch k {
				case "bot_token":
					return c.Channels.Slack.BotToken
				case "app_token":
					return c.Channels.Slack.AppToken
				case "owner":
					return c.Channels.Slack.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "bot_token":
					c.Channels.Slack.BotToken = v
				case "app_token":
					c.Channels.Slack.AppToken = v
				case "owner":
					c.Channels.Slack.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.Slack.Enabled },
		},
		{
			Name: "signal", Label: "Signal", Blurb: "Through signal-cli on this machine. Best with a second number for the twin.",
			Steps: []Step{
				{Text: "Install signal-cli (brew install signal-cli).", URL: "https://github.com/AsamK/signal-cli#installation", Link: "Install guide"},
				{Text: "Register a number for the twin: signal-cli -a +61… register, then signal-cli -a +61… verify CODE.", URL: "", Link: ""},
				{Text: "Or link it to your own Signal account instead: run signal-cli link -n mirrin, turn the sgnl:// link it prints into a QR code (for example qrencode -t ansi 'LINK'), and scan it on your phone under Settings, Linked devices. Then set Twin's number and Your number both to your own number; the twin answers in Note to Self.", URL: "", Link: ""},
			},
			Fields: []Field{
				{Key: "account", Label: "Twin's number (registered or linked in signal-cli)", Placeholder: "+61400000001", Required: true},
				owner("Your number", "+61400000002", ""),
			},
			get: func(c *Config, k string) string {
				switch k {
				case "account":
					return c.Channels.Signal.Account
				case "owner":
					return c.Channels.Signal.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "account":
					c.Channels.Signal.Account = v
				case "owner":
					c.Channels.Signal.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.Signal.Enabled },
		},
		{
			Name: "matrix", Label: "Matrix", Blurb: "Any homeserver. Give the twin its own account; it opens a DM with you.",
			Steps: []Step{
				{Text: "Create an account for the twin on your homeserver (matrix.org works).", URL: "https://app.element.io/#/register", Link: "Register on Element"},
				{Text: "Enter that account's password once; it's exchanged for a token and not stored. Or paste an access token instead.", URL: "", Link: ""},
			},
			Fields: []Field{
				{Key: "homeserver", Label: "Homeserver", Placeholder: "https://matrix.org", Required: true},
				{Key: "user_id", Label: "Twin's Matrix id", Placeholder: "@mirrin:matrix.org", Required: true},
				{Key: "password", Label: "Twin's password (login once)", Secret: true},
				{Key: "access_token", Label: "…or access token", Secret: true},
				owner("Your Matrix id", "@you:matrix.org", ""),
			},
			get: func(c *Config, k string) string {
				switch k {
				case "homeserver":
					return c.Channels.Matrix.Homeserver
				case "user_id":
					return c.Channels.Matrix.UserID
				case "access_token":
					return c.Channels.Matrix.AccessToken
				case "owner":
					return c.Channels.Matrix.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "homeserver":
					c.Channels.Matrix.Homeserver = v
				case "user_id":
					c.Channels.Matrix.UserID = v
				case "access_token":
					c.Channels.Matrix.AccessToken = v
				case "owner":
					c.Channels.Matrix.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.Matrix.Enabled },
		},
		{
			Name: "mattermost", Label: "Mattermost", Blurb: "A bot account on your own server.",
			Steps: []Step{
				{Text: "System Console → Integrations → Bot Accounts → Enable. Then Integrations → Bot Accounts → Add Bot Account, copy the token.", URL: "", Link: ""},
			},
			Fields: []Field{
				{Key: "url", Label: "Server URL", Placeholder: "https://chat.example.com", Required: true},
				{Key: "token", Label: "Bot token", Secret: true, Required: true},
				owner("Your username", "akshay", ""),
			},
			get: func(c *Config, k string) string {
				switch k {
				case "url":
					return c.Channels.Mattermost.URL
				case "token":
					return c.Channels.Mattermost.Token
				case "owner":
					return c.Channels.Mattermost.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "url":
					c.Channels.Mattermost.URL = v
				case "token":
					c.Channels.Mattermost.Token = v
				case "owner":
					c.Channels.Mattermost.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.Mattermost.Enabled },
		},
		{
			Name: "zulip", Label: "Zulip", Blurb: "A generic bot in your organisation.",
			Steps: []Step{
				{Text: "Personal settings → Bots → Add a new bot (Generic). Copy its email and API key.", URL: "", Link: ""},
			},
			Fields: []Field{
				{Key: "site", Label: "Site", Placeholder: "https://yourorg.zulipchat.com", Required: true},
				{Key: "email", Label: "Bot email", Placeholder: "mirrin-bot@yourorg.zulipchat.com", Required: true},
				{Key: "api_key", Label: "Bot API key", Secret: true, Required: true},
				owner("Your Zulip email", "you@example.com", ""),
			},
			get: func(c *Config, k string) string {
				switch k {
				case "site":
					return c.Channels.Zulip.Site
				case "email":
					return c.Channels.Zulip.Email
				case "api_key":
					return c.Channels.Zulip.APIKey
				case "owner":
					return c.Channels.Zulip.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "site":
					c.Channels.Zulip.Site = v
				case "email":
					c.Channels.Zulip.Email = v
				case "api_key":
					c.Channels.Zulip.APIKey = v
				case "owner":
					c.Channels.Zulip.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.Zulip.Enabled },
		},
		{
			Name: "irc", Label: "IRC", Blurb: "Sits on a server as a nick. No account needed on most networks.",
			Steps: []Step{
				{Text: "Optional: register the nick with NickServ and enter its password for SASL.", URL: "", Link: ""},
			},
			Fields: []Field{
				{Key: "server", Label: "Server", Placeholder: "irc.libera.chat:6697", Required: true},
				{Key: "nick", Label: "Twin's nick", Placeholder: "mirrin", Required: true},
				{Key: "password", Label: "NickServ password", Secret: true},
				owner("Your NickServ account", "akshay", "the account you log in to (a nick alone can be borrowed), or nick!user@host if the server has no NickServ"),
				{Key: "channels", Label: "Channels to join", Placeholder: "#room1, #room2"},
			},
			get: func(c *Config, k string) string {
				switch k {
				case "server":
					return c.Channels.IRC.Server
				case "nick":
					return c.Channels.IRC.Nick
				case "password":
					return c.Channels.IRC.Password
				case "owner":
					return c.Channels.IRC.Owner
				case "channels":
					return strings.Join(c.Channels.IRC.Channels, ", ")
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "server":
					c.Channels.IRC.Server = v
					c.Channels.IRC.TLS = strings.HasSuffix(v, ":6697") || strings.HasSuffix(v, ":7000") || strings.HasSuffix(v, ":7070")
				case "nick":
					c.Channels.IRC.Nick = v
				case "password":
					c.Channels.IRC.Password = v
				case "owner":
					c.Channels.IRC.Owner = v
				case "channels":
					c.Channels.IRC.Channels = nil
					for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
						if s != "" {
							c.Channels.IRC.Channels = append(c.Channels.IRC.Channels, s)
						}
					}
				}
			},
			on: func(c *Config) *bool { return &c.Channels.IRC.Enabled },
		},
		{
			Name: "mail", Label: "Email", Blurb: "Email the twin, get a reply in the thread. It answers from the twin's own mailbox.",
			Steps: []Step{
				{Text: "Set up the twin's mailbox first: in the settings file, fill in skills.email (mail server, username and app password). Gmail: Google Account → Security → App passwords.", URL: "https://myaccount.google.com/apppasswords", Link: "Gmail app passwords"},
			},
			Fields: []Field{
				owner("Your email address", "you@example.com", "only mail from here is answered"),
				{Key: "subject_tag", Label: "Subject tag (optional)", Placeholder: "[Mirrin]", Help: "answer only threads whose subject contains this"},
			},
			get: func(c *Config, k string) string {
				switch k {
				case "owner":
					return c.Channels.Mail.Owner
				case "subject_tag":
					return c.Channels.Mail.SubjectTag
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				switch k {
				case "owner":
					c.Channels.Mail.Owner = v
				case "subject_tag":
					c.Channels.Mail.SubjectTag = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.Mail.Enabled },
		},
		{
			Name: "imessage", Label: "iMessage", Blurb: "macOS only. Best with the Mac's Messages signed into a dedicated Apple ID for the twin.",
			Steps: []Step{
				{Text: "Grant Full Disk Access to mirrin (System Settings → Privacy & Security → Full Disk Access) and allow Automation for Messages when asked.", URL: "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles", Link: "Open Full Disk Access"},
			},
			Fields: []Field{owner("Your number or Apple ID email", "+61400000000", "")},
			get: func(c *Config, k string) string {
				if k == "owner" {
					return c.Channels.IMessage.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				if k == "owner" {
					c.Channels.IMessage.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.IMessage.Enabled },
		},
		{
			Name: "whatsapp", Label: "WhatsApp", Blurb: "Links to your own WhatsApp like WhatsApp Web. You then talk to your twin in the chat with yourself, and it messages you there.",
			Fields: []Field{owner("Your number", "+61400000000", "the WhatsApp account you'll link")},
			get: func(c *Config, k string) string {
				if k == "owner" {
					return c.Channels.WhatsApp.Owner
				}
				return ""
			},
			set: func(c *Config, k, v string) {
				if k == "owner" {
					c.Channels.WhatsApp.Owner = v
				}
			},
			on: func(c *Config) *bool { return &c.Channels.WhatsApp.Enabled },
		},
	}
}
