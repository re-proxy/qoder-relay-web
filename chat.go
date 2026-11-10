package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

/*
Qoder 双后端聊天：

  - legacy：IDE 登录态（security_oauth_token）走 Cosy 签名的 SSE 流式接口
  - cloud：PAT 走 Cloud Agents API

统一产出归一化事件：content / reasoning / tool_calls / usage / done / error。
*/

// chatEvent 是归一化后的流式事件。
type chatEvent struct {
	Type      string // content | reasoning | tool_calls | usage | done | error
	Text      string
	ToolCalls []ToolCall
	Usage     *TokenUsage
	Error     string
	Status    int
}

// ToolCall 是 OpenAI 风格的函数调用。
type ToolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc 是函数名与参数。
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// TokenUsage 是上游返回的用量。
type TokenUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
}

// ChatMessage 是归一化后的对话消息。
type ChatMessage struct {
	Role       string
	Content    any // string 或 []any（多模态分片）
	ToolCalls  []ToolCall
	ToolCallID string
}

// ChatRequest 是交给后端的一次请求。
type ChatRequest struct {
	Account         Account
	Edition         *Edition
	Model           string
	Messages        []ChatMessage
	Tools           any
	ReasoningEffort string
	ThinkingType    string
	ThinkingBudget  int64
	ImageURLs       []string
	WorkspaceRoot   string
	Timeout         time.Duration
}

// normalizeModelKey 把模型名收敛到上游 key。
func normalizeModelKey(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "", "auto", "default", "claude-sonnet-4.5", "claude_sonnet_4_5", "sonnet", "claude":
		return "auto"
	case "lite":
		return "lite"
	}
	return strings.TrimSpace(model)
}

// normalizeContent 把任意 content 压成文本。
func normalizeContent(content any) string {
	switch t := content.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64, bool, json.Number:
		return fmt.Sprint(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			m := obj(item)
			if m == nil {
				continue
			}
			switch str(m, "type") {
			case "text":
				parts = append(parts, str(m, "text"))
			case "image_url":
				if url := imageURLOf(m); url != "" {
					parts = append(parts, "[image] "+url)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	b, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	return string(b)
}

func imageURLOf(m map[string]any) string {
	if s, ok := m["image_url"].(string); ok {
		return s
	}
	return str(obj(m["image_url"]), "url")
}

func reasoningEnabled(req ChatRequest) bool {
	if strings.TrimSpace(req.ReasoningEffort) != "" {
		return true
	}
	switch req.ThinkingType {
	case "enabled", "adaptive":
		return true
	}
	return false
}

func normalizeReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "max"
	}
	return ""
}

func budgetToEffort(budget int64) string {
	switch {
	case budget <= 0:
		return "medium"
	case budget <= 4000:
		return "low"
	case budget <= 12000:
		return "medium"
	case budget <= 24000:
		return "high"
	default:
		return "max"
	}
}

func maxTokensForEffort(effort string) int64 {
	switch effort {
	case "low":
		return 8192
	case "medium":
		return 16384
	case "high":
		return 24576
	}
	return 32768
}

/* ---------------- legacy 后端 ---------------- */

// defaultTools 是模板自带的工具声明（Read/Bash）。
func defaultTools() []any {
	return []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "Read",
				"description": "Read a local file or image attachment referenced in the conversation.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"file_path": map[string]any{"type": "string", "description": "Path of the file to read."}},
					"required":   []any{"file_path"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "Bash",
				"description": "Execute a shell command when the client allows command execution.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string", "description": "The command to execute."}},
					"required":   []any{"command"},
				},
			},
		},
	}
}

