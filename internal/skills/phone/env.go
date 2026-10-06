package phone

import "github.com/MavrkAI/Mirrin/internal/config"

// envOr resolves a secret named by its environment variable, including keys
// saved in ~/.mirrin/secrets.env.
func envOr(name string) string { return config.Secret(name) }
