package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

/*
Qoder 账号信息查询：订阅计划、额度用量、签到活动、模型列表。
*/

// permanentExpiryMS 是「永不过期」阈值：上游用 9999-12-31 或 0 表示永久。
var permanentExpiryMS = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

// normalizeTime 把上游时间（ISO 字符串 / 秒 / 毫秒）统一成毫秒；0 表示无有效时间。
func normalizeTime(raw any) int64 {
	if raw == nil {
		return 0
	}
	var ms int64
	if f, ok := numberAny(raw); ok {
		ms = int64(f)
		if ms < 1e12 {
			ms *= 1000
		}
	} else if s, ok := raw.(string); ok {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return 0
		}
		ms = t.UnixMilli()
	} else {
		return 0
	}
	if ms <= 0 || ms >= permanentExpiryMS {
		return 0
	}
	return ms
}

// authHeaders 是计费/活动接口的统一身份头。
func authHeaders(ed *Edition, account *Account) map[string]string {
	ident := getMachineIdentity(ed.Value)
	return billingHeaders(ed, account, ident)
}

// quotaPart 归一化一段额度。
func quotaPart(raw map[string]any) *Quota {
	if raw == nil {
		return nil
	}
	q := &Quota{Unit: str(raw, "unit")}
	if f, ok := firstNumber(raw, "total"); ok {
		q.Total = ptrFloat(f)
	}
	if f, ok := firstNumber(raw, "used"); ok {
		q.Used = ptrFloat(f)
	}
	if f, ok := firstNumber(raw, "remaining"); ok {
		q.Remaining = ptrFloat(f)
	}
	if f, ok := firstNumber(raw, "percentage"); ok {
		q.Percentage = ptrFloat(f)
	}
	if q.Total == nil && q.Used == nil && q.Remaining == nil {
		return nil
	}
	return q
}

// normalizePackages 归一化专属资源包（活动奖励等，带独立过期时间）。
func normalizePackages(raw any) []Quota {
	arr := list(raw)
	out := make([]Quota, 0, len(arr))
	for _, item := range arr {
		p := obj(item)
		if p == nil {
			continue
		}
		q := Quota{
			Unit:            str(p, "unit"),
			IsQuotaExceeded: !boolean(p, "available", true),
			ExpiresAt:       normalizeTime(firstAny(p, "expiresAt", "expires_at")),
		}
		if f, ok := firstNumber(p, "total"); ok {
			q.Total = ptrFloat(f)
		}
		if f, ok := firstNumber(p, "used"); ok {
			q.Used = ptrFloat(f)
		}
		if f, ok := firstNumber(p, "remaining"); ok {
			q.Remaining = ptrFloat(f)
		}
		if (q.Total != nil && *q.Total > 0) || (q.Remaining != nil && *q.Remaining > 0) {
			out = append(out, q)
		}
	}
	return out
}

// combineQuota 合并套餐额度、加油包、专属资源包。
// 上游 percentage 语义是「已用比例」，合并结果保持同一语义。
func combineQuota(plan, addOn *Quota, packages []Quota) *Quota {
	if addOn != nil && (addOn.Total == nil || *addOn.Total <= 0) && (addOn.Remaining == nil || *addOn.Remaining <= 0) {
		addOn = nil
	}
	usable := make([]Quota, 0, len(packages))
	for _, p := range packages {
		if !p.IsQuotaExceeded {
			usable = append(usable, p)
		}
	}
	if plan == nil && addOn == nil && len(usable) == 0 {
		return nil
	}
	pools := make([]*Quota, 0, len(usable)+2)
	if plan != nil {
		pools = append(pools, plan)
	}
	if addOn != nil {
		pools = append(pools, addOn)
	}
	for i := range usable {
		pools = append(pools, &usable[i])
	}
	sum := func(get func(*Quota) *float64) *float64 {
		var total float64
		seen := false
		for _, pool := range pools {
			if v := get(pool); v != nil {
				total += *v
				seen = true
			}
		}
		if !seen {
			return nil
		}
		return ptrFloat(total)
	}
	out := &Quota{
		Total:     sum(func(q *Quota) *float64 { return q.Total }),
		Used:      sum(func(q *Quota) *float64 { return q.Used }),
		Remaining: sum(func(q *Quota) *float64 { return q.Remaining }),
	}
	for _, pool := range pools {
		if pool.Unit != "" {
			out.Unit = pool.Unit
			break
		}
	}
	if out.Total != nil && *out.Total > 0 && out.Used != nil {
		out.Percentage = ptrFloat(*out.Used / *out.Total)
	}
	return out
}

