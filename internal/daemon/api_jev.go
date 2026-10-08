package daemon

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/jev"
)

// The quick judgments card on Accounts (api.JevBackend): an optional
// TypeSafe key, kept in secrets.env like the model's; config.yaml gets only
// the switch.

// jevCheckTimeout bounds the card's test call, which a person waits on.
const jevCheckTimeout = 5 * time.Second

var (
	errJevKey = &api.HumanError{Sentence: "That key wasn't accepted.", Fix: "Check it at typesafe.ai and paste it again."}
	errJevOff = &api.HumanError{Sentence: "Couldn't reach TypeSafe just now.", Fix: "Your twin carries on as usual; try again later."}
)

// jevProblemText puts the last failure in the card's words.
func jevProblemText(p string) string {
	switch p {
	case "key":
		return errJevKey.Sentence + " " + errJevKey.Fix
	case "unreachable":
		return errJevOff.Sentence + " " + errJevOff.Fix
	}
	return ""
}

// JevState is the card's state. The key is only ever shown masked.
func (d *Daemon) JevState(context.Context) api.JevState {
	c := d.Config()
	key := c.JevKey()
	st := api.JevState{On: c.JevOn(), HasKey: key != "", Masked: api.MaskKey(key), Env: c.JevKeyEnv()}
	if st.On {
		st.Problem = jevProblemText(d.jevProblem())
	}
	return st
}

// jevKeyToUse is the key a check or save would use: the one supplied, else
// the one the twin has.
func (d *Daemon) jevKeyToUse(c *config.Config, key string) (string, error) {
	key = strings.TrimSpace(key)
	if strings.ContainsAny(key, "\r\n\t ") {
		return "", &api.HumanError{Sentence: "That key has spaces or line breaks in it.", Fix: "Copy the key again, on its own, and paste it."}
	}
	if key == "" {
		if have := c.JevKey(); have != "" {
			return have, nil
		}
		return "", &api.HumanError{Sentence: "There's no TypeSafe key yet.", Fix: "Paste your key from typesafe.ai, then press Check and save."}
	}
	// An exported key wins over a saved one, so saving another changes nothing.
	if name, exported := config.Exported(c.JevKeyEnv()); exported != "" && exported != key {
		return "", &api.HumanError{Sentence: "A different key is set in your environment.", Fix: "Remove " + name + " from the environment, restart Mirrin, then paste your new key."}
	}
	return key, nil
}

// CheckJev tests a key (or the saved one) with one tiny question, without
// saving anything.
func (d *Daemon) CheckJev(ctx context.Context, key string) error {
	c := d.Config()
	key, err := d.jevKeyToUse(&c, key)
	if err != nil {
		return err
	}
	return d.pingJev(ctx, &c, key)
}

// pingJev asks one noul about a fixed state: nothing of the owner's is sent.
func (d *Daemon) pingJev(ctx context.Context, c *config.Config, key string) error {
	res, err := d.jevClient(c, key, jevCheckTimeout).Ask(ctx, "ping", map[string]jev.Question{
		"ping": jev.Noul{Instructions: "Is this a connection test?"},
	})
	if err != nil {
		var se *jev.StatusError
		if errors.As(err, &se) && (se.Code == 401 || se.Code == 403) {
			return errJevKey
		}
		return errJevOff
	}
	d.jevUsage(ctx, "check", c.JevModel(), res)
	return nil
}

// SaveJev checks a supplied key and saves it to secrets.env, then writes
// the switch to the config. Switching on needs a key; forget removes the
// saved key and switches off.
func (d *Daemon) SaveJev(ctx context.Context, key string, on, forget bool) error {
	c := d.Config()
	if forget {
		if err := config.ForgetSecrets(c.JevKeyEnv()); err != nil {
			return errors.New("couldn't remove the key: " + err.Error())
		}
		err := d.UpdateConfig(func(c *config.Config) {
			c.Jev.Enabled = false
			c.Jev.TypeSafeAPIKey = ""
		})
		if err != nil {
			return err
		}
		d.jevWorked()
		// A key exported to the twin can't be forgotten from here: say so,
		// rather than "Removed" while the card still shows it.
		if name, v := config.Exported(c.JevKeyEnv()); v != "" {
			return &api.HumanError{Sentence: "Quick judgments are off, but your key is still set in your environment.",
				Fix: "To forget it, remove " + name + " from the environment and restart Mirrin."}
		}
		return nil
	}
	supplied := strings.TrimSpace(key) != ""
	if supplied {
		k, err := d.jevKeyToUse(&c, key)
		if err != nil {
			return err
		}
		if err := d.pingJev(ctx, &c, k); err != nil {
			return err
		}
		if err := config.SaveSecrets(map[string]string{c.JevKeyEnv(): k}); err != nil {
			return errors.New("couldn't save the key: " + err.Error())
		}
	} else if on && c.JevKey() == "" {
		return &api.HumanError{Sentence: "Add your TypeSafe key first.", Fix: "Paste it above, then press Check and save."}
	}
	err := d.UpdateConfig(func(c *config.Config) {
		c.Jev.Enabled = on
		if supplied {
			c.Jev.TypeSafeAPIKey = "" // a key in the config would hide the one just saved
		}
	})
	if err == nil && supplied {
		d.jevWorked()
	}
	return err
}

var _ api.JevBackend = (*Daemon)(nil)
