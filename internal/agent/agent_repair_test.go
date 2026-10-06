package agent

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestRepairNeverEmptiesAWindowThatOpensMidLoop(t *testing.T) {
	h := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "a", Text: "old"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "b", ToolName: "x"}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "b", Text: "ok"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "done"}}},
	}
	out := repair(h)
	if len(out) == 0 || out[0].Role != llm.RoleUser || out[0].Blocks[0].Type != llm.BlockText {
		t.Fatalf("expected a synthetic leading user message, got %+v", out)
	}
	if len(out) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(out))
	}
}
