package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// uuid 返回标准带横线的 UUIDv4 字符串。
func uuid() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// uuidCompact 返回去掉横线的 UUID。
func uuidCompact() string {
	return strings.ReplaceAll(uuid(), "-", "")
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// randomID 生成形如 acc_1a2b3c4d5e 的稳定可读 ID。
func randomID(prefix string) string {
	return prefix + "_" + strconv.FormatInt(time.Now().UnixMilli(), 16) + randomHex(4)
}

// secret 生成 32 字节 URL 安全随机串，用作 API Key。
func secret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// machineToken 生成 Cosy 机器令牌：两段 UUID 拼接取前 50 字符后 base64url 编码。
func machineToken() string {
	raw := uuid() + uuid()
	if len(raw) > 50 {
		raw = raw[:50]
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

/* ---------------- JSON 取值助手（上游字段命名混乱，需要容错） ---------------- */

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		if t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// firstStr 返回第一个非空字符串值。
func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := str(m, k); v != "" {
			return v
		}
	}
	return ""
}

func obj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func list(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func boolean(m map[string]any, key string, def bool) bool {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return def
}

func number(m map[string]any, key string, def float64) float64 {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
	}
	return def
}

// numberAny 从任意值解析数字，失败返回 (0,false)。
func numberAny(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case int32:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// ptrFloat 返回浮点指针，便于区分「0」与「没有值」。
func ptrFloat(f float64) *float64 { return &f }

func firstNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if m == nil {
			continue
		}
		if v, ok := m[k]; ok && v != nil {
			if f, ok := numberAny(v); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// normalizeExpire 统一时间：小于 1e12 视为秒，否则视为毫秒；支持 ISO 字符串。
// 返回 0 表示没有有效时间。
func normalizeExpire(raw any) int64 {
	if raw == nil {
		return 0
	}
	switch t := raw.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return 0
		}
	case float64:
		if t < 1e12 {
			return int64(t * 1000)
		}
		return int64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0
		}
		if f < 1e12 {
			return int64(f * 1000)
		}
		return int64(f)
	}
	if f, ok := numberAny(raw); ok {
		if f < 1e12 {
			return int64(f * 1000)
		}
		return int64(f)
	}
	if s, ok := raw.(string); ok {
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05"} {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UnixMilli()
			}
		}
	}
	return 0
}

// msToTime 把毫秒时间戳转成 time.Time，0 表示零值。
func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func isoFromMS(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// localDayKey 返回本地日期键，用于「今日已签到」判定。
func localDayKey(t time.Time) string {
	return t.Format("2006-01-02")
}

// sortedKeys 返回 map 的有序键，保证输出稳定。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// truncateRunes 按字符截断，避免切坏 UTF-8。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// randRead 读取加密安全随机字节。
func randRead(b []byte) (int, error) {
	return rand.Read(b)
}