// QuotaUsage 是额度查询结果。
type QuotaUsage struct {
	UsageType            string   `json:"usage_type"`
	TotalUsagePercentage *float64 `json:"total_usage_percentage,omitempty"`
	IsQuotaExceeded      bool     `json:"is_quota_exceeded"`
	ExpiresAt            int64    `json:"expires_at,omitempty"`
	Quota                *Quota   `json:"quota,omitempty"`
}

// GetQuotaUsage 查询账号额度用量。
func GetQuotaUsage(ctx context.Context, ed *Edition, account Account) (QuotaUsage, error) {
	res, err := doJSON(ctx, http.MethodGet, ed.OpenAPIBaseURL+ed.QuotaUsagePath, authHeaders(ed, &account), nil, 20*time.Second)
	if err != nil {
		return QuotaUsage{}, err
	}
	if !res.OK {
		return QuotaUsage{}, fmt.Errorf("额度查询 HTTP %d: %s", res.Status, res.errText())
	}
	data := obj(res.JSON)
	out := QuotaUsage{
		UsageType:       str(data, "usageType"),
		IsQuotaExceeded: boolean(data, "isQuotaExceeded", false),
		ExpiresAt:       normalizeTime(data["expiresAt"]),
	}
	if f, ok := firstNumber(data, "totalUsagePercentage"); ok {
		out.TotalUsagePercentage = ptrFloat(f)
	}
	out.Quota = combineQuota(
		quotaPart(obj(data["userQuota"])),
		quotaPart(obj(data["addOnQuota"])),
		normalizePackages(data["dedicatedResourcePackages"]),
	)
	if out.Quota != nil {
		out.Quota.IsQuotaExceeded = out.IsQuotaExceeded
		out.Quota.ExpiresAt = out.ExpiresAt
	}
	return out, nil
}

// UserPlan 是订阅计划查询结果。
type UserPlan struct {
	UserType     string `json:"user_type"`
	PlanTierName string `json:"plan_tier_name"`
	IsPersonal   bool   `json:"is_personal_version"`
	StartDate    int64  `json:"start_date,omitempty"`
	EndDate      int64  `json:"end_date,omitempty"`
}

// GetUserPlan 查询订阅计划。
func GetUserPlan(ctx context.Context, ed *Edition, account Account) (UserPlan, error) {
	res, err := doJSON(ctx, http.MethodGet, ed.OpenAPIBaseURL+ed.UserPlanPath, authHeaders(ed, &account), nil, 20*time.Second)
	if err != nil {
		return UserPlan{}, err
	}
	if !res.OK {
		return UserPlan{}, fmt.Errorf("订阅查询 HTTP %d: %s", res.Status, res.errText())
	}
	data := obj(res.JSON)
	return UserPlan{
		UserType:     firstStr(data, "user_type", "userType"),
		PlanTierName: firstStr(data, "plan_tier_name", "planTierName"),
		IsPersonal:   boolean(data, "is_personal_version", false),
		StartDate:    normalizeTime(firstAny(data, "start_date", "startDate")),
		EndDate:      normalizeTime(firstAny(data, "end_date", "endDate")),
	}, nil
}

/* ---------------- 签到活动 ---------------- */

