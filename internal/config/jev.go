package config

// Jev is TypeSafe's Jev, optional quick judgments (internal/jev): off
// unless the owner switches it on and a key resolves. A key in the
// environment alone never turns it on.
type Jev struct {
	// Enabled switches it on; it still needs a key.
	Enabled bool `yaml:"enabled,omitempty"`
	// TypeSafeAPIKey is a key written into the config; secrets.env is better.
	TypeSafeAPIKey string `yaml:"typesafe_api_key,omitempty"`
	// APIKeyEnv names the variable holding the key (default TYPESAFE_API_KEY).
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// Model is the Jev model asked (default jev-latest).
	Model string `yaml:"model,omitempty"`
}

// DefaultJevKeyEnv is where the TypeSafe key is kept when nothing else is named.
const DefaultJevKeyEnv = "TYPESAFE_API_KEY"

// JevKeyEnv is the variable the TypeSafe key is read from.
func (c *Config) JevKeyEnv() string {
	if c.Jev.APIKeyEnv != "" {
		return c.Jev.APIKeyEnv
	}
	return DefaultJevKeyEnv
}

// JevKey is the TypeSafe key: the one in the config, else the saved or
// exported one.
func (c *Config) JevKey() string { return secret(c.Jev.TypeSafeAPIKey, c.JevKeyEnv()) }

// JevOn reports whether the twin may ask Jev: switched on, with a key.
func (c *Config) JevOn() bool { return c.Jev.Enabled && c.JevKey() != "" }

// JevModel is the Jev model to ask.
func (c *Config) JevModel() string {
	if c.Jev.Model != "" {
		return c.Jev.Model
	}
	return "jev-latest"
}
