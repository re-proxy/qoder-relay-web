package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

/*
对外网关：OpenAI Chat Completions / Responses 与 Anthropic Messages 兼容层。

同一进程服务两个档位（国内 / 国际），档位由请求决定：
 1. 请求头 X-Qoder-Edition
 2. 请求体 edition 字段
 3. 模型名前缀 domestic/ 或 intl/
 4. 都没有则用配置的默认档位
*/

var errNoAccount = errors.New("没有可用账号，请先在管理页添加并启用账号")

// callSpec 描述一次网关调用。
type callSpec struct {
	edition      string
	model        string
	messages     []ChatMessage
	tools        any
	effort       string
	thinkingType string
	budget       int64
	keyName      string
}

func (c callSpec) chatRequest(acc Account) ChatRequest {
	return ChatRequest{
		Account:         acc,
		Edition:         editionOf(c.edition),
		Model:           c.model,
		Messages:        c.messages,
		Tools:           c.tools,
		ReasoningEffort: c.effort,
		ThinkingType:    c.thinkingType,
		ThinkingBudget:  c.budget,
	}
}

/* ---------------- 错误与鉴权 ---------------- */

func openAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "api_error",
			"code":    code,
			"status":  status,
		},
	})
}

func anthropicError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": message},
	})
}

// gatewayAuth 校验 API Key。未配置任何密钥时放行（首次启动的便捷路径）。
func (a *App) gatewayAuth(w http.ResponseWriter, r *http.Request, anthropic bool) (string, bool) {
	if !a.store.HasAnyKey() {
		return "", true
	}
	token := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		token = strings.TrimSpace(h[7:])
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if name, ok := a.store.AuthorizeKey(token); ok {
		return name, true
	}
	if anthropic {
		anthropicError(w, http.StatusUnauthorized, "authentication_error", "API Key 无效")
	} else {
		openAIError(w, http.StatusUnauthorized, "invalid_api_key", "API Key 无效")
	}
	return "", false
}

// resolveEdition 决定本次请求走哪个档位。
func (a *App) resolveEdition(r *http.Request, body map[string]any) (string, string) {
	edition := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Qoder-Edition")))
	model := str(body, "model")
	if edition == "" {
		edition = strings.ToLower(strings.TrimSpace(str(body, "edition")))
	}
	if edition == "" {
		lower := strings.ToLower(model)
		switch {
		case strings.HasPrefix(lower, "intl/"), strings.HasPrefix(lower, "international/"):
			edition = "international"
			model = model[strings.Index(model, "/")+1:]
		case strings.HasPrefix(lower, "cn/"), strings.HasPrefix(lower, "domestic/"):
			edition = "domestic"
			model = model[strings.Index(model, "/")+1:]
		}
	}
	if edition != "international" && edition != "domestic" {
		edition = a.cfg.DefaultEdition
	}
	if edition == "" {
		edition = "domestic"
	}
	return edition, model
}

/* ---------------- 请求解析 ---------------- */

func readBody(r *http.Request) (map[string]any, error) {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<20))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

// contentText 把 OpenAI 风格 content（字符串或分片数组）拍平成文本。
func contentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m := obj(part)
			if m == nil {
				continue
			}
			if t := str(m, "text"); t != "" {
				b.WriteString(t)
				continue
			}
			if m["type"] == "input_text" {
				b.WriteString(str(m, "text"))
			}
		}
		return b.String()
	}
	return ""
}

func messagesFromOpenAI(raw []any) []ChatMessage {
	out := make([]ChatMessage, 0, len(raw))
	for _, item := range raw {
		m := obj(item)
		if m == nil {
			continue
		}
		role := str(m, "role")
		msg := ChatMessage{Role: role, Content: m["content"], ToolCallID: str(m, "tool_call_id")}
		if calls := list(m["tool_calls"]); calls != nil {
			msg.ToolCalls = parseToolCalls(calls)
		}
		out = append(out, msg)
	}
	return out
}

