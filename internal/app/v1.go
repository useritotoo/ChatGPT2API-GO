package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func (s *Server) handleV1Models(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireIdentity(w, r); !ok {
		return
	}
	if client, err := s.upstreamClient(); err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		if models, err := client.ListModels(ctx); err == nil {
			writeJSON(w, 200, s.mergeDynamicModels(models))
			return
		}
	}
	models := []string{"gpt-image-2", "codex-gpt-image-2", "auto", "gpt-5", "gpt-5-mini", "gpt-5-1", "gpt-5-2", "gpt-5-3", "gpt-5-3-mini"}
	data := []map[string]any{}
	for _, m := range models {
		data = append(data, map[string]any{"id": m, "object": "model", "created": time.Now().Unix(), "owned_by": "chatgpt2api-go"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}
func (s *Server) imageResult(w http.ResponseWriter, r *http.Request, id *Identity, prompt, model, size, resolution, responseFormat string, n int, isEdit bool, inputs [][]byte) {
	action := "文生图"
	endpoint := "/v1/images/generations"
	if isEdit {
		action = "图生图"
		endpoint = "/v1/images/edits"
	}
	callID := s.logCallStart(id, endpoint, model, action, prompt)
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	if err := s.checkContent(prompt); err != nil {
		s.logCallFailure(callID, endpoint, model, action, err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if !s.consumeImage(id, n) {
		err := fmt.Errorf("画图额度不足")
		s.logCallFailure(callID, endpoint, model, action, err, nil)
		writeErr(w, 402, err.Error())
		return
	}
	refs := inputs
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	data := []map[string]any{}
	for i := 0; i < n; i++ {
		items, err := s.generateImageWithPool(ctx, prompt, model, size, resolution, refs)
		if err != nil {
			s.refundImage(id, n-len(data))
			s.logCallFailure(callID, endpoint, model, action, err, map[string]any{"n": n})
			writeErr(w, 502, err.Error())
			return
		}
		for _, result := range items {
			rel, url, err := s.saveImage(r, result.Bytes)
			if err != nil {
				s.refundImage(id, n-len(data))
				s.logCallFailure(callID, endpoint, model, action, err, nil)
				writeErr(w, 500, err.Error())
				return
			}
			s.recordOwner(id, rel)
			s.recordPrompt(rel, prompt, isEdit)
			item := map[string]any{"url": url, "revised_prompt": firstNonEmpty(result.RevisedPrompt, prompt)}
			if responseFormat == "b64_json" || responseFormat == "" {
				item["b64_json"] = base64.StdEncoding.EncodeToString(result.Bytes)
			}
			data = append(data, item)
			break
		}
	}
	s.logCallSuccess(callID, endpoint, model, action, map[string]any{"n": n, "image_count": len(data)})
	writeJSON(w, 200, map[string]any{"created": time.Now().Unix(), "data": data})
}
func (s *Server) handleV1ImagesGenerations(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b struct {
		Prompt         string `json:"prompt"`
		Model          string `json:"model"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		Resolution     string `json:"resolution"`
		ResponseFormat string `json:"response_format"`
		Stream         bool   `json:"stream"`
	}
	if !readBody(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.Prompt) == "" {
		writeErr(w, 400, "prompt is required")
		return
	}
	if b.Model == "" {
		b.Model = "gpt-image-2"
	}
	if b.Stream {
		s.imageResultStream(w, r, id, b.Prompt, b.Model, b.Size, b.Resolution, b.N, false, nil)
		return
	}
	s.imageResult(w, r, id, b.Prompt, b.Model, b.Size, b.Resolution, b.ResponseFormat, b.N, false, nil)
}
func (s *Server) handleV1ImagesEdits(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, 400, "invalid multipart form")
		return
	}
	prompt := r.FormValue("prompt")
	model := r.FormValue("model")
	if model == "" {
		model = "gpt-image-2"
	}
	n := 1
	fmt.Sscanf(r.FormValue("n"), "%d", &n)
	inputs := [][]byte{}
	for _, key := range []string{"image", "image[]"} {
		for _, fh := range r.MultipartForm.File[key] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(f)
			_ = f.Close()
			if len(b) > 0 {
				inputs = append(inputs, b)
			}
		}
	}
	if len(inputs) == 0 {
		writeErr(w, 400, "image file is required")
		return
	}
	s.imageResult(w, r, id, prompt, model, r.FormValue("size"), r.FormValue("resolution"), r.FormValue("response_format"), n, true, inputs)
}
func (s *Server) handleV1ChatCompletions(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b map[string]any
	if !readBody(w, r, &b) {
		return
	}
	if isImageChatRequest(b) {
		s.handleV1ChatImageCompletion(w, r, id, b)
		return
	}
	model := strAny(b["model"], "auto")
	messages, err := s.chatCompletionMessages(b)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	requestText := extractPrompt(b)
	callID := s.logCallStart(id, "/v1/chat/completions", model, "文本生成", requestText)
	if err := s.checkContent(requestText); err != nil {
		s.logCallFailure(callID, "/v1/chat/completions", model, "文本生成", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if !s.consumeChat(id, 1) {
		writeErr(w, 402, "对话额度不足")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if boolAny(b["stream"], false) {
		events, errs := s.streamTextWithRetry(ctx, messages, model, "", "")
		w.Header().Set("Content-Type", "text/event-stream")
		cid := "chatcmpl-" + randID(8)
		sentRole := false
		full := ""
		toolMode := toolsListNonEmpty(b["tools"])
		streamedVisible := ""
		toolStarted := false
		for ev := range events {
			delta := ev.Delta
			full += delta
			visible := full
			if toolMode {
				var started bool
				visible, started = streamableToolText(full)
				if started {
					toolStarted = true
				}
			}
			if toolStarted && !strings.HasPrefix(visible, streamedVisible) {
				continue
			}
			if !strings.HasPrefix(visible, streamedVisible) {
				streamedVisible = ""
			}
			nextDelta := visible[len(streamedVisible):]
			if nextDelta == "" && sentRole {
				continue
			}
			streamedVisible = visible
			d := map[string]any{"content": nextDelta}
			if !sentRole {
				d["role"] = "assistant"
				sentRole = true
			}
			sse(w, map[string]any{"id": cid, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": nil}}})
		}
		if err := <-errs; err != nil {
			s.refundChat(id, 1)
			s.logCallFailure(callID, "/v1/chat/completions", model, "文本生成", err, nil)
			sse(w, map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		} else {
			s.logCallSuccess(callID, "/v1/chat/completions", model, "文本生成", map[string]any{"stream": true})
		}
		parsedToolCalls := parseToolCalls(full)
		toolCalls := openAIToolCallsFromText(full, b["tools"])
		if err := validateToolChoice(parsedToolCalls, b["tools"], b["tool_choice"]); err != nil {
			sse(w, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_tool_choice"}})
		}
		if len(toolCalls) > 0 {
			if !sentRole {
				sse(w, map[string]any{"id": cid, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": nil}, "finish_reason": nil}}})
			}
			for i, call := range toolCalls {
				fn, _ := call["function"].(map[string]any)
				sse(w, map[string]any{"id": cid, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": call["id"], "type": "function", "function": map[string]any{"name": fn["name"], "arguments": fn["arguments"]}}}}, "finish_reason": nil}}})
			}
		}
		sse(w, map[string]any{"id": cid, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": chatCompletionFinishReason(toolCalls)}}})
		sseDone(w)
		return
	}
	content, err := s.collectTextWithRetry(ctx, messages, model)
	if err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/chat/completions", model, "文本生成", err, nil)
		writeErr(w, 502, err.Error())
		return
	}
	parsedToolCalls := parseToolCalls(content)
	if err := validateToolChoice(parsedToolCalls, b["tools"], b["tool_choice"]); err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/chat/completions", model, "文本生成", err, nil)
		writeErr(w, 422, err.Error())
		return
	}
	toolCalls := openAIToolCallsFromText(content, b["tools"])
	cleanContent := content
	if len(toolCalls) > 0 || (toolsListNonEmpty(b["tools"]) && containsToolMarkup(content)) {
		cleanContent = stripToolMarkup(content)
	}
	s.logCallSuccess(callID, "/v1/chat/completions", model, "文本生成", map[string]any{"completion_tokens": approxTokens(cleanContent)})
	pt, ct := countMessageTokens(messages), approxTokens(cleanContent)
	writeJSON(w, 200, map[string]any{"id": "chatcmpl-" + randID(8), "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": chatCompletionMessage(cleanContent, toolCalls), "finish_reason": chatCompletionFinishReason(toolCalls)}}, "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}})
}
func (s *Server) handleV1ChatImageCompletion(w http.ResponseWriter, r *http.Request, id *Identity, b map[string]any) {
	model := strAny(b["model"], "gpt-image-2")
	if strings.TrimSpace(model) == "" || !isSupportedImageModel(model) {
		model = "gpt-image-2"
	}
	prompt := extractChatPrompt(b)
	if strings.TrimSpace(prompt) == "" {
		writeErr(w, 400, "prompt is required")
		return
	}
	if err := s.checkContent(prompt); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	n := intAny(b["n"], 1)
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	if !s.consumeImage(id, n) {
		writeErr(w, 402, "画图额度不足")
		return
	}
	size := strAny(b["size"], "")
	resolution := strAny(b["resolution"], "")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	refs := extractChatImages(b)
	data := []map[string]any{}
	for i := 0; i < n; i++ {
		items, err := s.generateImageWithPool(ctx, prompt, model, size, resolution, refs)
		if err != nil {
			s.refundImage(id, n-len(data))
			writeErr(w, 502, err.Error())
			return
		}
		for _, result := range items {
			rel, url, err := s.saveImage(r, result.Bytes)
			if err != nil {
				s.refundImage(id, n-len(data))
				writeErr(w, 500, err.Error())
				return
			}
			s.recordOwner(id, rel)
			s.recordPrompt(rel, prompt, len(refs) > 0)
			data = append(data, map[string]any{"url": url, "b64_json": base64.StdEncoding.EncodeToString(result.Bytes), "revised_prompt": firstNonEmpty(result.RevisedPrompt, prompt)})
			break
		}
	}
	content := buildChatImageMarkdown(data)
	if boolAny(b["stream"], false) {
		w.Header().Set("Content-Type", "text/event-stream")
		cid := "chatcmpl-" + randID(8)
		created := time.Now().Unix()
		sse(w, map[string]any{"id": cid, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}}})
		sse(w, map[string]any{"id": cid, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		sseDone(w)
		return
	}
	writeJSON(w, 200, map[string]any{"id": "chatcmpl-" + randID(8), "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": approxTokens(prompt), "completion_tokens": approxTokens(content), "total_tokens": approxTokens(prompt) + approxTokens(content)}})
}

func (s *Server) handleV1Responses(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b map[string]any
	if !readBody(w, r, &b) {
		return
	}
	model := strAny(b["model"], "auto")
	requestText := extractPrompt(b)
	callID := s.logCallStart(id, "/v1/responses", model, "Responses", requestText)
	if hasResponseImageGenerationTool(b) {
		s.handleV1ResponseImage(w, r, id, b, callID)
		return
	}
	if !s.consumeChat(id, 1) {
		writeErr(w, 402, "对话额度不足")
		return
	}
	messages, err := s.responseMessagesFromBody(b)
	if err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	requestText = messagesPlainText(messages)
	if err := s.checkContent(requestText); err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if boolAny(b["stream"], false) {
		s.streamResponseEvents(w, r, id, messages, model, b["tools"], b["tool_choice"], callID)
		return
	}
	content, err := s.collectUpstreamText(r, messages, model)
	if err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, nil)
		writeErr(w, 502, err.Error())
		return
	}
	parsedToolCalls := parseToolCalls(content)
	if err := validateToolChoice(parsedToolCalls, b["tools"], b["tool_choice"]); err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, nil)
		writeErr(w, 422, err.Error())
		return
	}
	cleanContent := content
	if len(parsedToolCalls) > 0 || (toolsListNonEmpty(b["tools"]) && containsToolMarkup(content)) {
		cleanContent = stripToolMarkup(content)
	}
	s.logCallSuccess(callID, "/v1/responses", model, "Responses", map[string]any{"output_tokens": approxTokens(cleanContent)})
	out := []map[string]any{}
	if strings.TrimSpace(cleanContent) != "" || len(parsedToolCalls) == 0 {
		out = append(out, responseTextOutputItem(cleanContent, "", "completed"))
	}
	out = append(out, responseFunctionCallItems(parsedToolCalls)...)
	inputTokens, outputTokens := approxTokens(requestText), approxTokens(cleanContent)
	writeJSON(w, 200, map[string]any{"id": "resp_" + randID(8), "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": out, "parallel_tool_calls": false, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens}})
}
func (s *Server) handleV1Messages(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" && r.Header.Get("x-api-key") != "" {
		r.Header.Set("Authorization", "Bearer "+r.Header.Get("x-api-key"))
	}
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b map[string]any
	if !readBody(w, r, &b) {
		return
	}
	model := strAny(b["model"], "auto")
	requestText := extractPrompt(b)
	callID := s.logCallStart(id, "/v1/messages", model, "Messages", requestText)
	if !s.consumeChat(id, 1) {
		writeErr(w, 402, "对话额度不足")
		return
	}
	messages, err := s.anthropicMessagesFromBody(b)
	if err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/messages", model, "Messages", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	requestText = messagesPlainText(messages)
	if err := s.checkContent(requestText); err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/messages", model, "Messages", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if boolAny(b["stream"], false) {
		s.streamAnthropicEvents(w, r, id, messages, model, b["tools"], b["tool_choice"], callID)
		return
	}
	content, err := s.collectUpstreamText(r, messages, model)
	if err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/messages", model, "Messages", err, nil)
		writeErr(w, 502, err.Error())
		return
	}
	parsedToolCalls := parseToolCalls(content)
	if err := validateToolChoice(parsedToolCalls, b["tools"], b["tool_choice"]); err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/messages", model, "Messages", err, nil)
		writeErr(w, 422, err.Error())
		return
	}
	s.logCallSuccess(callID, "/v1/messages", model, "Messages", map[string]any{"output_tokens": approxTokens(stripToolMarkup(content))})
	blocks, stopReason := anthropicContentBlocks(content, b["tools"])
	writeJSON(w, 200, map[string]any{"id": "msg_" + randID(8), "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stopReason, "stop_sequence": nil, "usage": map[string]any{"input_tokens": approxTokens(requestText), "output_tokens": approxTokens(content)}})
}
func (s *Server) handleV1ResponseImage(w http.ResponseWriter, r *http.Request, id *Identity, b map[string]any, callID string) {
	model := strAny(b["model"], "gpt-image-2")
	if strings.TrimSpace(model) == "" || !isSupportedImageModel(model) {
		model = "gpt-image-2"
	}
	prompt := extractResponsePrompt(b["input"])
	if prompt == "" {
		err := fmt.Errorf("input text is required")
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.checkContent(prompt); err != nil {
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if !s.consumeImage(id, 1) {
		writeErr(w, 402, "画图额度不足")
		return
	}
	refs := extractResponseImages(b["input"])
	size := ""
	if len(refs) == 0 {
		size = "1:1"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	items, err := s.generateImageWithPool(ctx, prompt, model, size, "", refs)
	if err != nil {
		s.refundImage(id, 1)
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, nil)
		writeErr(w, 502, err.Error())
		return
	}
	data := []map[string]any{}
	for _, result := range items {
		rel, url, err := s.saveImage(r, result.Bytes)
		if err != nil {
			s.refundImage(id, 1)
			s.logCallFailure(callID, "/v1/responses", model, "Responses", err, nil)
			writeErr(w, 500, err.Error())
			return
		}
		s.recordOwner(id, rel)
		s.recordPrompt(rel, prompt, len(refs) > 0)
		data = append(data, map[string]any{"url": url, "b64_json": base64.StdEncoding.EncodeToString(result.Bytes), "revised_prompt": firstNonEmpty(result.RevisedPrompt, prompt)})
		break
	}
	respID := "resp_" + randID(8)
	created := time.Now().Unix()
	out := responseImageOutputItems(prompt, data)
	if boolAny(b["stream"], false) {
		s.logCallSuccess(callID, "/v1/responses", model, "Responses", map[string]any{"stream": true, "image_count": len(out)})
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, responseCreatedEvent(respID, model, created))
		sse(w, responseInProgressEvent(respID, model, created))
		if len(out) > 0 {
			sse(w, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": out[0]})
		}
		completed := responseCompletedEvent(respID, model, created, out)
		sse(w, completed)
		sse(w, map[string]any{"type": "response.done", "response": completed["response"]})
		sseDone(w)
		return
	}
	s.logCallSuccess(callID, "/v1/responses", model, "Responses", map[string]any{"image_count": len(out)})
	writeJSON(w, 200, responseCompletedEvent(respID, model, created, out)["response"])
}

func (s *Server) streamResponseEvents(w http.ResponseWriter, r *http.Request, id *Identity, messages []map[string]any, model string, tools any, toolChoice any, callID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	events, errs := s.streamTextWithRetry(ctx, messages, model, "", "")
	w.Header().Set("Content-Type", "text/event-stream")
	respID := "resp_" + randID(8)
	itemID := "msg_" + randID(8)
	created := time.Now().Unix()
	full := ""
	streamedVisible := ""
	textOpened := false
	sse(w, responseCreatedEvent(respID, model, created))
	sse(w, responseInProgressEvent(respID, model, created))
	for ev := range events {
		if ev.Delta == "" {
			continue
		}
		full += ev.Delta
		visible := full
		if toolsListNonEmpty(tools) {
			visible, _ = streamableToolText(full)
		}
		if !strings.HasPrefix(visible, streamedVisible) {
			streamedVisible = ""
		}
		delta := visible[len(streamedVisible):]
		if delta == "" {
			continue
		}
		if !textOpened {
			textOpened = true
			sse(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": responseTextOutputItem("", itemID, "in_progress")})
			sse(w, map[string]any{"type": "response.content_part.added", "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		}
		streamedVisible = visible
		sse(w, map[string]any{"type": "response.output_text.delta", "delta": delta, "item_id": itemID, "output_index": 0, "content_index": 0})
	}
	if err := <-errs; err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, map[string]any{"stream": true})
		failed := responseFailedEvent(respID, model, created, err.Error())
		sse(w, failed)
		sse(w, map[string]any{"type": "response.done", "response": failed["response"]})
		sseDone(w)
		return
	}
	parsedToolCalls := parseToolCalls(full)
	if err := validateToolChoice(parsedToolCalls, tools, toolChoice); err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/responses", model, "Responses", err, map[string]any{"stream": true})
		failed := responseFailedEvent(respID, model, created, err.Error())
		sse(w, failed)
		sse(w, map[string]any{"type": "response.done", "response": failed["response"]})
		sseDone(w)
		return
	}
	cleanContent := full
	if len(parsedToolCalls) > 0 || (toolsListNonEmpty(tools) && containsToolMarkup(full)) {
		cleanContent = stripToolMarkup(full)
	}
	s.logCallSuccess(callID, "/v1/responses", model, "Responses", map[string]any{"stream": true, "output_tokens": approxTokens(cleanContent)})
	out := []map[string]any{}
	outputIndex := 0
	if textOpened || strings.TrimSpace(cleanContent) != "" || len(parsedToolCalls) == 0 {
		item := responseTextOutputItem(cleanContent, itemID, "completed")
		if !textOpened {
			sse(w, map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": responseTextOutputItem("", itemID, "in_progress")})
			sse(w, map[string]any{"type": "response.content_part.added", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		}
		sse(w, map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "text": cleanContent})
		sse(w, map[string]any{"type": "response.content_part.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": cleanContent, "annotations": []any{}}})
		sse(w, map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item})
		out = append(out, item)
		outputIndex++
	}
	for _, item := range responseFunctionCallItems(parsedToolCalls) {
		args := strAny(item["arguments"], "{}")
		sse(w, map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": map[string]any{"id": item["id"], "type": "function_call", "call_id": item["call_id"], "name": item["name"], "arguments": "", "status": "in_progress"}})
		sse(w, map[string]any{"type": "response.function_call_arguments.delta", "item_id": item["id"], "output_index": outputIndex, "delta": args})
		sse(w, map[string]any{"type": "response.function_call_arguments.done", "item_id": item["id"], "output_index": outputIndex, "arguments": args})
		sse(w, map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item})
		out = append(out, item)
		outputIndex++
	}
	completed := responseCompletedEvent(respID, model, created, out)
	sse(w, completed)
	sse(w, map[string]any{"type": "response.done", "response": completed["response"]})
	sseDone(w)
}

func (s *Server) streamAnthropicEvents(w http.ResponseWriter, r *http.Request, id *Identity, messages []map[string]any, model string, tools any, toolChoice any, callID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	events, errs := s.streamTextWithRetry(ctx, messages, model, "", "")
	w.Header().Set("Content-Type", "text/event-stream")
	msgID := "msg_" + randID(8)
	sseEvent(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": msgID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": approxTokens(messagesText(messages)), "output_tokens": 0}}})
	toolMode := toolsListNonEmpty(tools)
	textOpen := !toolMode
	current := ""
	streamed := ""
	toolStarted := false
	if textOpen {
		sseEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	}
	for ev := range events {
		if ev.Delta == "" {
			continue
		}
		current += ev.Delta
		visible := current
		if toolMode {
			var started bool
			visible, started = streamableToolText(current)
			if started {
				toolStarted = true
			}
		}
		if toolStarted && !strings.HasPrefix(visible, streamed) {
			continue
		}
		if strings.HasPrefix(visible, streamed) {
			d := visible[len(streamed):]
			if d != "" {
				if !textOpen {
					textOpen = true
					sseEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
				}
				streamed = visible
				sseEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": d}})
			}
		}
	}
	if err := <-errs; err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/messages", model, "Messages", err, map[string]any{"stream": true})
		sseEvent(w, "error", map[string]any{"type": "error", "error": map[string]any{"type": "upstream_error", "message": err.Error()}})
		return
	}
	parsedToolCalls := parseToolCalls(current)
	if err := validateToolChoice(parsedToolCalls, tools, toolChoice); err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/v1/messages", model, "Messages", err, map[string]any{"stream": true})
		sseEvent(w, "error", map[string]any{"type": "error", "error": map[string]any{"type": "invalid_tool_choice", "message": err.Error()}})
		return
	}
	s.logCallSuccess(callID, "/v1/messages", model, "Messages", map[string]any{"stream": true, "output_tokens": approxTokens(stripToolMarkup(current))})
	blocks, stopReason := anthropicContentBlocks(current, tools)
	if textOpen {
		sseEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	}
	startIndex := 0
	if textOpen {
		startIndex = 1
	}
	if stopReason == "tool_use" {
		toolIndex := startIndex
		for _, block := range blocks {
			if block["type"] == "text" {
				continue
			}
			idx := toolIndex
			toolIndex++
			sseEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}})
			payload, _ := json.Marshal(block["input"])
			sseEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(payload)}})
			sseEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		}
	}
	sseEvent(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": approxTokens(current)}})
	sseEvent(w, "message_stop", map[string]any{"type": "message_stop", "created": time.Now().Unix()})
}

func (s *Server) imageResultStream(w http.ResponseWriter, r *http.Request, id *Identity, prompt, model, size, resolution string, n int, isEdit bool, inputs [][]byte) {
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	if err := s.checkContent(prompt); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.consumeImage(id, n) {
		writeErr(w, 402, "画图额度不足")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	refs := inputs
	w.Header().Set("Content-Type", "text/event-stream")
	created := time.Now().Unix()
	for i := 0; i < n; i++ {
		items, err := s.generateImageWithPool(ctx, prompt, model, size, resolution, refs)
		if err != nil {
			s.refundImage(id, n-i)
			s.logSvc.add("call", "上游图片生成失败", map[string]any{"model": model, "error": err.Error()})
			sse(w, map[string]any{"object": "image.generation.message", "created": created, "model": model, "index": i, "total": n, "message": err.Error()})
			sseDone(w)
			return
		}
		data := []map[string]any{}
		for _, result := range items {
			rel, url, err := s.saveImage(r, result.Bytes)
			if err != nil {
				s.refundImage(id, n-i)
				sse(w, map[string]any{"error": err.Error()})
				sseDone(w)
				return
			}
			s.recordOwner(id, rel)
			s.recordPrompt(rel, prompt, isEdit)
			data = append(data, map[string]any{"url": url, "b64_json": base64.StdEncoding.EncodeToString(result.Bytes), "revised_prompt": firstNonEmpty(result.RevisedPrompt, prompt)})
			break
		}
		sse(w, map[string]any{"object": "image.generation.result", "created": created, "model": model, "index": i, "total": n, "data": data})
	}
	ssedoneAndDone(w)
}

func ssedoneAndDone(w http.ResponseWriter) {
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (s *Server) anthropicMessagesFromBody(b map[string]any) ([]map[string]any, error) {
	messages, err := s.messagesFromBodyWithFiles(b)
	if err != nil {
		return nil, err
	}
	sys := messageTextAny(b["system"])
	toolPrompt := buildToolPrompt(b["tools"])
	if strings.Contains(sys, "You are Claude Code") && toolPrompt != "" {
		toolPrompt = "Tool output adapter: when calling tools, output ONLY this XML and no prose/markdown:\n<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters></tool_call></tool_calls>"
	}
	merged := strings.TrimSpace(strings.Join([]string{sys, toolPrompt}, "\n\n"))
	if merged != "" {
		messages = append([]map[string]any{{"role": "system", "content": merged}}, messages...)
	}
	return messages, nil
}

func buildToolPrompt(tools any) string {
	arr, ok := tools.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	blocks := []string{}
	for _, raw := range arr {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		name := firstNonEmpty(strAny(tool["name"], ""), strAny(fn["name"], ""))
		if name == "" {
			continue
		}
		desc := firstNonEmpty(strAny(tool["description"], ""), strAny(fn["description"], ""))
		schema := firstNonEmpty(jsonString(tool["input_schema"]), jsonString(tool["parameters"]), jsonString(fn["input_schema"]), jsonString(fn["parameters"]), "{}")
		blocks = append(blocks, "Tool: "+name+"\nDescription: "+desc+"\nParameters: "+schema)
	}
	if len(blocks) == 0 {
		return ""
	}
	return "Available tools:\n" + strings.Join(blocks, "\n") + "\n\nTool use rules:\n- If the user asks to list/read/search files, inspect project state, run a command, or answer from local code, you MUST call a suitable tool first. Do not say you cannot access files.\n- To call tools, output ONLY this XML and no prose/markdown:\n<tool_calls><invoke name=\"TOOL_NAME\"><parameter name=\"PARAM\"><![CDATA[value]]></parameter></invoke></tool_calls>\n- Use invoke name exactly as listed. Put each argument in a parameter tag using the exact schema name. Use CDATA for strings, code, paths, JSON, or shell commands.\n- Legacy JSON tool_calls is accepted for compatibility, but XML is preferred."
}

func jsonString(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return ""
	}
	return string(b)
}

func toolsListNonEmpty(tools any) bool {
	arr, ok := tools.([]any)
	return ok && len(arr) > 0
}

func messagesText(messages []map[string]any) string {
	parts := []string{}
	for _, m := range messages {
		parts = append(parts, messageTextAny(m["content"]))
	}
	return strings.Join(parts, "\n")
}

func streamableToolText(text string) (string, bool) {
	idx := toolMarkupIndex(text)
	if idx >= 0 {
		return strings.TrimRight(text[:idx], " \t\r\n"), true
	}
	lower := strings.ToLower(text)
	markers := toolMarkupMarkers()
	safeLen := len(text)
	for i := 0; i < len(lower); i++ {
		suffix := lower[i:]
		if suffix == "" || suffix[0] != '<' {
			continue
		}
		for _, marker := range markers {
			if strings.HasPrefix(marker, suffix) {
				if i < safeLen {
					safeLen = i
				}
				break
			}
		}
	}
	if safeLen < len(text) {
		return strings.TrimRight(text[:safeLen], " \t\r\n"), false
	}
	return text, false
}

func containsToolMarkup(text string) bool {
	return toolMarkupIndex(text) >= 0 || toolJSONIndex(text) >= 0
}

func toolMarkupIndex(text string) int {
	lower := strings.ToLower(text)
	idx := -1
	for _, marker := range toolMarkupMarkers() {
		if i := strings.Index(lower, marker); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	return idx
}

func toolMarkupMarkers() []string {
	return []string{"<tool_calls", "<tool_call", "<function_call", "<invoke"}
}

func toolJSONIndex(text string) int {
	return strings.Index(strings.ToLower(text), `"tool_calls"`)
}

func stripToolMarkup(text string) string {
	visible, started := streamableToolText(text)
	if started {
		return strings.TrimSpace(visible)
	}
	cleaned := regexp.MustCompile(`(?is)<tool_calls\b[^>]*>.*?</tool_calls>|<tool_call\b[^>]*>.*?</tool_call>|<function_call\b[^>]*>.*?</function_call>|<invoke\b[^>]*>.*?</invoke>`).ReplaceAllString(text, "")
	cleaned = stripJSONToolCalls(cleaned)
	return strings.TrimSpace(cleaned)
}

func anthropicContentBlocks(text string, tools any) ([]map[string]any, string) {
	toolMode := toolsListNonEmpty(tools)
	calls := []map[string]any{}
	if toolMode {
		calls = parseToolCalls(text)
	}
	clean := stripToolMarkup(text)
	blocks := []map[string]any{}
	if clean != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": clean})
	}
	for _, call := range calls {
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": "toolu_" + randID(8), "name": call["name"], "input": call["input"]})
	}
	if len(calls) > 0 {
		return blocks, "tool_use"
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks, "end_turn"
}

func parseToolCalls(text string) []map[string]any {
	text = stripToolCodeContexts(text)
	out := []map[string]any{}
	seen := map[string]bool{}
	appendCall := func(name string, input map[string]any) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if input == nil {
			input = map[string]any{}
		}
		keyBytes, _ := json.Marshal(map[string]any{"name": name, "input": input})
		key := string(keyBytes)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, map[string]any{"name": name, "input": input})
	}
	for _, call := range parseJSONToolCalls(text) {
		appendCall(strAny(call["name"], ""), mapAny(call["input"]))
	}
	for _, call := range parseCanonicalToolCalls(text) {
		appendCall(strAny(call["name"], ""), mapAny(call["input"]))
	}
	for _, block := range toolCallBlocks(text) {
		name := firstNonEmpty(xmlValue(block, "tool_name"), xmlValue(block, "name"), xmlValue(block, "function"))
		params := firstNonEmpty(xmlValue(block, "parameters"), xmlValue(block, "input"), xmlValue(block, "arguments"), "{}")
		appendCall(name, parseToolParams(params))
	}
	return out
}

func stripFencedCodeBlocks(text string) string {
	return regexp.MustCompile("(?is)```.*?```").ReplaceAllString(text, "")
}

func parseJSONToolCalls(text string) []map[string]any {
	spans := jsonToolCallSpans(text)
	out := []map[string]any{}
	for _, span := range spans {
		var payload map[string]any
		if json.Unmarshal([]byte(span), &payload) != nil {
			continue
		}
		items, ok := payload["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name := firstNonEmpty(strAny(item["tool_name"], ""), strAny(item["name"], ""))
			input := mapAny(firstNonEmptyAny(item["parameters"], item["arguments"], item["input"]))
			if input == nil {
				input = parseToolArgumentValue(firstNonEmptyAny(item["parameters"], item["arguments"], item["input"]))
			}
			out = append(out, map[string]any{"name": name, "input": input})
		}
	}
	return out
}

func stripJSONToolCalls(text string) string {
	spans := jsonToolCallSpansWithBounds(text)
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, span := range spans {
		if span.start < last || span.end > len(text) {
			continue
		}
		b.WriteString(text[last:span.start])
		last = span.end
	}
	b.WriteString(text[last:])
	return b.String()
}

type textSpan struct {
	start int
	end   int
}

func jsonToolCallSpans(text string) []string {
	bounds := jsonToolCallSpansWithBounds(text)
	out := make([]string, 0, len(bounds))
	for _, span := range bounds {
		out = append(out, text[span.start:span.end])
	}
	return out
}

func jsonToolCallSpansWithBounds(text string) []textSpan {
	out := []textSpan{}
	lower := strings.ToLower(text)
	searchFrom := 0
	for {
		idx := strings.Index(lower[searchFrom:], `"tool_calls"`)
		if idx < 0 {
			break
		}
		idx += searchFrom
		start := strings.LastIndex(text[:idx], "{")
		if start < 0 {
			searchFrom = idx + len(`"tool_calls"`)
			continue
		}
		end := matchingJSONEnd(text, start)
		if end < 0 {
			searchFrom = idx + len(`"tool_calls"`)
			continue
		}
		candidate := text[start:end]
		var payload map[string]any
		if json.Unmarshal([]byte(candidate), &payload) == nil {
			if _, ok := payload["tool_calls"]; ok {
				out = append(out, textSpan{start: start, end: end})
				searchFrom = end
				continue
			}
		}
		searchFrom = idx + len(`"tool_calls"`)
	}
	return out
}

func matchingJSONEnd(text string, start int) int {
	if start < 0 || start >= len(text) || text[start] != '{' {
		return -1
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func firstNonEmptyAny(values ...any) any {
	for _, value := range values {
		if strings.TrimSpace(strAny(value, "")) != "" {
			return value
		}
		if m, ok := value.(map[string]any); ok && len(m) > 0 {
			return m
		}
	}
	return nil
}

func mapAny(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return nil
}

func parseToolArgumentValue(value any) map[string]any {
	if value == nil {
		return nil
	}
	s := strings.TrimSpace(strAny(value, ""))
	if s == "" {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) == nil {
		return obj
	}
	return nil
}

func toolCallBlocks(text string) []string {
	blocks := []string{}
	for _, tag := range []string{"tool_call", "function_call", "invoke"} {
		pattern := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*>`)
		starts := pattern.FindAllStringIndex(text, -1)
		for _, start := range starts {
			end := matchingCloseTag(text, tag, start[1])
			if end >= 0 {
				blocks = append(blocks, text[start[1]:end])
			}
		}
	}
	return blocks
}

func matchingCloseTag(text, tag string, offset int) int {
	openRe := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*>`)
	closeRe := regexp.MustCompile(`(?is)</` + tag + `>`)
	pos := offset
	depth := 1
	for depth > 0 {
		nextOpen := openRe.FindStringIndex(text[pos:])
		nextClose := closeRe.FindStringIndex(text[pos:])
		if nextClose == nil {
			return -1
		}
		if nextOpen != nil && nextOpen[0] < nextClose[0] {
			depth++
			pos += nextOpen[1]
			continue
		}
		depth--
		if depth == 0 {
			return pos + nextClose[0]
		}
		pos += nextClose[1]
	}
	return -1
}

func xmlValue(text, tag string) string {
	re := regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(tag) + `\b[^>]*>(.*?)</` + regexp.QuoteMeta(tag) + `>`)
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	value := strings.TrimSpace(m[1])
	if cm := regexp.MustCompile(`(?is)^<!\[CDATA\[(.*?)]]>$`).FindStringSubmatch(value); len(cm) > 1 {
		value = cm[1]
	}
	return strings.TrimSpace(html.UnescapeString(value))
}

func parseToolParams(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) == nil {
		return obj
	}
	obj = map[string]any{}
	re := regexp.MustCompile(`(?is)<([\w.-]+)\b[^>]*>(.*?)</\1>`)
	for _, m := range re.FindAllStringSubmatch(raw, -1) {
		obj[m[1]] = parseToolValue(m[2])
	}
	return obj
}

func parseToolValue(raw string) any {
	value := strings.TrimSpace(raw)
	if cm := regexp.MustCompile(`(?is)^<!\[CDATA\[(.*?)]]>$`).FindStringSubmatch(value); len(cm) > 1 {
		value = cm[1]
	}
	value = strings.TrimSpace(html.UnescapeString(value))
	var parsed any
	if json.Unmarshal([]byte(value), &parsed) == nil {
		return parsed
	}
	return value
}

func responseTextOutputItem(text, itemID, status string) map[string]any {
	if itemID == "" {
		itemID = "msg_" + randID(8)
	}
	return map[string]any{"id": itemID, "type": "message", "status": status, "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func responseImageOutputItems(prompt string, data []map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, item := range data {
		b64 := strings.TrimSpace(strAny(item["b64_json"], ""))
		if b64 == "" {
			continue
		}
		out = append(out, map[string]any{"id": fmt.Sprintf("ig_%d", len(out)+1), "type": "image_generation_call", "status": "completed", "result": b64, "revised_prompt": firstNonEmpty(strAny(item["revised_prompt"], ""), prompt)})
	}
	return out
}

func responseCreatedEvent(id, model string, created int64) map[string]any {
	return map[string]any{"type": "response.created", "response": responseBase(id, model, created, "in_progress", []any{}, nil)}
}

func responseInProgressEvent(id, model string, created int64) map[string]any {
	return map[string]any{"type": "response.in_progress", "response": responseBase(id, model, created, "in_progress", []any{}, nil)}
}

func responseCompletedEvent(id, model string, created int64, output []map[string]any) map[string]any {
	return map[string]any{"type": "response.completed", "response": responseBase(id, model, created, "completed", output, nil)}
}

func responseFailedEvent(id, model string, created int64, message string) map[string]any {
	err := map[string]any{"message": message, "type": "upstream_error", "code": "upstream_error"}
	return map[string]any{"type": "response.failed", "response": responseBase(id, model, created, "failed", []any{}, err)}
}

func responseBase(id, model string, created int64, status string, output any, err any) map[string]any {
	return map[string]any{"id": id, "object": "response", "created_at": created, "status": status, "error": err, "incomplete_details": nil, "model": model, "output": output, "parallel_tool_calls": false}
}

func hasResponseImageGenerationTool(b map[string]any) bool {
	if tools, ok := b["tools"].([]any); ok {
		for _, tool := range tools {
			if m, ok := tool.(map[string]any); ok && strings.TrimSpace(strAny(m["type"], "")) == "image_generation" {
				return true
			}
		}
	}
	if m, ok := b["tool_choice"].(map[string]any); ok && strings.TrimSpace(strAny(m["type"], "")) == "image_generation" {
		return true
	}
	return false
}

func extractResponsePrompt(input any) string {
	switch v := input.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		role := strings.ToLower(strings.TrimSpace(strAny(v["role"], "")))
		if role != "" && role != "user" {
			return ""
		}
		return strings.TrimSpace(messageTextAny(v["content"]))
	case []any:
		parts := []string{}
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(strAny(m["type"], "")) == "input_text" {
				if t := strings.TrimSpace(strAny(m["text"], "")); t != "" {
					parts = append(parts, t)
				}
				continue
			}
			role := strings.ToLower(strings.TrimSpace(strAny(m["role"], "")))
			if role != "" && role != "user" {
				continue
			}
			if t := strings.TrimSpace(messageTextAny(m["content"])); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func extractResponseImages(input any) [][]byte {
	switch v := input.(type) {
	case map[string]any:
		return extractImagesFromContent(v["content"])
	case []any:
		for i := len(v) - 1; i >= 0; i-- {
			m, ok := v[i].(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(strAny(m["type"], "")) == "input_image" {
				if img := extractImagesFromContent([]any{m}); len(img) > 0 {
					return img
				}
			}
			if img := extractImagesFromContent(m["content"]); len(img) > 0 {
				return img
			}
		}
	}
	return nil
}

func countMessageTokens(messages []map[string]any) int {
	total := 0
	for _, m := range messages {
		total += 4 + approxTokens(strAny(m["role"], "")) + approxTokens(messageTextAny(m["content"]))
	}
	if total > 0 {
		total += 2
	}
	return total
}

func approxTokens(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// 轻量估算：英文约 4 字符/token，中文按 rune 数更接近。
	runes := []rune(s)
	if len(runes) < len(s) {
		return maxInt(1, len(runes)/2)
	}
	return maxInt(1, len(s)/4)
}

func isSupportedImageModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "gpt-image-2" || m == "codex-gpt-image-2" {
		return true
	}
	return strings.HasSuffix(m, "-codex-gpt-image-2")
}

func isImageChatRequest(b map[string]any) bool {
	if isSupportedImageModel(strAny(b["model"], "")) {
		return true
	}
	if mods, ok := b["modalities"].([]any); ok {
		for _, item := range mods {
			if strings.ToLower(strings.TrimSpace(strAny(item, ""))) == "image" {
				return true
			}
		}
	}
	return false
}

func extractChatPrompt(b map[string]any) string {
	if p := strings.TrimSpace(strAny(b["prompt"], "")); p != "" {
		return p
	}
	parts := []string{}
	if msgs, ok := b["messages"].([]any); ok {
		for _, item := range msgs {
			m, ok := item.(map[string]any)
			if !ok || strings.ToLower(strings.TrimSpace(strAny(m["role"], ""))) != "user" {
				continue
			}
			if t := strings.TrimSpace(messageTextAny(m["content"])); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func extractChatImages(b map[string]any) [][]byte {
	msgs, ok := b["messages"].([]any)
	if !ok {
		return nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(strAny(m["role"], ""))) != "user" {
			continue
		}
		images := extractImagesFromContent(m["content"])
		if len(images) > 0 {
			return images
		}
	}
	return nil
}

func extractImagesFromContent(content any) [][]byte {
	parts, ok := content.([]any)
	if !ok {
		return nil
	}
	out := [][]byte{}
	for _, item := range parts {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.TrimSpace(strAny(m["type"], ""))
		dataURL := ""
		switch typ {
		case "image_url":
			if obj, ok := m["image_url"].(map[string]any); ok {
				dataURL = strAny(obj["url"], "")
			} else {
				dataURL = strAny(m["image_url"], "")
			}
		case "input_image":
			dataURL = strAny(m["image_url"], "")
		}
		if !strings.HasPrefix(dataURL, "data:") || !strings.Contains(dataURL, ",") {
			continue
		}
		payload := strings.SplitN(dataURL, ",", 2)[1]
		if b, err := base64.StdEncoding.DecodeString(payload); err == nil && len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

func buildChatImageMarkdown(data []map[string]any) string {
	parts := []string{}
	for i, item := range data {
		if u := strings.TrimSpace(strAny(item["url"], "")); u != "" {
			parts = append(parts, fmt.Sprintf("![image_%d](%s)", i+1, u))
			continue
		}
		if b64 := strings.TrimSpace(strAny(item["b64_json"], "")); b64 != "" {
			parts = append(parts, fmt.Sprintf("![image_%d](data:image/png;base64,%s)", i+1, b64))
		}
	}
	if len(parts) == 0 {
		return "Image generation completed."
	}
	return strings.Join(parts, "\n\n")
}

func extractPrompt(b map[string]any) string {
	if v := strings.TrimSpace(strAny(b["prompt"], "")); v != "" {
		return v
	}
	if v := strings.TrimSpace(strAny(b["input"], "")); v != "" {
		return v
	}
	if msgs, ok := b["messages"].([]any); ok && len(msgs) > 0 {
		if m, ok := msgs[len(msgs)-1].(map[string]any); ok {
			return strings.TrimSpace(messageTextAny(m["content"]))
		}
	}
	return ""
}
