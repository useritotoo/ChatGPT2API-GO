package app

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"
)

type toolPolicy struct {
	Required   bool
	ForcedName string
}

type streamToolSieve struct {
	buffer string
}

func newStreamToolSieve() *streamToolSieve { return &streamToolSieve{} }

func (s *streamToolSieve) Process(delta string) string {
	if delta == "" {
		return ""
	}
	s.buffer += delta
	visible, _ := streamableToolText(s.buffer)
	return visible
}

func (s *streamToolSieve) Text() string { return stripToolMarkup(s.buffer) }

func (s *streamToolSieve) Calls() []map[string]any { return parseToolCalls(s.buffer) }

func toolChoicePolicy(toolChoice any) toolPolicy {
	policy := toolPolicy{}
	switch choice := toolChoice.(type) {
	case string:
		policy.Required = strings.TrimSpace(choice) == "required"
	case map[string]any:
		typeName := strings.TrimSpace(strAny(choice["type"], ""))
		policy.Required = typeName == "required"
		if fn, ok := choice["function"].(map[string]any); ok {
			policy.ForcedName = strings.TrimSpace(strAny(fn["name"], ""))
		}
		policy.ForcedName = firstNonEmpty(policy.ForcedName, strings.TrimSpace(strAny(choice["name"], "")))
		if policy.ForcedName != "" {
			policy.Required = true
		}
	}
	return policy
}

func validateToolChoice(calls []map[string]any, tools any, toolChoice any) error {
	if !toolsListNonEmpty(tools) || isToolChoiceNone(toolChoice) {
		return nil
	}
	policy := toolChoicePolicy(toolChoice)
	if !policy.Required && policy.ForcedName == "" {
		return nil
	}
	if len(calls) == 0 {
		return fmt.Errorf("tool_choice requires a tool call")
	}
	if policy.ForcedName != "" {
		for _, call := range calls {
			if strings.TrimSpace(strAny(call["name"], "")) == policy.ForcedName {
				return nil
			}
		}
		return fmt.Errorf("tool_choice requires tool %q", policy.ForcedName)
	}
	return nil
}

func stripToolCodeContexts(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "```") || strings.HasPrefix(text[i:], "~~~") {
			fence := text[i : i+3]
			j := strings.Index(text[i+3:], fence)
			if j < 0 {
				break
			}
			i += 3 + j + 3
			continue
		}
		if text[i] == '`' {
			j := strings.IndexByte(text[i+1:], '`')
			if j < 0 {
				break
			}
			i += j + 2
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func parseCanonicalToolCalls(text string) []map[string]any {
	out := []map[string]any{}
	for _, wrapper := range toolWrapperBlocks(text) {
		for _, block := range invokeBlocks(wrapper) {
			name := firstNonEmpty(tagAttr(block.openTag, "name"), xmlValue(block.body, "name"), xmlValue(block.body, "tool_name"))
			input := parseCanonicalParameters(block.body)
			if len(input) == 0 {
				params := firstNonEmpty(xmlValue(block.body, "parameters"), xmlValue(block.body, "input"), xmlValue(block.body, "arguments"))
				if params != "" {
					input = parseToolParams(params)
				}
			}
			out = append(out, map[string]any{"name": name, "input": input})
		}
	}
	return out
}

type invokeBlock struct {
	openTag string
	body    string
}

func toolWrapperBlocks(text string) []string {
	blocks := []string{}
	re := regexp.MustCompile(`(?is)<tool_calls\b[^>]*>`)
	for _, start := range re.FindAllStringIndex(text, -1) {
		end := matchingCloseTag(text, "tool_calls", start[1])
		if end >= 0 {
			blocks = append(blocks, text[start[1]:end])
		}
	}
	return blocks
}

func invokeBlocks(text string) []invokeBlock {
	blocks := []invokeBlock{}
	re := regexp.MustCompile(`(?is)<invoke\b[^>]*>`)
	for _, start := range re.FindAllStringIndex(text, -1) {
		end := matchingCloseTag(text, "invoke", start[1])
		if end >= 0 {
			blocks = append(blocks, invokeBlock{openTag: text[start[0]:start[1]], body: text[start[1]:end]})
		}
	}
	return blocks
}

func tagAttr(tag, name string) string {
	re := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(name) + `\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	m := re.FindStringSubmatch(tag)
	if len(m) == 0 {
		return ""
	}
	for i := 2; i < len(m); i++ {
		if m[i] != "" {
			return strings.TrimSpace(html.UnescapeString(m[i]))
		}
	}
	return ""
}

func parseCanonicalParameters(raw string) map[string]any {
	obj := map[string]any{}
	re := regexp.MustCompile(`(?is)<parameter\b([^>]*)>(.*?)</parameter>`)
	for _, m := range re.FindAllStringSubmatch(raw, -1) {
		name := tagAttr(m[1], "name")
		if name == "" {
			continue
		}
		obj[name] = parseToolParamContent(m[2])
	}
	return obj
}

func parseToolParamContent(raw string) any {
	value := strings.TrimSpace(raw)
	if items := parseToolItems(value); len(items) > 0 {
		return items
	}
	return parseToolValue(value)
}

func parseToolItems(raw string) []any {
	re := regexp.MustCompile(`(?is)<item\b[^>]*>(.*?)</item>`)
	matches := re.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return nil
	}
	items := make([]any, 0, len(matches))
	for _, m := range matches {
		items = append(items, parseToolValue(m[1]))
	}
	return items
}

func normalizeToolInputForSchema(name string, input map[string]any, tools any) map[string]any {
	if input == nil || !toolsListNonEmpty(tools) {
		return input
	}
	schema := toolParametersSchema(name, tools)
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return input
	}
	out := map[string]any{}
	for k, v := range input {
		out[k] = v
		prop, _ := props[k].(map[string]any)
		if strings.TrimSpace(strAny(prop["type"], "")) == "string" {
			switch v.(type) {
			case map[string]any, []any:
				b, _ := json.Marshal(v)
				out[k] = string(b)
			}
		}
	}
	return out
}

func toolParametersSchema(name string, tools any) map[string]any {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	for _, raw := range arr {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		toolName := firstNonEmpty(strAny(tool["name"], ""), strAny(fn["name"], ""))
		if toolName != name {
			continue
		}
		for _, candidate := range []any{tool["input_schema"], tool["parameters"], fn["input_schema"], fn["parameters"]} {
			if m, ok := candidate.(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

func responseFunctionCallItems(calls []map[string]any) []map[string]any {
	items := []map[string]any{}
	for _, call := range calls {
		name := strings.TrimSpace(strAny(call["name"], ""))
		if name == "" {
			continue
		}
		args, _ := json.Marshal(call["input"])
		callID := "call_" + randID(8)
		items = append(items, map[string]any{"id": "fc_" + randID(8), "type": "function_call", "call_id": callID, "name": name, "arguments": string(args), "status": "completed"})
	}
	return items
}