func messagesFromAnthropic(system any, raw []any) []ChatMessage {
	out := make([]ChatMessage, 0, len(raw)+1)
	if sys := contentText(system); strings.TrimSpace(sys) != "" {
		out = append(out, ChatMessage{Role: "system", Content: sys})
	}
	for _, item := range raw {
		m := obj(item)
		if m == nil {
			continue
		}
		role := str(m, "role")
		blocks := list(m["content"])
		if blocks == nil {
			out = append(out, ChatMessage{Role: role, Content: str(m, "content")})
			continue
		}
		var texts []string
		var parts []any
		var calls []ToolCall
		for _, blk := range blocks {
			b := obj(blk)
			if b == nil {
				continue
			}
			switch str(b, "type") {
			case "text":
				texts = append(texts, str(b, "text"))
				parts = append(parts, map[string]any{"type": "text", "text": str(b, "text")})
			case "image":
				if src := obj(b["source"]); src != nil && str(src, "type") == "base64" {
					url := "data:" + str(src, "media_type") + ";base64," + str(src, "data")
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			case "tool_use":
				args, _ := json.Marshal(b["input"])
				calls = append(calls, ToolCall{
					Index:    len(calls),
					ID:       str(b, "id"),
					Type:     "function",
					Function: ToolCallFunc{Name: str(b, "name"), Arguments: string(args)},
				})
			case "tool_result":
				out = append(out, ChatMessage{
					Role:       "tool",
					Content:    contentText(b["content"]),
					ToolCallID: str(b, "tool_use_id"),
				})
			}
		}
		if len(parts) > 1 {
			out = append(out, ChatMessage{Role: role, Content: parts, ToolCalls: calls})
		} else if len(parts) == 1 {
			out = append(out, ChatMessage{Role: role, Content: parts[0], ToolCalls: calls})
		} else {
			out = append(out, ChatMessage{Role: role, Content: strings.Join(texts, "\n"), ToolCalls: calls})
		}
	}
	return out
}

func messagesFromResponses(instructions string, input any) []ChatMessage {
	out := make([]ChatMessage, 0, 8)
	if strings.TrimSpace(instructions) != "" {
		out = append(out, ChatMessage{Role: "system", Content: instructions})
	}
	switch v := input.(type) {
	case string:
		out = append(out, ChatMessage{Role: "user", Content: v})
	case []any:
		for _, item := range v {
			m := obj(item)
			if m == nil {
				if s, ok := item.(string); ok {
					out = append(out, ChatMessage{Role: "user", Content: s})
				}
				continue
			}
			switch str(m, "type") {
			case "function_call_output":
				out = append(out, ChatMessage{Role: "tool", Content: str(m, "output"), ToolCallID: str(m, "call_id")})
			case "function_call":
				out = append(out, ChatMessage{
					Role:    "assistant",
					Content: "",
					ToolCalls: []ToolCall{{
						Index:    0,
						ID:       str(m, "call_id"),
						Type:     "function",
						Function: ToolCallFunc{Name: str(m, "name"), Arguments: str(m, "arguments")},
					}},
				})
			default:
				role := str(m, "role")
				if role == "" {
					role = "user"
				}
				out = append(out, ChatMessage{Role: role, Content: m["content"]})
			}
		}
	}
	return out
}

// anthropicToolsToOpenAI 把 {name,input_schema} 转成 OpenAI function 工具。
func anthropicToolsToOpenAI(raw []any) any {
	if raw == nil {
		return nil
	}
	out := make([]any, 0, len(raw))
	for _, item := range raw {
		m := obj(item)
		if m == nil || str(m, "name") == "" {
			continue
		}
		fn := map[string]any{"name": str(m, "name")}
		if desc := str(m, "description"); desc != "" {
			fn["description"] = desc
		}
		if schema := obj(m["input_schema"]); schema != nil {
			fn["parameters"] = schema
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

/* ---------------- 账号选择与重试 ---------------- */

func isRequestError(status int) bool {
	switch status {
	case 400, 404, 413, 415, 422:
		return true
	}
	return false
}

func statusOf(err error) int {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue.Status
	}
	return 0
}

// tokenExpiring 判断登录 token 是否即将过期（5 分钟内）。
func tokenExpiring(a Account) bool {
	if a.TokenExpireAt <= 0 {
		return false
	}
	return a.TokenExpireAt-time.Now().UnixMilli() < 5*60*1000
}

// refreshAccount 用 refresh_token / PAT 续期并写回存储。
func (a *App) refreshAccount(ctx context.Context, acc Account) (Account, bool) {
	if acc.RefreshToken == "" && acc.PAT == "" {
		return acc, false
	}
	ed := editionOf(acc.Edition)
	out, err := RefreshAccountToken(ctx, ed, acc)
	if err != nil {
		logf("账号 %s 续期失败: %v", accountDisplayName(acc), err)
		return acc, false
	}
	updated, ok, err := a.store.UpdateAccount(acc.ID, func(x *Account) {
		x.SecurityToken = out.SecurityToken
		if out.RefreshToken != "" {
			x.RefreshToken = out.RefreshToken
		}
		x.TokenExpireAt = out.TokenExpireAt
		if out.RefreshTokenExpireAt > 0 {
			x.RefreshTokenExpireAt = out.RefreshTokenExpireAt
		}
		x.Status = "active"
		x.LastError = ""
		x.UpdatedAt = time.Now().UnixMilli()
	})
	if err != nil || !ok {
		return acc, false
	}
	return updated, true
}

// openStream 选账号并建立上游流。首个事件前允许换号重试。
func (a *App) openStream(ctx context.Context, call callSpec) (<-chan chatEvent, Account, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		acc, ok := a.store.PickAccount(call.edition, true)
		if !ok {
			if lastErr != nil {
				return nil, Account{}, lastErr
			}
			return nil, Account{}, errNoAccount
		}
		if tokenExpiring(acc) {
			if refreshed, ok := a.refreshAccount(ctx, acc); ok {
				acc = refreshed
			}
		}
		req := call.chatRequest(acc)
		ch, err := streamChat(ctx, req)
		if err == nil {
			return ch, acc, nil
		}
		lastErr = err
		status := statusOf(err)
		if isRequestError(status) {
			return nil, acc, err
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			if refreshed, ok := a.refreshAccount(ctx, acc); ok {
				req.Account = refreshed
				if ch, err2 := streamChat(ctx, req); err2 == nil {
					return ch, refreshed, nil
				} else {
					lastErr = err2
					status = statusOf(err2)
					acc = refreshed
				}
			}
		}
		a.store.MarkFailure(acc.ID, status, err.Error())
	}
	return nil, Account{}, lastErr
}

// runChat 跑完一次非流式请求，返回汇总结果。
type chatResult struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	Usage        TokenUsage
	FinishReason string
}

func drainChat(ch <-chan chatEvent) chatResult {
	var res chatResult
	for ev := range ch {
		switch ev.Type {
		case "content":
			res.Content += ev.Text
		case "reasoning":
			res.Reasoning += ev.Text
		case "tool_calls":
			res.ToolCalls = mergeToolCalls(res.ToolCalls, ev.ToolCalls)
		case "usage":
			if ev.Usage != nil {
				res.Usage = *ev.Usage
			}
		case "error":
			res.FinishReason = "error"
			res.Content += ""
		}
	}
	if len(res.ToolCalls) > 0 {
		res.FinishReason = "tool_calls"
	} else if res.FinishReason != "error" {
		res.FinishReason = "stop"
	}
	return res
}

func estimateTokens(text string) int64 {
	n := int64(len([]rune(text)))
	return (n + 3) / 4
}

func messagesText(msgs []ChatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(contentText(m.Content))
		b.WriteString("\n")
	}
	return b.String()
}

