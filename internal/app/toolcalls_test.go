package app

import (
	"strings"
	"testing"
)

func TestParseCanonicalXMLToolCalls(t *testing.T) {
	text := `<tool_calls><invoke name="read_file"><parameter name="path"><![CDATA[README.md]]></parameter><parameter name="limit">10</parameter></invoke></tool_calls>`
	calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %#v", calls)
	}
	if calls[0]["name"] != "read_file" {
		t.Fatalf("name = %#v, want read_file", calls[0]["name"])
	}
	input, _ := calls[0]["input"].(map[string]any)
	if input == nil || input["path"] != "README.md" {
		t.Fatalf("input = %#v, want path README.md", input)
	}
	if input["limit"] != float64(10) {
		t.Fatalf("limit = %#v, want numeric 10", input["limit"])
	}
}

func TestParseCanonicalXMLArrayItems(t *testing.T) {
	text := `<tool_calls><invoke name="multi"><parameter name="paths"><item>a.go</item><item>b.go</item></parameter></invoke></tool_calls>`
	calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %#v", calls)
	}
	input := calls[0]["input"].(map[string]any)
	paths, ok := input["paths"].([]any)
	if !ok || len(paths) != 2 || paths[0] != "a.go" || paths[1] != "b.go" {
		t.Fatalf("paths = %#v, want [a.go b.go]", input["paths"])
	}
}

func TestToolCallsIgnoredInCodeContexts(t *testing.T) {
	text := "```xml\n<tool_calls><invoke name=\"read_file\"><parameter name=\"path\">README.md</parameter></invoke></tool_calls>\n```\n`{\"tool_calls\":[{\"tool_name\":\"x\"}]}`"
	calls := parseToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("expected no calls from code contexts, got %#v", calls)
	}
}

func TestStripToolMarkupWithCanonicalXML(t *testing.T) {
	text := `Before <tool_calls><invoke name="read_file"><parameter name="path">README.md</parameter></invoke></tool_calls> after`
	cleaned := stripToolMarkup(text)
	if strings.Contains(cleaned, "tool_calls") || strings.Contains(cleaned, "invoke") {
		t.Fatalf("expected tool markup stripped, got %q", cleaned)
	}
	if cleaned != "Before" {
		t.Fatalf("cleaned = %q, want prefix text before tool call", cleaned)
	}
}

func TestNormalizeToolInputForStringSchema(t *testing.T) {
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "write", "parameters": map[string]any{"type": "object", "properties": map[string]any{"content": map[string]any{"type": "string"}}}}}}
	input := map[string]any{"content": map[string]any{"hello": "world"}}
	normalized := normalizeToolInputForSchema("write", input, tools)
	if normalized["content"] != `{"hello":"world"}` {
		t.Fatalf("content = %#v, want JSON string", normalized["content"])
	}
}

func TestValidateToolChoiceRequired(t *testing.T) {
	tools := []any{map[string]any{"function": map[string]any{"name": "read_file"}}}
	if err := validateToolChoice(nil, tools, "required"); err == nil {
		t.Fatal("expected required tool_choice error")
	}
	calls := []map[string]any{{"name": "read_file", "input": map[string]any{"path": "README.md"}}}
	if err := validateToolChoice(calls, tools, "required"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
