package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// retiredKey remembers the retired model the owner was asked about, so they
// are asked once, not after every restart. retiredVoiceKey is the same for
// the spoken-reply model (llm.voice_model).
const (
	retiredKey      = "model.retired.v1"
	retiredVoiceKey = "model.retired.voice.v1"
)

// The settings keep_model can change: the model the twin thinks with, and
// the faster one it speaks with.
const (
	fieldModel      = "model"
	fieldVoiceModel = "voice_model"
)

// noticeRetired checks whether the model, or the spoken-reply model, just
// fell back from one its provider retired, and if so asks the owner, once,
// whether to keep the stand-in. It runs after every turn and background run.
func (d *Daemon) noticeRetired() {
	if d.agent == nil {
		return
	}
	for field, p := range map[string]llm.Provider{fieldModel: d.agent.Provider(), fieldVoiceModel: d.voiceModel()} {
		provider, from, to, ok := llm.FallbackNews(p)
		if !ok {
			continue
		}
		d.log.Warn("model retired; using the provider's default", "setting", field, "provider", provider, "retired", from, "using", to)
		go d.modelRetired(field, provider, from, to) // not on the turn that noticed: the notice is recorded after it
	}
}

// voiceModel is the spoken-reply model, or nil when replies are spoken with the main one.
func (d *Daemon) voiceModel() llm.Provider {
	if p := d.voiceLLM.Load(); p != nil {
		return *p
	}
	return nil
}

// fellBack reports the model configured and the stand-in in use, when the
// configured one was retired and the twin fell back from it.
func (d *Daemon) fellBack() (configured, using string, ok bool) {
	if d.agent == nil {
		return "", "", false
	}
	if f, isF := d.agent.Provider().(*llm.Fallback); isF {
		return f.Using()
	}
	return "", "", false
}

// modelInUse is the model the twin thinks with now: the configured one, or
// the stand-in for it once its provider retired it. /status and the menu
// bar show it, so they never name a model that no longer answers.
func (d *Daemon) modelInUse() string {
	if _, using, ok := d.fellBack(); ok {
		return using
	}
	return d.Config().LLM.Model
}

// modelRetired tells the owner their model was retired and that the twin
// thinks (or speaks) with the provider's current default meanwhile, and
// raises an approval to save that. Nothing changes on disk without their yes.
func (d *Daemon) modelRetired(field, provider, from, to string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	told, key, does := provider+"/"+from, retiredKey, "think"
	if field == fieldVoiceModel {
		key, does = retiredVoiceKey, "speak"
	}
	if v, _ := d.store.Get(ctx, key); v == told {
		return
	}
	text := fmt.Sprintf("%s has retired %s, the model I was set to %s with, so I'm using %s for now.", llm.ProviderLabel(provider), from, does, to)
	owner := d.ownerChatKey()
	if owner == "" {
		if err := desktopNotify(d.Config().Name, text+" Pick the model to keep from my menu."); err != nil {
			d.log.Warn("tell owner about a retired model", "err", err)
			return
		}
		_ = d.store.Set(ctx, key, told)
		return
	}
	input, _ := json.Marshal(map[string]string{"field": field, "provider": provider, "from": from, "to": to})
	verb := "thinking"
	if field == fieldVoiceModel {
		verb = "speaking"
	}
	id, err := d.store.CreateApproval(ctx, owner, "keep_model", input, fmt.Sprintf("keep %s with %s in place of the retired %s", verb, to, from))
	if err != nil {
		d.log.Warn("retired model: approval", "err", err)
		return
	}
	if ap, err := d.store.GetApproval(ctx, id); err == nil {
		d.approvalRaised(*ap)
	}
	text += fmt.Sprintf(" Shall I keep %s? %s", to, answerHint(owner, id))
	if err := d.Notify(ctx, owner, text); err != nil {
		d.log.Warn("tell owner about a retired model", "chat", owner, "err", err)
		return
	}
	_ = d.store.Set(ctx, key, told)
}