// fillUsage 在上游未返回 usage 时按字符估算。
func fillUsage(u *TokenUsage, msgs []ChatMessage, output string) {
	if u.PromptTokens == 0 {
		u.PromptTokens = estimateTokens(messagesText(msgs))
	}
	if u.CompletionTokens == 0 {
		u.CompletionTokens = estimateTokens(output)
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
}

func (a *App) recordUsage(call callSpec, acc Account, u TokenUsage, started time.Time, err error) {
	entry := Usage{
		Time:       time.Now().UnixMilli(),
		Model:      call.model,
		Account:    accountDisplayName(acc),
		AccountID:  acc.ID,
		Edition:    call.edition,
		KeyName:    call.keyName,
		Input:      u.PromptTokens,
		Output:     u.CompletionTokens,
		CacheRead:  u.CachedTokens,
		Success:    err == nil,
		Status:     http.StatusOK,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if err != nil {
		entry.Error = truncateRunes(err.Error(), 200)
		if s := statusOf(err); s > 0 {
			entry.Status = s
		} else {
			entry.Status = http.StatusBadGateway
		}
	}
	a.store.Record(entry)
}

func accountDisplayName(a Account) string {
	for _, v := range []string{a.Name, a.Email, a.Phone, a.Label, a.UID, a.ID} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "未知账号"
}

/* ---------------- /v1/chat/completions ---------------- */

func (a *App) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	keyName, ok := a.gatewayAuth(w, r, false)
	if !ok {
		return
	}
	body, err := readBody(r)
	if err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON")
		return
	}
	msgs := messagesFromOpenAI(list(body["messages"]))
	if len(msgs) == 0 {
		openAIError(w, http.StatusBadRequest, "invalid_request", "messages 不能为空")
		return
	}
	edition, model := a.resolveEdition(r, body)
	call := callSpec{
		edition:      edition,
		model:        model,
		messages:     msgs,
		tools:        body["tools"],
		effort:       str(body, "reasoning_effort"),
		thinkingType: str(obj(body["thinking"]), "type"),
		keyName:      keyName,
	}
	if f, ok := firstNumber(obj(body["thinking"]), "budget_tokens"); ok {
		call.budget = int64(f)
	}
	stream := boolean(body, "stream", false)
	started := time.Now()

	ch, acc, err := a.openStream(r.Context(), call)
	if err != nil {
		a.recordUsage(call, acc, TokenUsage{}, started, err)
		openAIError(w, statusOr(statusOf(err), http.StatusServiceUnavailable), "upstream_error", err.Error())
		return
	}

	id := "chatcmpl-" + uuidCompact()
	created := time.Now().Unix()

	if !stream {
		res := drainChat(ch)
		u := res.Usage
		fillUsage(&u, msgs, res.Content)
		a.recordUsage(call, acc, u, started, nil)
		message := map[string]any{"role": "assistant", "content": res.Content}
		if res.Reasoning != "" {
			message["reasoning_content"] = res.Reasoning
		}
		if len(res.ToolCalls) > 0 {
			message["tool_calls"] = res.ToolCalls
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": res.FinishReason,
			}},
			"usage": map[string]any{
				"prompt_tokens":     u.PromptTokens,
				"completion_tokens": u.CompletionTokens,
				"total_tokens":      u.TotalTokens,
			},
		})
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	})

	var usage TokenUsage
	var content strings.Builder
	var calls []ToolCall
	for ev := range ch {
		switch ev.Type {
		case "content":
			content.WriteString(ev.Text)
			send(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ev.Text}, "finish_reason": nil}},
			})
		case "reasoning":
			send(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": ev.Text}, "finish_reason": nil}},
			})
		case "tool_calls":
			calls = mergeToolCalls(calls, ev.ToolCalls)
			send(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": ev.ToolCalls}, "finish_reason": nil}},
			})
		case "usage":
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case "error":
			send(map[string]any{"error": map[string]any{"message": ev.Error, "type": "api_error"}})
			a.recordUsage(call, acc, usage, started, fmt.Errorf("%s", ev.Error))
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	send(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	fillUsage(&usage, msgs, content.String())
	a.recordUsage(call, acc, usage, started, nil)
}

