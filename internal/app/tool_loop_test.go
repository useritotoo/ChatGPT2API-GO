package app

import (
	"context"
	"testing"
)

func TestToConversationMessagesConvertsToolRoleToUserText(t *testing.T) {
	client := &UpstreamClient{}
	messages := []map[string]any{
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"README.md"}`}}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "file contents"},
	}
	converted, err := client.toConversationMessages(context.Background(), messages)
	if err != nil {
		t.Fatalf("toConversationMessages returned error: %v", err)
	}
	if len(converted) != 2 {
		t.Fatalf("converted length = %d, want 2", len(converted))
	}
	toolMsg := converted[1]
	author, _ := toolMsg["author"].(map[string]any)
	if author["role"] != "user" {
		t.Fatalf("tool message upstream role = %#v, want user", author["role"])
	}
	content, _ := toolMsg["content"].(map[string]any)
	parts, _ := content["parts"].([]string)
	if len(parts) != 1 || parts[0] != "Tool result from call_1:\nfile contents" {
		t.Fatalf("tool message parts = %#v", content["parts"])
	}
}
