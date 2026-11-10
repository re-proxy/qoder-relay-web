package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

/*
模型别名解析。

客户端经常发送展示名（DeepSeek-Flash）或带 effort 后缀（qwen3.8-max-high），
上游真正需要的是 model key（dfmodel）。这里统一映射，避免 400。
*/

var modelKeys = map[string]bool{
	"auto": true, "lite": true,
	"qmodel_38max": true, "qfmodel": true, "qmodel_latest": true, "qmodel": true, "q37fmodel": true,
	"dmodel": true, "dfmodel": true, "gmodel": true, "gfmodel": true, "gm51model": true,
	"kmodel_latest": true, "kmodel": true, "jmodel": true, "mmodel": true,
}

var modelAliases = map[string]string{
	"auto": "auto",
	"lite": "lite",

	"qwen3.8-max":    "qmodel_38max",
	"qwen-3.8-max":   "qmodel_38max",
	"qwen38max":      "qmodel_38max",
	"qwen3.8-flash":  "qfmodel",
	"qwen-3.8-flash": "qfmodel",
	"qwen3.7-max":    "qmodel_latest",
	"qwen-3.7-max":   "qmodel_latest",
	"qwen3.7-plus":   "qmodel",
	"qwen-3.7-plus":  "qmodel",
	"qwen3.7-flash":  "q37fmodel",
	"qwen-3.7-flash": "q37fmodel",

	"deepseek-v4-pro": "dmodel",
	"deepseek-4-pro":  "dmodel",
	"deepseek-v4":     "dmodel",
	"deepseek-pro":    "dmodel",
	"deepseek-flash":  "dfmodel",
	"deepseek-chat":   "dfmodel",
	"deepseek-v3":     "dfmodel",

	"glm-5.3":       "gmodel",
	"glm5.3":        "gmodel",
	"glm-5.3-flash": "gfmodel",
	"glm5.3-flash":  "gfmodel",
	"glm-5.2":       "gm51model",
	"glm5.2":        "gm51model",

	"kimi-k3":           "kmodel_latest",
	"kimi-k2.8-preview": "kmodel",
	"kimi-k2.8":         "kmodel",

	"step-5-preview": "jmodel",
	"step-5":         "jmodel",
	"step5":          "jmodel",

	"minimax-m3.1-flash": "mmodel",
	"minimax":            "mmodel",
}

// defaultModelList 是内置模型条目，作为上游模型列表不可用时的兜底。
// 内容与国内版 app/chat 场景的目录保持一致，保证没有可用账号时 /v1/models 依然可用。
var defaultModelList = []ModelInfo{
	{ID: "auto", DisplayName: "Auto（默认）", OwnedBy: "qoder", IsReasoning: true, IsVL: true},
	{ID: "qmodel_38max", DisplayName: "Qwen3.8-Max", OwnedBy: "qoder", IsReasoning: true, IsVL: true},
	{ID: "qfmodel", DisplayName: "Qwen3.8-Flash", OwnedBy: "qoder", IsReasoning: true, IsVL: true},
	{ID: "dmodel", DisplayName: "DeepSeek-V4-Pro", OwnedBy: "qoder", IsReasoning: true},
	{ID: "dfmodel", DisplayName: "DeepSeek-Flash", OwnedBy: "qoder", IsReasoning: true},
	{ID: "gmodel", DisplayName: "GLM-5.3", OwnedBy: "qoder", IsReasoning: true},
	{ID: "gfmodel", DisplayName: "GLM-5.3-Flash", OwnedBy: "qoder", IsReasoning: true},
	{ID: "kmodel_latest", DisplayName: "Kimi-K3.1-Pro", OwnedBy: "qoder", IsReasoning: true},
	{ID: "kmodel", DisplayName: "Kimi-K2.8-Preview", OwnedBy: "qoder", IsReasoning: true},
	{ID: "jmodel", DisplayName: "Step-5-Preview", OwnedBy: "qoder", IsReasoning: true},
	{ID: "mmodel", DisplayName: "MiniMax-M3.1-Flash", OwnedBy: "qoder", IsReasoning: true},
	{ID: "lite", DisplayName: "Lite", OwnedBy: "qoder"},
}

// ModelInfo 是模型目录条目。
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	OwnedBy     string `json:"owned_by,omitempty"`
	IsReasoning bool   `json:"is_reasoning,omitempty"`
	IsVL        bool   `json:"is_vl,omitempty"`
	Source      string `json:"source,omitempty"` // system | upstream | custom
}

var aliasStripPrefix = regexp.MustCompile(`^qoder[/:_\s-]+`)
var aliasStripChars = regexp.MustCompile(`[^a-z0-9.-]`)
var effortSuffix = regexp.MustCompile(`^(.*?)[\s_-]+(xhigh|high|medium|low|max)$`)

// normalizeAlias 把展示名规范化成别名表里的形式。
func normalizeAlias(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = aliasStripPrefix.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "_", " ")), "-")
	s = aliasStripChars.ReplaceAllString(s, "")
	return s
}

func lookupModelKey(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if modelKeys[lower] {
		return lower
	}
	if v, ok := modelAliases[normalizeAlias(raw)]; ok {
		return v
	}
	return ""
}