/* ---------------- /v1/responses ---------------- */

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request) {
	keyName, ok := a.gatewayAuth(w, r, false)
	if !ok {
		return
	}
	body, err := readBody(r)
	if err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON")
		return
	}
	msgs := messagesFromResponses(str(body, "instructions"), body["input"])
	if len(msgs) == 0 {
		openAIError(w, http.StatusBadRequest, "invalid_request", "input 不能为空")
		return
	}
	edition, model := a.resolveEdition(r, body)
	call := callSpec{
		edition:      edition,
		model:        model,
		messages:     msgs,
		tools:        body["tools"],
		effort:       str(obj(body["reasoning"]), "effort"),
		thinkingType: str(obj(body["thinking"]), "type"),
		keyName:      keyName,
	}
	stream := boolean(body, "stream", false)
	started := time.Now()

	ch, acc, err := a.openStream(r.Context(), call)
	if err != nil {
		a.recordUsage(call, acc, TokenUsage{}, started, err)
		openAIError(w, statusOr(statusOf(err), http.StatusServiceUnavailable), "upstream_error", err.Error())
		return
	}

	id := "resp_" + uuidCompact()
	created := time.Now().Unix()

	if !stream {
		res := drainChat(ch)
		u := res.Usage
		fillUsage(&u, msgs, res.Content)
		a.recordUsage(call, acc, u, started, nil)
		output := make([]any, 0, 3)
		if res.Reasoning != "" {
			output = append(output, map[string]any{
				"type": "reasoning", "id": "rs_" + uuidCompact(), "summary": []any{map[string]any{"type": "summary_text", "text": res.Reasoning}},
			})
		}
		content := make([]any, 0, 1)
		if res.Content != "" {
			content = append(content, map[string]any{"type": "output_text", "text": res.Content, "annotations": []any{}})
		}
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + uuidCompact(), "role": "assistant", "status": "completed", "content": content,
		})
		for i, tc := range res.ToolCalls {
			output = append(output, map[string]any{
				"type": "function_call", "id": fmt.Sprintf("fc_%d_%s", i, uuidCompact()),
				"call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments, "status": "completed",
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         id,
			"object":     "response",
			"created_at": created,
			"model":      model,
			"status":     "completed",
			"output":     output,
			"usage": map[string]any{
				"input_tokens":  u.PromptTokens,
				"output_tokens": u.CompletionTokens,
				"total_tokens":  u.TotalTokens,
			},
		})
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(event string, payload any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	itemID := "msg_" + uuidCompact()
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "created_at": created, "model": model, "status": "in_progress"}})

	var usage TokenUsage
	var content strings.Builder
	var calls []ToolCall
	for ev := range ch {
		switch ev.Type {
		case "content":
			content.WriteString(ev.Text)
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": itemID, "delta": ev.Text})
		case "reasoning":
			emit("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": itemID, "delta": ev.Text})
		case "tool_calls":
			calls = mergeToolCalls(calls, ev.ToolCalls)
			for _, tc := range ev.ToolCalls {
				emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": itemID, "call_id": tc.ID, "delta": tc.Function.Arguments})
			}
		case "usage":
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case "error":
			emit("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": id, "status": "failed", "error": map[string]any{"message": ev.Error}}})
			a.recordUsage(call, acc, usage, started, fmt.Errorf("%s", ev.Error))
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
	}
	emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": itemID, "text": content.String()})
	emit("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed"}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	fillUsage(&usage, msgs, content.String())
	a.recordUsage(call, acc, usage, started, nil)
}