func newLegacyTemplate() map[string]any {
	return map[string]any{
		"request_id":     uuid(),
		"chat_record_id": uuid(),
		"stream":         true,
		"chat_task":      "FREE_INPUT",
		"chat_context": map[string]any{
			"chatPrompt": "",
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"is_reasoning": false, "key": "auto"},
				"originalContent": map[string]any{"type": "text", "text": ""},
			},
			"features":  []any{},
			"imageUrls": nil,
			"text":      map[string]any{"type": "text", "text": ""},
		},
		"image_urls":       nil,
		"is_reply":         true,
		"is_retry":         false,
		"session_id":       uuid(),
		"code_language":    "",
		"source":           1,
		"version":          "3",
		"chat_prompt":      "",
		"parameters":       map[string]any{"max_tokens": int64(32768)},
		"aliyun_user_type": "personal_standard",
		"session_type":     "qodercli",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"model_config": map[string]any{
			"key":              "auto",
			"display_name":     "Auto",
			"model":            "",
			"format":           "openai",
			"is_vl":            false,
			"is_reasoning":     false,
			"api_key":          "",
			"url":              "",
			"source":           "system",
			"max_input_tokens": 180000,
		},
		"business": map[string]any{
			"id":          uuid(),
			"begin_at":    time.Now().UnixMilli(),
			"scene":       "chat",
			"type":        "chat",
			"sub_type":    "free_input",
			"name":        "",
			"language":    "",
			"parent_type": "",
		},
		"messages": []any{},
		"tools":    defaultTools(),
	}
}

// latestPrompt 返回最后一条 user 消息的文本，用于业务名与上下文回显。
func latestPrompt(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Role) != "user" {
			continue
		}
		if text := strings.TrimSpace(normalizeContent(messages[i].Content)); text != "" {
			return normalizeContent(messages[i].Content)
		}
	}
	return ""
}