// resolveModelRequest 返回上游 model key 与 effort。
// 只有去掉 effort 后缀后能识别出基础模型时才剥离，避免把 Qwen3.8-Max 的 Max 当 effort。
func resolveModelRequest(raw string) (key string, effort string) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = "auto"
	}
	if direct := lookupModelKey(value); direct != "" {
		return direct, ""
	}
	if m := effortSuffix.FindStringSubmatch(value); m != nil {
		if base := lookupModelKey(m[1]); base != "" {
			return base, strings.ToLower(m[2])
		}
	}
	return value, ""
}

/* ---------------- 上游模型列表 ---------------- */

const modelListCacheTTL = 10 * time.Minute

type modelListCacheEntry struct {
	at     time.Time
	models []ModelInfo
}

var modelListCache = struct {
	sync.Mutex
	byEdition map[string]modelListCacheEntry
}{byEdition: map[string]modelListCacheEntry{}}

// fetchUpstreamModels 拉取上游模型列表（带 10 分钟缓存）。
// 需要 Cosy 签名：签名路径剥 /algo 前缀、GET 不带请求体；国际版按 api1 → api2 → api3 兜底。
func fetchUpstreamModels(ctx context.Context, ed *Edition, account Account) []ModelInfo {
	modelListCache.Lock()
	if hit, ok := modelListCache.byEdition[ed.Value]; ok && time.Since(hit.at) < modelListCacheTTL {
		models := hit.models
		modelListCache.Unlock()
		return models
	}
	modelListCache.Unlock()

	urls := append([]string{ed.ModelListURL}, ed.ModelListFallbacks...)
	var lastErr error
	for _, url := range urls {
		if url == "" {
			continue
		}
		headers, err := modelListHeaders(ed, &account, url)
		if err != nil {
			lastErr = err
			continue
		}
		res, err := doJSON(ctx, http.MethodGet, url, headers, nil, 20*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		if !res.OK {
			lastErr = &upstreamError{Status: res.Status, Message: res.errText()}
			continue
		}
		payload := res.JSON
		if payload == nil && strings.TrimSpace(res.Text) != "" {
			// 上游可能整体使用自定义 base64 编码
			if decoded, derr := legacyDecode(res.Text); derr == nil {
				var v any
				if json.Unmarshal([]byte(decoded), &v) == nil {
					payload = v
				}
			}
		}
		models := extractModels(payload)
		if len(models) > 0 {
			modelListCache.Lock()
			modelListCache.byEdition[ed.Value] = modelListCacheEntry{at: time.Now(), models: models}
			modelListCache.Unlock()
			return models
		}
		lastErr = errNoModels
	}
	if lastErr != nil && lastErr != errNoModels {
		logf("模型列表拉取失败（%s）：%v", ed.Value, lastErr)
	}
	return nil
}

var errNoModels = &upstreamError{Status: 0, Message: "上游未返回模型列表"}

// extractModels 从上游响应里找出模型数组。
// 新版响应顶层按场景分组：chat / developer / inline / quest …
func extractModels(payload any) []ModelInfo {
	if payload == nil {
		return nil
	}
	if arr, ok := payload.([]any); ok {
		return normalizeModels(arr)
	}
	m := obj(payload)
	if m == nil {
		return nil
	}
	for _, key := range []string{"chat", "models", "data", "body", "list"} {
		switch v := m[key].(type) {
		case []any:
			if models := normalizeModels(v); len(models) > 0 {
				return models
			}
		case string:
			var parsed any
			if json.Unmarshal([]byte(v), &parsed) == nil {
				if arr, ok := parsed.([]any); ok {
					if models := normalizeModels(arr); len(models) > 0 {
						return models
					}
				}
			}
		case map[string]any:
			// 部分响应把数组包在 {list:[...]} 里
			for _, k2 := range []string{"list", "models", "data"} {
				if arr, ok := v[k2].([]any); ok {
					if models := normalizeModels(arr); len(models) > 0 {
						return models
					}
				}
			}
		}
	}
	return nil
}

func normalizeModels(arr []any) []ModelInfo {
	out := make([]ModelInfo, 0, len(arr))
	for _, item := range arr {
		switch t := item.(type) {
		case string:
			if t != "" {
				out = append(out, ModelInfo{ID: t, DisplayName: t, OwnedBy: "qoder", Source: "upstream"})
			}
		case map[string]any:
			if !boolean(t, "enable", true) || !boolean(t, "enabled", true) {
				continue
			}
			id := firstStr(t, "key", "id", "model", "name")
			if id == "" {
				continue
			}
			owner := "qoder"
			if src := str(t, "source"); src != "" && src != "system" {
				owner = src
			}
			out = append(out, ModelInfo{
				ID:          id,
				DisplayName: firstStr(t, "display_name", "displayName", "name", "key", "id"),
				OwnedBy:     owner,
				IsReasoning: boolean(t, "is_reasoning", false),
				IsVL:        boolean(t, "is_vl", false),
				Source:      "upstream",
			})
		}
	}
	return out
}

// upstreamError 表示上游返回的非 2xx 响应。
type upstreamError struct {
	Status  int
	Message string
}

func (e *upstreamError) Error() string {
	if e.Status > 0 {
		return "上游 HTTP " + itoa(e.Status) + ": " + e.Message
	}
	return e.Message
}

// statusCode 返回可透传给客户端的状态码（无效时为 502）。
func (e *upstreamError) statusCode() int {
	if e.Status >= 400 && e.Status < 600 {
		return e.Status
	}
	return 502
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
