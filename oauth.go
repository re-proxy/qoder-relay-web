package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

/*
Qoder OAuth 设备流登录。

流程：
 1. startLogin 生成 PKCE（nonce / verifier / challenge）与 loginID，构造验证 URL
 2. 用户在浏览器完成登录（URL 带 nonce / challenge / client_id / machine_id）
 3. PollLogin 轮询 /api/v1/deviceToken/poll，拿到 token 后拉取
    userinfo / user/status / user/plan，组装账号
 4. RefreshToken 用 refresh_token 或 PAT 续期
*/

const (
	pollInterval     = 1500 * time.Millisecond
	pollTimeout      = 10 * time.Minute
	maxPendingLogins = 20
)

// pendingLogin 是一次进行中的设备流登录。
type pendingLogin struct {
	LoginID   string
	Nonce     string
	Verifier  string
	Edition   string
	ExpiresAt time.Time
}

var pendingLogins = struct {
	sync.Mutex
	order []string
	byID  map[string]*pendingLogin
}{byID: map[string]*pendingLogin{}}

// loginStart 是发起登录的响应。
type loginStart struct {
	LoginID         string `json:"login_id"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int64  `json:"expires_in"`
	Edition         string `json:"edition"`
}

func pkceVerifier() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return secret()
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// StartLogin 发起一次设备流登录。
func StartLogin(ed *Edition) (loginStart, error) {
	nonce := uuidCompact()
	verifier := pkceVerifier()
	challenge := pkceChallenge(verifier)
	loginID := uuidCompact()

	u, err := url.Parse(ed.LoginBaseURL)
	if err != nil {
		return loginStart{}, err
	}
	q := u.Query()
	q.Set("nonce", nonce)
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("client_id", ed.ClientID)
	u.RawQuery = q.Encode()

	p := &pendingLogin{
		LoginID:   loginID,
		Nonce:     nonce,
		Verifier:  verifier,
		Edition:   ed.Value,
		ExpiresAt: time.Now().Add(pollTimeout),
	}

	pendingLogins.Lock()
	pendingLogins.byID[loginID] = p
	pendingLogins.order = append(pendingLogins.order, loginID)
	for len(pendingLogins.order) > maxPendingLogins {
		oldest := pendingLogins.order[0]
		pendingLogins.order = pendingLogins.order[1:]
		delete(pendingLogins.byID, oldest)
	}
	pendingLogins.Unlock()

	return loginStart{
		LoginID:         loginID,
		VerificationURI: u.String(),
		ExpiresIn:       int64(pollTimeout / time.Millisecond),
		Edition:         ed.Value,
	}, nil
}

// CancelLogin 取消一次进行中的登录。
func CancelLogin(loginID string) bool {
	pendingLogins.Lock()
	defer pendingLogins.Unlock()
	if _, ok := pendingLogins.byID[loginID]; !ok {
		return false
	}
	delete(pendingLogins.byID, loginID)
	for i, id := range pendingLogins.order {
		if id == loginID {
			pendingLogins.order = append(pendingLogins.order[:i], pendingLogins.order[i+1:]...)
			break
		}
	}
	return true
}

func takePending(loginID string) *pendingLogin {
	pendingLogins.Lock()
	defer pendingLogins.Unlock()
	p := pendingLogins.byID[loginID]
	if p == nil {
		return nil
	}
	delete(pendingLogins.byID, loginID)
	for i, id := range pendingLogins.order {
		if id == loginID {
			pendingLogins.order = append(pendingLogins.order[:i], pendingLogins.order[i+1:]...)
			break
		}
	}
	return p
}

// PollResult 是轮询一次的结果。
type PollResult struct {
	Status  string   // pending | done | error
	Error   string   `json:",omitempty"`
	Account *Account `json:",omitempty"`
}

var whitelistMessages = map[string]string{
	"NoIpPermission": "企业设置了 IP 白名单，当前 IP 无法登录",
	"AppDisable":     "Qoder 应用已被停用",
	"LoginExpire":    "Qoder 登录已失效，请重试",
	"NotAllow":       "当前账号暂无 Qoder 使用权限",
	"NOT_ALLOW":      "当前账号暂无 Qoder 使用权限",
}

// PollLogin 轮询一次设备流。
func PollLogin(ctx context.Context, loginID string) PollResult {
	pendingLogins.Lock()
	p := pendingLogins.byID[loginID]
	pendingLogins.Unlock()
	if p == nil {
		return PollResult{Status: "error", Error: "登录会话不存在或已过期，请重新发起"}
	}
	if time.Now().After(p.ExpiresAt) {
		takePending(loginID)
		return PollResult{Status: "error", Error: "登录已超时，请重试"}
	}
	ed := editionOf(p.Edition)

	pollURL, err := url.Parse(ed.OpenAPIBaseURL + ed.PollPath)
	if err != nil {
		return PollResult{Status: "error", Error: err.Error()}
	}
	q := pollURL.Query()
	q.Set("nonce", p.Nonce)
	q.Set("verifier", p.Verifier)
	q.Set("challenge_method", "S256")
	pollURL.RawQuery = q.Encode()

	res, err := doJSON(ctx, http.MethodGet, pollURL.String(), nil, nil, 20*time.Second)
	if err != nil {
		return PollResult{Status: "pending", Error: err.Error()}
	}
	if res.Status == http.StatusNotFound {
		return PollResult{Status: "pending"}
	}
	if !res.OK {
		return PollResult{Status: "error", Error: fmt.Sprintf("轮询失败 HTTP %d: %s", res.Status, res.errText())}
	}
	token := obj(res.JSON)
	tokenValue := firstStr(token, "token", "device_token", "access_token", "accessToken")
	if tokenValue == "" {
		return PollResult{Status: "pending"}
	}
	takePending(loginID)

	authHeaders := map[string]string{"Authorization": "Bearer " + tokenValue}
	userInfo, _ := doJSON(ctx, http.MethodGet, ed.OpenAPIBaseURL+ed.UserInfoPath, authHeaders, nil, 20*time.Second)
	userPlan, _ := doJSON(ctx, http.MethodGet, ed.OpenAPIBaseURL+ed.UserPlanPath, authHeaders, nil, 20*time.Second)

	statusHeaders := map[string]string{
		"Authorization":   "Bearer " + tokenValue,
		"Cosy-ClientType": "0",
		"Cosy-MachineOS":  runtimeArch() + "_" + runtimeOS(),
	}
	userStatus, _ := doJSON(ctx, http.MethodGet, ed.OpenAPIBaseURL+ed.UserStatusPath, statusHeaders, nil, 20*time.Second)

	status := obj(userStatus.JSON)
	info := obj(userInfo.JSON)
	plan := obj(userPlan.JSON)

	if wl := firstStr(status, "whitelistStatus"); wl != "" {
		if msg, ok := whitelistMessages[wl]; ok {
			return PollResult{Status: "error", Error: msg}
		}
		if strings.HasPrefix(strings.ToUpper(wl), "NOT") {
			return PollResult{Status: "error", Error: "当前账号暂无 Qoder 使用权限"}
		}
	}

	uid := firstStr(status, "id")
	if uid == "" {
		uid = firstStr(token, "user_id", "userId")
	}
	if uid == "" {
		uid = firstStr(info, "id", "uid")
	}
	if uid == "" {
		return PollResult{Status: "error", Error: "登录成功但上游未返回账号 ID，请重试"}
	}

	now := time.Now().UnixMilli()
	account := &Account{
		ID:                   randomID("acc"),
		Edition:              ed.Value,
		Name:                 firstStr(info, "name", "username"),
		Email:                firstStr(info, "email"),
		Phone:                firstStr(info, "phone", "security_mobile", "securityMobile"),
		AvatarURL:            firstStr(info, "avatarUrl", "avatar_url"),
		UID:                  uid,
		AID:                  firstStr(info, "aid"),
		YxUID:                firstStr(status, "yxUid", "yx_uid"),
		OrgID:                firstStr(status, "orgId", "org_id"),
		OrgName:              firstStr(status, "orgName", "org_name"),
		UserType:             firstStr(status, "userType", "user_type"),
		SecurityToken:        tokenValue,
		RefreshToken:         firstStr(token, "refresh_token", "refreshToken"),
		TokenExpireAt:        normalizeExpire(token["expires_at"]),
		RefreshTokenExpireAt: normalizeExpire(token["refresh_token_expires_at"]),
		AuthSource:           "oauth",
		Status:               "active",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if account.UserType == "" {
		account.UserType = firstStr(info, "userType", "user_type")
	}
	if account.UserType == "" {
		account.UserType = firstStr(plan, "user_type", "userType")
	}
	if account.UserType == "" {
		account.UserType = "personal_standard"
	}
	if tier := firstStr(plan, "plan_tier_name", "planTierName"); tier != "" {
		account.Plan = &Plan{
			UserType:     firstStr(plan, "user_type", "userType"),
			PlanTierName: tier,
			EndDate:      normalizeExpire(firstAny(plan, "end_date", "endDate")),
		}
	}
	if quota, ok := firstNumber(status, "quota"); ok {
		account.Quota = &Quota{Remaining: ptrFloat(quota), IsQuotaExceeded: boolean(status, "isQuotaExceeded", false)}
	}
	return PollResult{Status: "done", Account: account}
}

// firstAny 返回第一个非 nil 的值。
func firstAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if m == nil {
			continue
		}
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

/* ---------------- 账号资料 / 续期 ---------------- */

// FetchProfile 拉取 userinfo，回填手机号/邮箱等缺失字段。
func FetchProfile(ctx context.Context, ed *Edition, account Account) (name, email, phone string, ok bool) {
	token := account.SecurityToken
	if token == "" {
		token = account.PAT
	}
	res, err := doJSON(ctx, http.MethodGet, ed.OpenAPIBaseURL+ed.UserInfoPath, map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/json",
		"User-Agent":    "Qoder",
	}, nil, 15*time.Second)
	if err != nil || !res.OK {
		return "", "", "", false
	}
	info := obj(res.JSON)
	return firstStr(info, "name", "username"),
		firstStr(info, "email"),
		firstStr(info, "phone", "security_mobile", "securityMobile"),
		true
}

// refreshedToken 是续期结果。
type refreshedToken struct {
	SecurityToken        string
	RefreshToken         string
	TokenExpireAt        int64
	RefreshTokenExpireAt int64
}

// RefreshAccountToken 用 refresh_token / PAT 续期。
//
// 设备流（drt-）只走 deviceToken/refresh；旧版 PAT 账号（jrt-）先走
// jobToken/refresh，失败时用 pt- 重新兑换 job token。
func RefreshAccountToken(ctx context.Context, ed *Edition, account Account) (refreshedToken, error) {
	refresh := account.RefreshToken
	pat := account.PAT
	if refresh == "" && pat == "" {
		return refreshedToken{}, fmt.Errorf("账号没有 refresh_token，无法续期，请重新登录")
	}
	var errs []string
	deviceRefresh := func() (refreshedToken, error) {
		raw, err := postRefresh(ctx, ed.OpenAPIBaseURL+ed.RefreshPath, map[string]any{"refresh_token": refresh})
		if err != nil {
			return refreshedToken{}, err
		}
		return normalizeRefresh(raw, account)
	}
	jobRefresh := func() (refreshedToken, error) {
		raw, err := postRefresh(ctx, ed.OpenAPIBaseURL+"/api/v1/jobToken/refresh", map[string]any{"refresh_token": refresh})
		if err != nil {
			return refreshedToken{}, err
		}
		return normalizeRefresh(raw, account)
	}
	patExchange := func() (refreshedToken, error) {
		raw, err := postRefresh(ctx, ed.OpenAPIBaseURL+"/api/v1/jobToken/exchange", map[string]any{"personal_token": pat})
		if err != nil {
			return refreshedToken{}, err
		}
		return normalizeRefresh(raw, account)
	}

	if strings.HasPrefix(refresh, "drt-") {
		if out, err := deviceRefresh(); err == nil {
			return out, nil
		} else {
			errs = append(errs, "设备令牌刷新失败: "+err.Error())
		}
	}
	if strings.HasPrefix(refresh, "jrt-") {
		if out, err := jobRefresh(); err == nil {
			return out, nil
		} else {
			errs = append(errs, "Job Token 刷新失败: "+err.Error())
		}
		if pat != "" {
			if out, err := patExchange(); err == nil {
				return out, nil
			} else {
				errs = append(errs, "PAT 兑换失败: "+err.Error())
			}
		}
		return refreshedToken{}, fmt.Errorf("%s", strings.Join(errs, "；"))
	}
	if refresh == "" && pat != "" {
		if out, err := patExchange(); err == nil {
			return out, nil
		} else {
			errs = append(errs, "PAT 兑换失败: "+err.Error())
		}
	}
	if refresh != "" {
		if out, err := deviceRefresh(); err == nil {
			return out, nil
		} else {
			errs = append(errs, "设备令牌刷新失败: "+err.Error())
		}
	}
	if len(errs) == 0 {
		return refreshedToken{}, fmt.Errorf("刷新失败")
	}
	return refreshedToken{}, fmt.Errorf("%s", strings.Join(errs, "；"))
}

func postRefresh(ctx context.Context, url string, body map[string]any) (any, error) {
	res, err := doJSON(ctx, http.MethodPost, url, map[string]string{"Content-Type": "application/json"}, body, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("HTTP %d: %s", res.Status, res.errText())
	}
	return res.JSON, nil
}

// normalizeRefresh 归一化续期响应。
// jobToken 的 expires_in 单位是毫秒，直接加到当前时间。
func normalizeRefresh(raw any, account Account) (refreshedToken, error) {
	data := obj(raw)
	if inner := obj(data["data"]); inner != nil {
		data = inner
	}
	token := firstStr(data, "device_token", "token", "securityOauthToken", "access_token", "accessToken")
	if token == "" {
		return refreshedToken{}, fmt.Errorf("刷新响应缺少 token")
	}
	expireAt := normalizeExpire(firstAny(data, "expires_at", "expireTime", "expiresAt"))
	if expireAt == 0 {
		if secs, ok := firstNumber(data, "expires_in", "expiresIn"); ok && secs > 0 {
			expireAt = time.Now().UnixMilli() + int64(secs)
		}
	}
	refresh := firstStr(data, "refresh_token", "refreshToken", "refreshTokenValue")
	if refresh == "" {
		refresh = account.RefreshToken
	}
	return refreshedToken{
		SecurityToken:        token,
		RefreshToken:         refresh,
		TokenExpireAt:        expireAt,
		RefreshTokenExpireAt: normalizeExpire(firstAny(data, "refresh_token_expires_at", "refreshTokenExpiresAt", "refreshTokenExpireAt", "refresh_expires_at")),
	}, nil
}

// jsonBody 是管理 API 解析请求体的助手。
func jsonBody(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<20))
	dec.UseNumber()
	return dec.Decode(out)
}