// modelTools is keep_model: what the owner's yes to a retired model's
// stand-in carries out. The model is never offered it (it is hidden: only
// the approval the daemon raises calls it). It is dangerous risk, so however
// loose the autonomy settings the owner is asked, and it does one thing
// only: put the provider's current default in place of a configured model,
// or spoken-reply model, the provider really retired. It can't be talked
// into switching to any other model (a web page or an email telling the
// twin to, say).
func (d *Daemon) modelTools() []tools.Tool {
	return []tools.Tool{tools.New("keep_model",
		"Save the provider's current default model in the settings in place of the configured one, after the provider retired it. The daemon asks the owner with a numbered approval when that happens; only use it for that. It refuses any other change.",
		tools.Schema(map[string]tools.Prop{
			"field":    {Type: "string", Description: "the setting: model (the default) or voice_model", Enum: []string{fieldModel, fieldVoiceModel}},
			"provider": {Type: "string", Description: "anthropic, openai or gemini", Required: true},
			"from":     {Type: "string", Description: "the retired model", Required: true},
			"to":       {Type: "string", Description: "the provider's current default model", Required: true},
		}), tools.RiskDangerous,
		func(ctx context.Context, call tools.Call) (string, error) {
			var in struct{ Field, Provider, From, To string }
			if err := tools.Decode(call, &in); err != nil {
				return "", err
			}
			voice := false
			switch in.Field {
			case "", fieldModel:
			case fieldVoiceModel:
				voice = true
			default:
				return "", fmt.Errorf("keep_model changes the model or the voice_model, not %q", in.Field)
			}
			provider := providerName(in.Provider)
			c := d.Config()
			current := c.LLM.Model
			if voice {
				current = c.LLM.VoiceModel
			}
			if providerName(c.LLM.Provider) != provider || current != in.From {
				return fmt.Sprintf("Nothing to change: the settings already say %s, %s.", c.LLM.Provider, current), nil
			}
			if err := d.canKeep(ctx, voice, provider, in.From, in.To); err != nil {
				return "", err
			}
			err := d.UpdateConfig(func(c *config.Config) {
				if voice {
					c.LLM.VoiceModel = in.To
					return
				}
				c.LLM.Model = in.To
				if pc, ok := c.LLM.Providers[provider]; ok && pc.Model == in.From {
					pc.Model = in.To
					c.LLM.Providers[provider] = pc
				}
			})
			if err != nil {
				return "", err
			}
			if voice {
				return fmt.Sprintf("Saved: I'll speak with %s from now on.", in.To), nil
			}
			return fmt.Sprintf("Saved: I'll think with %s from now on.", in.To), nil
		}).Hide()}
}

// canKeep checks that keep_model is being asked for what it is for: the
// provider's current default, in place of a model the provider has retired
// (the twin fell back from it in this run, or told the owner so before).
func (d *Daemon) canKeep(ctx context.Context, voice bool, provider, from, to string) error {
	def := llm.DefaultModel(provider)
	if def == "" || to != def {
		return fmt.Errorf("keep_model only puts %s's current default (%s) in place of a retired model, not %q; to pick another model, choose it from my Model menu", llm.ProviderLabel(provider), def, to)
	}
	p, key := d.agent.Provider(), retiredKey
	if voice {
		p, key = d.voiceModel(), retiredVoiceKey
	}
	if f, ok := p.(*llm.Fallback); ok {
		if configured, _, fellBack := f.Using(); fellBack && configured == from {
			return nil
		}
	}
	if v, _ := d.store.Get(ctx, key); v == provider+"/"+from {
		return nil
	}
	return fmt.Errorf("%s hasn't retired %s, so there's nothing to replace; to pick another model, choose it from my Model menu", llm.ProviderLabel(provider), from)
}

// providerName is a provider setting as the fallback names it: lower case,
// and Anthropic when unset.
func providerName(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "anthropic"
	}
	return p
}