// Campaign 是可领取的签到活动。
type Campaign struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	RewardCredit float64 `json:"reward_credit,omitempty"`
}

func campaignField(c map[string]any, snake, camel string) string {
	return firstStr(c, snake, camel)
}

func campaignAction(c map[string]any) string {
	return strings.ToUpper(strings.TrimSpace(campaignField(c, "action_type", "actionType")))
}

func campaignStatus(c map[string]any) string {
	return strings.ToUpper(strings.TrimSpace(campaignField(c, "claim_status", "claimStatus")))
}

func campaignAmount(c map[string]any) float64 {
	if f, ok := firstNumber(obj(c["benefit"]), "amount"); ok {
		return f
	}
	if f, ok := firstNumber(c, "rewardCredit", "reward_credit", "credit"); ok {
		return f
	}
	return 0
}

func campaignKind(c map[string]any) string {
	return strings.ToUpper(strings.TrimSpace(str(obj(c["benefit"]), "kind")))
}

// isCreditCampaign 判断是否为积分类活动（新版带 benefit.kind=CREDITS，老活动 kind 为空）。
func isCreditCampaign(c map[string]any) bool {
	kind := campaignKind(c)
	return kind == "" || kind == "CREDITS"
}

// campaignActive 判断活动时间窗（上游是秒级时间戳）。
func campaignActive(c map[string]any, now time.Time) bool {
	nowSec := float64(now.Unix())
	if startAt, ok := firstNumber(c, "startAt", "start_at"); ok && startAt > 0 && nowSec < startAt {
		return false
	}
	if endAt, ok := firstNumber(c, "endAt", "end_at"); ok && endAt > 0 && nowSec > endAt {
		return false
	}
	return true
}