func legacyUserContents(content any) []any {
	if s, ok := content.(string); ok {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": s}}
	}
	if arr, ok := content.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, part := range arr {
			m := obj(part)
			if m == nil {
				continue
			}
			switch str(m, "type") {
			case "text":
				if strings.TrimSpace(str(m, "text")) != "" {
					out = append(out, map[string]any{"type": "text", "text": str(m, "text")})
				}
			case "image_url":
				if url := imageURLOf(m); url != "" {
					out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	text := normalizeContent(content)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []any{map[string]any{"type": "text", "text": text}}
}

func legacyMessages(messages []ChatMessage) []any {
	out := make([]any, 0, len(messages))
	for _, message := range messages {
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		switch role {
		case "user":
			contents := legacyUserContents(message.Content)
			if len(contents) == 0 {
				continue
			}
			out = append(out, map[string]any{
				"role":     "user",
				"content":  "",
				"contents": contents,
				"response_meta": map[string]any{
					"id":    "",
					"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
				},
				"reasoning_content_signature": "",
			})
		case "assistant":
			entry := map[string]any{"role": "assistant", "content": normalizeContent(message.Content)}
			if len(message.ToolCalls) > 0 {
				entry["tool_calls"] = message.ToolCalls
			}
			if strings.TrimSpace(normalizeContent(message.Content)) == "" && len(message.ToolCalls) == 0 {
				continue
			}
			out = append(out, entry)
		case "tool":
			text := normalizeContent(message.Content)
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": message.ToolCallID, "content": text})
		default:
			text := normalizeContent(message.Content)
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, map[string]any{"role": role, "content": text})
		}
	}
	return out
}

// buildLegacyRequest 组装上游请求体。
func buildLegacyRequest(req ChatRequest) map[string]any {
	body := newLegacyTemplate()
	requestID := uuid()
	body["request_id"] = requestID
	body["chat_record_id"] = requestID
	body["session_id"] = uuid()
	body["stream"] = true
	if req.Account.UserType != "" {
		body["aliyun_user_type"] = req.Account.UserType
	}

	modelKey := normalizeModelKey(req.Model)
	isReasoning := reasoningEnabled(req)
	isVL := len(req.ImageURLs) > 0

	modelConfig := obj(body["model_config"])
	modelConfig["key"] = modelKey
	modelConfig["is_reasoning"] = isReasoning
	modelConfig["is_vl"] = isVL

	chatContext := obj(body["chat_context"])
	extra := obj(chatContext["extra"])
	extraConfig := obj(extra["modelConfig"])
	extraConfig["key"] = modelKey
	extraConfig["is_reasoning"] = isReasoning
	extraConfig["is_vl"] = isVL

	business := obj(body["business"])
	business["id"] = uuid()
	business["begin_at"] = time.Now().UnixMilli()
	prompt := latestPrompt(req.Messages)
	if len([]rune(prompt)) > 30 {
		prompt = string([]rune(prompt)[:30])
	}
	business["name"] = prompt

	obj(chatContext["text"])["text"] = prompt
	obj(extra["originalContent"])["text"] = latestPrompt(req.Messages)
	if isVL {
		chatContext["imageUrls"] = req.ImageURLs
		body["image_urls"] = req.ImageURLs
	}

	effort := normalizeReasoningEffort(req.ReasoningEffort)
	if effort == "" && isReasoning {
		effort = budgetToEffort(req.ThinkingBudget)
	}
	if effort != "" {
		params := obj(body["parameters"])
		params["reasoning_effort"] = effort
		if cur, ok := numberAny(params["max_tokens"]); !ok || int64(cur) < maxTokensForEffort(effort) {
			params["max_tokens"] = maxTokensForEffort(effort)
		}
	}

	body["messages"] = legacyMessages(req.Messages)
	switch {
	case req.Tools != nil:
		if arr, ok := req.Tools.([]any); ok {
			body["tools"] = arr
		} else {
			body["tools"] = []any{req.Tools}
		}
	case isVL:
		// 带图片时保留默认工具（Read/Bash）
	default:
		body["tools"] = []any{}
	}
	return body
}

// parseLegacyLine 解析 legacy SSE 的一行 data（{"body": <openai chunk>}）。
func parseLegacyLine(dataLine string) []chatEvent {
	if dataLine == "" || dataLine == "[DONE]" {
		return nil
	}
	var envelope map[string]any
	dec := json.NewDecoder(strings.NewReader(dataLine))
	dec.UseNumber()
	if err := dec.Decode(&envelope); err != nil {
		return nil
	}

	var chunk map[string]any
	if s, ok := envelope["body"].(string); ok {
		var v map[string]any
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		if err := d.Decode(&v); err == nil {
			chunk = v
		}
	} else if m, ok := envelope["body"].(map[string]any); ok {
		chunk = m
	}

	statusCode := 0
	if f, ok := firstNumber(envelope, "statusCodeValue", "statusCode", "status"); ok {
		statusCode = int(f)
	}
	codeValue := 0
	if f, ok := firstNumber(envelope, "code", "errorCode"); ok {
		codeValue = int(f)
	} else if chunk != nil {
		if f, ok := firstNumber(chunk, "code", "errorCode"); ok {
			codeValue = int(f)
		}
	}
	chunkError := ""
	if chunk != nil {
		if e := obj(chunk["error"]); e != nil {
			chunkError = str(e, "message")
		} else if s, ok := chunk["error"].(string); ok {
			chunkError = s
		}
	}
	if chunkError == "" {
		if e := obj(envelope["error"]); e != nil {
			chunkError = str(e, "message")
		} else if s, ok := envelope["error"].(string); ok {
			chunkError = s
		}
	}
	planGate := codeValue == 112
	failed := (statusCode >= 400) || chunkError != "" || planGate
	if failed {
		detail := chunkError
		if planGate {
			detail = "模型计划不可用或额度受限（code 112）"
		}
		if detail == "" {
			detail = firstStr(chunk, "message")
		}
		if detail == "" {
			detail = firstStr(envelope, "message")
		}
		if detail == "" {
			detail = str(obj(envelope["headers"]), "error")
		}
		msg := detail
		if !planGate {
			code := "?"
			if statusCode >= 400 {
				code = itoa(statusCode)
			}
			msg = "上游 HTTP " + code
			if detail != "" {
				msg += ": " + truncateRunes(detail, 300)
			}
		}
		return []chatEvent{{Type: "error", Error: msg, Status: statusCode}}
	}
	if chunk == nil {
		return nil
	}

	events := make([]chatEvent, 0, 4)
	for _, item := range list(chunk["choices"]) {
		choice := obj(item)
		delta := obj(choice["delta"])
		if delta == nil {
			continue
		}
		if text := str(delta, "content"); text != "" {
			events = append(events, chatEvent{Type: "content", Text: text})
		}
		if text := str(delta, "reasoning_content"); text != "" {
			events = append(events, chatEvent{Type: "reasoning", Text: text})
		}
		if calls := parseToolCalls(delta["tool_calls"]); len(calls) > 0 {
			events = append(events, chatEvent{Type: "tool_calls", ToolCalls: calls})
		}
	}
	if usage := obj(chunk["usage"]); usage != nil {
		prompt, _ := firstNumber(usage, "prompt_tokens", "promptTokens")
		completion, _ := firstNumber(usage, "completion_tokens", "completionTokens")
		total, _ := firstNumber(usage, "total_tokens", "totalTokens")
		reasoning, _ := firstNumber(obj(usage["completion_tokens_details"]), "reasoning_tokens")
		cached, _ := firstNumber(obj(usage["prompt_tokens_details"]), "cached_tokens")
		events = append(events, chatEvent{Type: "usage", Usage: &TokenUsage{
			PromptTokens:     int64(prompt),
			CompletionTokens: int64(completion),
			TotalTokens:      int64(total),
			ReasoningTokens:  int64(reasoning),
			CachedTokens:     int64(cached),
		}})
	}
	return events
}

// parseToolCalls 解析 OpenAI 风格 tool_calls 数组。
func parseToolCalls(raw any) []ToolCall {
	arr := list(raw)
	if arr == nil {
		return nil
	}
	out := make([]ToolCall, 0, len(arr))
	for _, item := range arr {
		m := obj(item)
		if m == nil {
			continue
		}
		call := ToolCall{
			ID:   str(m, "id"),
			Type: str(m, "type"),
			Function: ToolCallFunc{
				Name:      str(obj(m["function"]), "name"),
				Arguments: str(obj(m["function"]), "arguments"),
			},
		}
		if f, ok := firstNumber(m, "index"); ok {
			call.Index = int(f)
		} else {
			call.Index = len(out)
		}
		if call.Type == "" {
			call.Type = "function"
		}
		out = append(out, call)
	}
	return out
}

// legacyStream 走 IDE 登录态 + Cosy 签名流。
func legacyStream(ctx context.Context, req ChatRequest) (<-chan chatEvent, error) {
	body := buildLegacyRequest(req)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	encoded := legacyEncode(string(raw))
	headers, err := legacyStreamHeaders(req.Edition, &req.Account, req.Edition.StreamPath, encoded)
	if err != nil {
		return nil, err
	}
	headers["content-type"] = "application/json"
	headers["user-agent"] = "qoder-relay-web/1.0"
	headers["x-model-key"] = normalizeModelKey(req.Model)
	headers["x-model-source"] = "system"

	idle := req.Timeout
	if idle <= 0 {
		idle = 300 * time.Second
	}
	sse, err := openSSE(ctx, http.MethodPost, req.Edition.StreamURL, headers, encoded, idle)
	if err != nil {
		return nil, err
	}

	out := make(chan chatEvent, 32)
	go func() {
		defer close(out)
		defer sse.Close()
		sawOutput := false
		for {
			line, ok, err := sse.NextLine()
			if err != nil {
				out <- chatEvent{Type: "error", Error: err.Error()}
				return
			}
			if !ok {
				break
			}
			data, isData := sseData(line)
			if !isData {
				continue
			}
			if data == "[DONE]" {
				break
			}
			events := parseLegacyLine(data)
			for _, ev := range events {
				switch ev.Type {
				case "content", "reasoning", "tool_calls":
					sawOutput = true
				case "error":
					out <- ev
					return
				}
				out <- ev
			}
		}
		// 旧版 SSE 正常结束时可能没有 [DONE]，EOF 即完成；但全空流不能伪装成功
		if !sawOutput {
			out <- chatEvent{Type: "error", Error: "上游未返回任何内容（可能被风控、模型不可用或计划受限）"}
			return
		}
		out <- chatEvent{Type: "done"}
	}()
	return out, nil
}

/* ---------------- cloud 后端（PAT） ---------------- */

type cloudSession struct {
	baseURL       string
	pat           string
	defaultModel  string
	workspaceRoot string
	environmentID string
	agentID       string
}

func newCloudSession(ed *Edition, pat, model, workspaceRoot string) *cloudSession {
	return &cloudSession{
		baseURL:       strings.TrimRight(ed.CloudBaseURL, "/") + "/api/v1/cloud",
		pat:           pat,
		defaultModel:  model,
		workspaceRoot: workspaceRoot,
	}
}

func (c *cloudSession) get(ctx context.Context, path string) (any, error) {
	res, err := doJSON(ctx, http.MethodGet, c.baseURL+path, map[string]string{"Authorization": "Bearer " + c.pat}, nil, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("GET %s HTTP %d: %s", path, res.Status, res.errText())
	}
	return res.JSON, nil
}

func (c *cloudSession) post(ctx context.Context, path string, body any) (any, error) {
	res, err := doJSON(ctx, http.MethodPost, c.baseURL+path, map[string]string{
		"Authorization": "Bearer " + c.pat,
		"Content-Type":  "application/json",
	}, body, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("POST %s HTTP %d: %s", path, res.Status, res.errText())
	}
	return res.JSON, nil
}

func (c *cloudSession) ensureEnvironment(ctx context.Context) error {
	if c.environmentID != "" {
		return nil
	}
	raw, err := c.get(ctx, "/environments")
	if err == nil {
		payload := obj(raw)
		for _, item := range list(payload["data"]) {
			env := obj(item)
			status := str(env, "status")
			if status == "" || status == "ready" {
				c.environmentID = str(env, "id")
				return nil
			}
		}
	}
	created, cerr := c.post(ctx, "/environments", map[string]any{
		"name":   "qoder-relay-web-default",
		"config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "unrestricted"}},
	})
	if cerr != nil {
		return cerr
	}
	c.environmentID = str(obj(created), "id")
	if c.environmentID == "" {
		return fmt.Errorf("创建 Cloud 环境失败：响应缺少 id")
	}
	return nil
}

func (c *cloudSession) ensureAgent(ctx context.Context, model string) error {
	if c.agentID != "" {
		return nil
	}
	instructions := strings.Join([]string{
		"You are an efficient programming assistant.",
		"Reply concisely and correctly.",
		"When the user supplies chat history, treat the provided transcript as authoritative context.",
		"If the task is coding-related, prefer concrete patches, commands, and implementation details.",
	}, "\n")
	if model == "" {
		model = c.defaultModel
	}
	created, err := c.post(ctx, "/agents", map[string]any{
		"name":   "qoder-relay-web-bridge",
		"model":  model,
		"system": instructions,
		"tools":  []any{map[string]any{"type": "bash_20250124"}, map[string]any{"type": "text_editor_20250124"}},
	})
	if err != nil {
		return err
	}
	c.agentID = str(obj(created), "id")
	if c.agentID == "" {
		return fmt.Errorf("创建 Cloud Agent 失败：响应缺少 id")
	}
	return nil
}

func (c *cloudSession) createSession(ctx context.Context) (string, error) {
	body := map[string]any{"agent": c.agentID, "environment_id": c.environmentID}
	if c.workspaceRoot != "" {
		body["metadata"] = map[string]any{"workspace_root": c.workspaceRoot}
	}
	created, err := c.post(ctx, "/sessions", body)
	if err != nil {
		return "", err
	}
	id := str(obj(created), "id")
	if id == "" {
		return "", fmt.Errorf("创建 Cloud Session 失败：响应缺少 id")
	}
	return id, nil
}

func (c *cloudSession) sendMessage(ctx context.Context, sessionID string, req ChatRequest) error {
	_, err := c.post(ctx, "/sessions/"+sessionID+"/events", map[string]any{
		"events": []any{map[string]any{"type": "user.message", "content": renderPrompt(req)}},
	})
	return err
}

// renderPrompt 把对话压成单条 Cloud 消息。
func renderPrompt(req ChatRequest) string {
	lines := []string{
		"You are handling an OpenAI-style chat completion request.",
		"Conversation transcript:",
	}
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		lines = append(lines, "", "["+role+"]", normalizeContent(msg.Content), "")
	}
	if req.Tools != nil {
		if b, err := json.Marshal(req.Tools); err == nil {
			lines = append(lines, "Available tool schema from the caller:")
			lines = append(lines, string(b))
			lines = append(lines, "If tool use is necessary, emit structured tool-use actions when supported by the runtime.")
		}
	}
	lines = append(lines, "Respond to the latest user request using the transcript above.")
	return strings.Join(lines, "\n")
}

func normalizeCloudContent(content any) string {
	switch t := content.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			m := obj(item)
			if m == nil {
				continue
			}
			if str(m, "type") == "text" {
				parts = append(parts, str(m, "text"))
			}
		}
		return strings.Join(parts, "\n")
	}
	b, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	return string(b)
}