/* ---------------- /v1/messages（Anthropic） ---------------- */

func (a *App) handleMessages(w http.ResponseWriter, r *http.Request) {
	keyName, ok := a.gatewayAuth(w, r, true)
	if !ok {
		return
	}
	body, err := readBody(r)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON")
		return
	}
	msgs := messagesFromAnthropic(body["system"], list(body["messages"]))
	if len(msgs) == 0 {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}
	edition, model := a.resolveEdition(r, body)
	call := callSpec{
		edition:      edition,
		model:        model,
		messages:     msgs,
		tools:        anthropicToolsToOpenAI(list(body["tools"])),
		effort:       str(obj(body["output_config"]), "effort"),
		thinkingType: str(obj(body["thinking"]), "type"),
		keyName:      keyName,
	}
	if f, ok := firstNumber(obj(body["thinking"]), "budget_tokens"); ok {
		call.budget = int64(f)
	}
	stream := boolean(body, "stream", false)
	started := time.Now()

	ch, acc, err := a.openStream(r.Context(), call)
	if err != nil {
		a.recordUsage(call, acc, TokenUsage{}, started, err)
		kind := "api_error"
		if statusOf(err) == http.StatusUnauthorized {
			kind = "authentication_error"
		}
		anthropicError(w, statusOr(statusOf(err), http.StatusBadGateway), kind, err.Error())
		return
	}

	id := "msg_" + uuidCompact()

	if !stream {
		res := drainChat(ch)
		u := res.Usage
		fillUsage(&u, msgs, res.Content)
		a.recordUsage(call, acc, u, started, nil)
		blocks := make([]any, 0, 3)
		if res.Reasoning != "" {
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": res.Reasoning, "signature": ""})
		}
		if res.Content != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": res.Content})
		}
		for _, tc := range res.ToolCalls {
			var input any = map[string]any{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
		}
		stop := "end_turn"
		if len(res.ToolCalls) > 0 {
			stop = "tool_use"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       blocks,
			"stop_reason":   stop,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  u.PromptTokens,
				"output_tokens": u.CompletionTokens,
			},
		})
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(event string, payload any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	var usage TokenUsage
	var content strings.Builder
	var calls []ToolCall
	blockIndex := 0
	textOpen := false
	thinkingOpen := false
	for ev := range ch {
		switch ev.Type {
		case "reasoning":
			if !thinkingOpen {
				emit("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
				thinkingOpen = true
			}
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text}})
		case "content":
			if thinkingOpen {
				emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
				thinkingOpen = false
				blockIndex++
			}
			if !textOpen {
				emit("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "text", "text": ""}})
				textOpen = true
			}
			content.WriteString(ev.Text)
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "text_delta", "text": ev.Text}})
		case "tool_calls":
			calls = mergeToolCalls(calls, ev.ToolCalls)
			for _, tc := range ev.ToolCalls {
				if thinkingOpen {
					emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
					thinkingOpen = false
					blockIndex++
				}
				if textOpen {
					emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
					textOpen = false
					blockIndex++
				}
				emit("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}}})
				emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
				emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
				blockIndex++
			}
		case "usage":
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case "error":
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": ev.Error}})
			a.recordUsage(call, acc, usage, started, fmt.Errorf("%s", ev.Error))
			return
		}
	}
	if thinkingOpen || textOpen {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
	}
	stop := "end_turn"
	if len(calls) > 0 {
		stop = "tool_use"
	}
	fillUsage(&usage, msgs, content.String())
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
	a.recordUsage(call, acc, usage, started, nil)
}