// ListCampaigns 拉取活动列表。
// 官方客户端不带 forceRefresh：带上它会让未打开过活动页的账号只返回 VIEW_DETAILS 行，
// 每日 CLAIM_BENEFIT 行被服务端过滤掉。
func ListCampaigns(ctx context.Context, ed *Edition, account Account) ([]map[string]any, error) {
	res, err := doJSON(ctx, http.MethodGet, ed.OpenAPIBaseURL+ed.CampaignsPath, authHeaders(ed, &account), nil, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("活动查询 HTTP %d: %s", res.Status, res.errText())
	}
	var raw []any
	switch v := res.JSON.(type) {
	case []any:
		raw = v
	case map[string]any:
		if arr := list(v["campaigns"]); arr != nil {
			raw = arr
		} else if inner := obj(v["data"]); inner != nil {
			if arr := list(inner["campaigns"]); arr != nil {
				raw = arr
			} else if arr := list(inner); arr != nil {
				raw = arr
			}
		} else if arr := list(v["data"]); arr != nil {
			raw = arr
		}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if c := obj(item); c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

// FindClaimableCheckins 返回可领取的签到活动。
func FindClaimableCheckins(campaigns []map[string]any, now time.Time) []Campaign {
	out := make([]Campaign, 0, len(campaigns))
	for _, c := range campaigns {
		if campaignStatus(c) != "CLAIMABLE" {
			continue
		}
		action := campaignAction(c)
		if action != "" && action != "CLAIM_BENEFIT" {
			continue
		}
		if !isCreditCampaign(c) || !campaignActive(c, now) {
			continue
		}
		id := firstStr(c, "id", "campaignId", "campaign_id")
		if id == "" {
			continue
		}
		out = append(out, Campaign{
			ID:           id,
			Name:         firstStr(c, "name", "title"),
			RewardCredit: campaignAmount(c),
		})
	}
	return out
}

// TodayCheckedIn 判断今日是否已签到。
// 只认签到类（CLAIM_BENEFIT）活动；部分老活动领取后降级为 VIEW_DETAILS 但保留积分金额，也算已签。
func TodayCheckedIn(campaigns []map[string]any) bool {
	for _, c := range campaigns {
		status := campaignStatus(c)
		if status != "CLAIMED" && status != "REPLAYED" {
			continue
		}
		action := campaignAction(c)
		if action == "CLAIM_BENEFIT" {
			return true
		}
		if (action == "" || action == "VIEW_DETAILS") && campaignKind(c) == "CREDITS" && campaignAmount(c) > 0 {
			return true
		}
	}
	return false
}

var claimFailureMessages = map[string]string{
	"SAME_PERSON_ALREADY_CLAIMED":  "同一身份下的其他账号本轮已领取",
	"ALREADY_CLAIMED":              "今天已经领取过",
	"REPLAYED":                     "今天已经领取过",
	"REDEMPTION_CODE_OUT_OF_STOCK": "今日奖励已被领完",
	"ACHIEVEMENT_NOT_COMPLETED":    "活动条件未完成",
	"CAMPAIGN_NOT_ACTIVE":          "活动未开始或已结束",
	"RISK_BLOCKED":                 "风控拦截，请稍后再试",
	"RISK_DEPENDENCY_UNAVAILABLE":  "风控服务暂不可用，请稍后再试",
}

// ClaimResult 是领取结果。
type ClaimResult struct {
	Success      bool    `json:"success"`
	Replayed     bool    `json:"replayed"`
	RewardCredit float64 `json:"reward_credit,omitempty"`
}

// ClaimCampaign 领取奖励（幂等）。
func ClaimCampaign(ctx context.Context, ed *Edition, account Account, campaignID string) (ClaimResult, error) {
	endpoint := ed.OpenAPIBaseURL + ed.ClaimPath + "/" + url.PathEscape(campaignID) + "/claim"
	headers := authHeaders(ed, &account)
	headers["Content-Type"] = "application/json"
	res, err := doJSON(ctx, http.MethodPost, endpoint, headers, "{}", 20*time.Second)
	if err != nil {
		return ClaimResult{}, err
	}
	if !res.OK {
		code := strings.ToUpper(firstStr(obj(res.JSON), "errorCode", "failureCode"))
		if res.Status == http.StatusConflict && (strings.Contains(code, "ALREADY") || code == "REPLAYED") {
			return ClaimResult{Success: true, Replayed: true}, nil
		}
		return ClaimResult{}, fmt.Errorf("领取 HTTP %d: %s", res.Status, res.errText())
	}
	payload := obj(res.JSON)
	body := obj(payload["data"])
	if body == nil {
		body = payload
	}
	failureCode := strings.ToUpper(firstStr(body, "failureCode", "failure_code"))
	if failureCode == "" {
		failureCode = strings.ToUpper(str(payload, "failureCode"))
	}
	if msg, ok := claimFailureMessages[failureCode]; ok {
		if failureCode == "SAME_PERSON_ALREADY_CLAIMED" || failureCode == "ALREADY_CLAIMED" || failureCode == "REPLAYED" {
			return ClaimResult{Success: true, Replayed: true}, nil
		}
		return ClaimResult{}, fmt.Errorf("%s", msg)
	}
	status := strings.ToUpper(str(body, "status"))
	if status == "BLOCKED" {
		return ClaimResult{}, fmt.Errorf("活动暂不可领取，请稍后再试")
	}
	if boolean(body, "replayed", false) || boolean(payload, "replayed", false) {
		return ClaimResult{Success: true, Replayed: true}, nil
	}
	if status != "" && status != "CLAIMED" && status != "GRANTED" && status != "SUCCESS" {
		return ClaimResult{}, fmt.Errorf("上游未确认领取成功（status=%s）", status)
	}
	reward := campaignAmount(body)
	if reward == 0 {
		if f, ok := firstNumber(body, "rewardCredit", "credit"); ok {
			reward = f
		}
	}
	return ClaimResult{Success: true, RewardCredit: reward}, nil
}
