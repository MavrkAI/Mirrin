package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/channels/signal"
	"github.com/MavrkAI/Mirrin/internal/channels/slack"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestMediaSetupErrorsReachOwner(t *testing.T) {
	d := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	d.agent.SetProvider(seeingLLM{d.llm})
	stubSpeech(t, nil, "", nil)
	for _, tt := range []struct {
		err  error
		want string
	}{
		{slack.ErrFilePermission, "files:read"},
		{signal.ErrAttachmentUpgrade, "Update signal-cli"},
		{signal.ErrAttachmentSizeUnknown, "attachment's size"},
	} {
		for _, owner := range []bool{true, false} {
			for _, kind := range []string{channels.Photo, channels.Voice} {
				in := channels.Inbound{IsOwner: owner, Attachment: &channels.Attachment{Fetch: func(context.Context, int64) ([]byte, error) { return nil, tt.err }}}
				var why *unopened
				if kind == channels.Voice {
					_, why = d.hear(context.Background(), in)
				} else {
					_, why = d.keepPhoto(context.Background(), in)
				}
				if why == nil || strings.Contains(why.reply, tt.want) != owner || strings.Contains(why.note, tt.want) != owner {
					t.Fatalf("owner=%v kind=%s: %+v", owner, kind, why)
				}
			}
		}
	}
}