// cloudStream 走 Cloud Agents API。
func cloudStream(ctx context.Context, req ChatRequest) (<-chan chatEvent, error) {
	session := newCloudSession(req.Edition, req.Account.PAT, req.Model, req.WorkspaceRoot)
	if err := session.ensureEnvironment(ctx); err != nil {
		return nil, err
	}
	if err := session.ensureAgent(ctx, req.Model); err != nil {
		return nil, err
	}
	sessionID, err := session.createSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := session.sendMessage(ctx, sessionID, req); err != nil {
		return nil, err
	}

	idle := req.Timeout
	if idle <= 0 {
		idle = 300 * time.Second
	}
	sse, err := openSSE(ctx, http.MethodGet, session.baseURL+"/sessions/"+sessionID+"/events/stream",
		map[string]string{"Authorization": "Bearer " + session.pat}, nil, idle)
	if err != nil {
		return nil, err
	}

	out := make(chan chatEvent, 32)
	go func() {
		defer close(out)
		defer sse.Close()
		var eventType, dataLine string
		sawOutput := false
		for {
			line, ok, err := sse.NextLine()
			if err != nil {
				out <- chatEvent{Type: "error", Error: err.Error()}
				return
			}
			if !ok {
				break
			}
			if name, isEvent := sseEvent(line); isEvent {
				eventType = name
				continue
			}
			if data, isData := sseData(line); isData {
				dataLine = data
				continue
			}
			if strings.TrimSpace(line) != "" || (eventType == "" && dataLine == "") {
				continue
			}
			var payload map[string]any
			if dataLine != "" && dataLine != "{}" {
				d := json.NewDecoder(strings.NewReader(dataLine))
				d.UseNumber()
				if err := d.Decode(&payload); err != nil {
					payload = nil
				}
			}
			switch eventType {
			case "session.status_idle", "terminated":
				out <- chatEvent{Type: "done"}
				return
			case "session.error":
				out <- chatEvent{Type: "error", Error: "session error: " + dataLine}
				return
			case "agent.message":
				text := normalizeCloudContent(payload["content"])
				if text != "" {
					sawOutput = true
					out <- chatEvent{Type: "content", Text: text}
				}
			case "agent.tool_use":
				sawOutput = true
				args := "{}"
				if b, err := json.Marshal(payload["input"]); err == nil {
					args = string(b)
				}
				out <- chatEvent{Type: "tool_calls", ToolCalls: []ToolCall{{
					Index: 0,
					ID:    "call_" + randomHex(6),
					Type:  "function",
					Function: ToolCallFunc{
						Name:      str(payload, "name"),
						Arguments: args,
					},
				}}}
			}
			eventType = ""
			dataLine = ""
		}
		if !sawOutput {
			out <- chatEvent{Type: "error", Error: "上游未返回任何内容（可能被风控或会话异常）"}
			return
		}
		out <- chatEvent{Type: "done"}
	}()
	return out, nil
}

// streamChat 按账号类型选择后端。
func streamChat(ctx context.Context, req ChatRequest) (<-chan chatEvent, error) {
	if req.Account.PAT != "" {
		return cloudStream(ctx, req)
	}
	if req.Account.SecurityToken != "" {
		return legacyStream(ctx, req)
	}
	return nil, fmt.Errorf("账号既没有 PAT 也没有登录 token，请重新登录或导入")
}

// mergeToolCalls 按 index 累积流式 tool_calls 分片。
func mergeToolCalls(acc []ToolCall, incoming []ToolCall) []ToolCall {
	for _, call := range incoming {
		index := call.Index
		for len(acc) <= index {
			acc = append(acc, ToolCall{Index: len(acc), Type: "function"})
		}
		target := &acc[index]
		if call.ID != "" {
			target.ID = call.ID
		}
		if call.Type != "" {
			target.Type = call.Type
		}
		if call.Function.Name != "" {
			target.Function.Name = call.Function.Name
		}
		target.Function.Arguments += call.Function.Arguments
	}
	return acc
}