func statusOr(status, fallback int) int {
	if status > 0 {
		return status
	}
	return fallback
}

/* ---------------- /v1/models ---------------- */

func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.gatewayAuth(w, r, false); !ok {
		return
	}
	edition, _ := a.resolveEdition(r, map[string]any{})
	models := a.modelCatalog(r.Context(), edition)
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":           m.ID,
			"object":       "model",
			"created":      0,
			"owned_by":     m.OwnedBy,
			"display_name": m.DisplayName,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// modelCatalog 合并默认模型、上游目录与管理页覆盖项。
func (a *App) modelCatalog(ctx context.Context, edition string) []ModelInfo {
	merged := map[string]ModelInfo{}
	order := make([]string, 0, 24)
	add := func(m ModelInfo) {
		if _, seen := merged[m.ID]; !seen {
			order = append(order, m.ID)
		}
		merged[m.ID] = m
	}
	for _, m := range defaultModelList {
		add(m)
	}
	if acc, ok := a.store.PickAccount(edition, false); ok {
		ed := editionOf(edition)
		for _, m := range fetchUpstreamModels(ctx, ed, acc) {
			add(m)
		}
	}
	settings := a.store.Models()
	out := make([]ModelInfo, 0, len(order))
	for _, id := range order {
		m := merged[id]
		if s, ok := settings[id]; ok {
			if s.Disabled {
				continue
			}
			if s.Alias != "" {
				m.DisplayName = s.Alias
			}
		}
		out = append(out, m)
	}
	return out
}

// handleCountTokens 是 Anthropic 的 token 预估端点。
func (a *App) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.gatewayAuth(w, r, true); !ok {
		return
	}
	body, err := readBody(r)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON")
		return
	}
	msgs := messagesFromAnthropic(body["system"], list(body["messages"]))
	extras := ""
	if sys := body["system"]; sys != nil {
		raw, _ := json.Marshal(sys)
		extras += string(raw)
	}
	if tools := body["tools"]; tools != nil {
		raw, _ := json.Marshal(tools)
		extras += string(raw)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": estimateTokens(messagesText(msgs)) + estimateTokens(extras),
	})
}
