package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

/*
管理 API：cookie 会话 + 仅 JSON 的写操作 + Origin 校验。

所有写操作要求 Content-Type: application/json，并校验 Origin 与 Host 一致，
从而在不引入 CSRF token 的前提下保护 cookie 会话。
*/

// adminCookie 是管理会话 cookie 名。
const adminCookie = "qoder_admin"

// Config 是进程配置，全部来自环境变量。
type Config struct {
	Addr           string
	DataDir        string
	AdminPassword  string
	MasterKey      string
	DefaultEdition string
	CheckinEnabled bool
	CheckinHour    int
	CheckinMinute  int
	SecureCookie   bool
	Version        string
}

// App 是网关运行期状态。
type App struct {
	cfg    Config
	store  *Store
	root   context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	adminSessions map[string]time.Time
	loginGate     chan struct{}
	inFlight      map[string]int

	proxyMu   sync.Mutex
	proxyStop map[string]context.CancelFunc
}

func newApp(cfg Config) (*App, error) {
	if strings.TrimSpace(cfg.AdminPassword) == "" {
		return nil, errors.New("RELAY_ADMIN_PASSWORD 不能为空")
	}
	store, err := openStore(cfg.DataDir, cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	root, cancel := context.WithCancel(context.Background())
	return &App{
		cfg:           cfg,
		store:         store,
		root:          root,
		cancel:        cancel,
		adminSessions: map[string]time.Time{},
		loginGate:     make(chan struct{}, 2),
		inFlight:      map[string]int{},
		proxyStop:     map[string]context.CancelFunc{},
	}, nil
}

/* ---------------- 响应助手 ---------------- */

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": "relay_error", "code": code, "message": code},
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("多余的内容")
	}
	return nil
}

/* ---------------- 会话 ---------------- */

// guardMutations 校验写请求的 Content-Type 与 Origin。
func guardMutations(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		fail(w, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			fail(w, http.StatusForbidden, "origin_rejected")
			return false
		}
	}
	return true
}

func (a *App) sessionValid(r *http.Request) bool {
	cookie, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.adminSessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(a.adminSessions, cookie.Value)
		return false
	}
	return true
}

func (a *App) setSession(w http.ResponseWriter, r *http.Request) bool {
	select {
	case a.loginGate <- struct{}{}:
		defer time.AfterFunc(time.Second, func() { <-a.loginGate })
	default:
		return false
	}
	token := secret()
	a.mu.Lock()
	for k, t := range a.adminSessions {
		if time.Now().After(t) {
			delete(a.adminSessions, k)
		}
	}
	if len(a.adminSessions) >= 64 {
		a.mu.Unlock()
		return false
	}
	a.adminSessions[token] = time.Now().Add(12 * time.Hour)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || a.cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   43200,
	})
	return true
}

func (a *App) clearSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminCookie); err == nil {
		a.mu.Lock()
		delete(a.adminSessions, cookie.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || a.cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

/* ---------------- 账号视图 ---------------- */

// publicAccount 是返回给管理页的账号视图（不含完整凭证）。
func publicAccount(a Account) map[string]any {
	view := map[string]any{
		"id":               a.ID,
		"edition":          a.Edition,
		"label":            a.Label,
		"name":             a.Name,
		"email":            a.Email,
		"phone":            a.Phone,
		"avatar_url":       a.AvatarURL,
		"uid":              a.UID,
		"user_type":        a.UserType,
		"group":            a.Group,
		"status":           a.Status,
		"last_error":       a.LastError,
		"last_used_at":     a.LastUsedAt,
		"created_at":       a.CreatedAt,
		"updated_at":       a.UpdatedAt,
		"auth_source":      a.AuthSource,
		"token_expire_at":  a.TokenExpireAt,
		"checkin_day":      a.CheckinDay,
		"checkin_result":   a.CheckinResult,
		"checkin_retry_at": a.CheckinAt,
		"checkin_off":      a.CheckinOff,
		"has_token":        a.SecurityToken != "",
		"has_refresh":      a.RefreshToken != "",
		"has_pat":          a.PAT != "",
		"token_mask":       maskToken(a.SecurityToken, a.PAT),
		"cooling":          a.coolUntil > time.Now().UnixMilli(),
	}
	if a.Plan != nil {
		view["plan"] = a.Plan
	}
	if a.Quota != nil {
		view["quota"] = a.Quota
	}
	return view
}

// maskToken 展示凭证指纹：长度 <= 12 时只露前 3 位。
func maskToken(token, pat string) string {
	value := token
	if value == "" {
		value = pat
	}
	if value == "" {
		return ""
	}
	if len(value) <= 12 {
		return value[:min(3, len(value))] + "***"
	}
	return value[:6] + "***" + value[len(value)-4:]
}

/* ---------------- 管理路由 ---------------- */

// admin 处理 /admin/* 请求。
func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	if !guardMutations(w, r) {
		return
	}
	path := r.URL.Path

	if path == "/admin/login" && r.Method == http.MethodPost {
		var body struct {
			Password string `json:"password"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			fail(w, http.StatusBadRequest, "invalid_json")
			return
		}
		if !subtleEqual(body.Password, a.cfg.AdminPassword) {
			fail(w, http.StatusUnauthorized, "invalid_admin_password")
			return
		}
		if !a.setSession(w, r) {
			fail(w, http.StatusTooManyRequests, "login_rate_limited")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if path == "/admin/session" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": a.sessionValid(r)})
		return
	}

	if !a.sessionValid(r) {
		fail(w, http.StatusUnauthorized, "admin_login_required")
		return
	}

	switch {
	case path == "/admin/logout" && r.Method == http.MethodPost:
		a.clearSession(w, r)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case path == "/admin/state" && r.Method == http.MethodGet:
		a.adminState(w, r)
	case path == "/admin/account" && r.Method == http.MethodPost:
		a.adminAccount(w, r)
	case path == "/admin/accounts/import" && r.Method == http.MethodPost:
		a.adminImport(w, r)
	case path == "/admin/accounts/export" && r.Method == http.MethodGet:
		a.adminExport(w, r)
	case path == "/admin/account/refresh" && r.Method == http.MethodPost:
		a.adminRefresh(w, r)
	case path == "/admin/account/credit" && r.Method == http.MethodPost:
		a.adminCredit(w, r)
	case path == "/admin/account/checkin" && r.Method == http.MethodPost:
		a.adminCheckin(w, r)
	case path == "/admin/oauth/start" && r.Method == http.MethodPost:
		a.adminOAuthStart(w, r)
	case path == "/admin/oauth/poll" && r.Method == http.MethodPost:
		a.adminOAuthPoll(w, r)
	case path == "/admin/oauth/cancel" && r.Method == http.MethodPost:
		a.adminOAuthCancel(w, r)
	case path == "/admin/key" && r.Method == http.MethodPost:
		a.adminKey(w, r)
	case path == "/admin/model" && r.Method == http.MethodPost:
		a.adminModel(w, r)
	case path == "/admin/models/refresh" && r.Method == http.MethodPost:
		a.adminModelsRefresh(w, r)
	case path == "/admin/proxy" && r.Method == http.MethodPost:
		a.adminProxy(w, r)
	case path == "/admin/usage/clear" && r.Method == http.MethodPost:
		a.adminUsageClear(w, r)
	default:
		http.NotFound(w, r)
	}
}

// adminState 返回管理页所需的全部状态。
func (a *App) adminState(w http.ResponseWriter, r *http.Request) {
	accounts := a.store.Accounts("")
	views := make([]any, 0, len(accounts))
	for _, acc := range accounts {
		views = append(views, publicAccount(acc))
	}
	keys := make([]any, 0)
	for _, k := range a.store.Keys() {
		keys = append(keys, map[string]any{
			"id":           k.ID,
			"name":         k.Name,
			"enabled":      k.Enabled,
			"created":      k.Created,
			"secret":       k.Secret,
			"calls":        k.Calls,
			"last_used_at": k.LastUsedAt,
		})
	}
	models := a.modelCatalog(r.Context(), a.cfg.DefaultEdition)
	modelViews := make([]any, 0, len(models))
	settings := a.store.Models()
	for _, m := range models {
		entry := map[string]any{
			"id":           m.ID,
			"display_name": m.DisplayName,
			"owned_by":     m.OwnedBy,
			"is_reasoning": m.IsReasoning,
			"source":       m.Source,
		}
		if s, ok := settings[m.ID]; ok {
			entry["disabled"] = s.Disabled
			entry["note"] = s.Note
		}
		modelViews = append(modelViews, entry)
	}
	usage := a.store.Usage(200)
	checkinLog := a.store.CheckinLog(50)
	a.mu.Lock()
	inFlight := 0
	for _, n := range a.inFlight {
		inFlight += n
	}
	a.mu.Unlock()
	proxyCfg := a.store.ProxyConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        views,
		"keys":            keys,
		"models":          modelViews,
		"usage":           usage,
		"checkin_log":     checkinLog,
		"counts":          a.store.Counts(),
		"in_flight":       inFlight,
		"proxy":           proxyCfg,
		"proxy_running":   a.proxyRunning(),
		"default_edition": a.cfg.DefaultEdition,
		"checkin_enabled": a.cfg.CheckinEnabled,
		"checkin_at":      fmt.Sprintf("%02d:%02d", a.cfg.CheckinHour, a.cfg.CheckinMinute),
		"version":         a.cfg.Version,
		"account_groups":  accountGroups,
		"editions":        editionOptions(),
	})
}

func editionOptions() []map[string]string {
	out := make([]map[string]string, 0, len(editionList))
	for _, value := range editionList {
		ed := editions[value]
		out = append(out, map[string]string{"value": ed.Value, "label": ed.Label})
	}
	return out
}

// adminAccount 处理账号的增删改与启停。
func (a *App) adminAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Name   string `json:"name"`
		Value  string `json:"value"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	switch body.Action {
	case "rename":
		name := strings.TrimSpace(body.Name)
		if name == "" || len(name) > 120 {
			fail(w, http.StatusBadRequest, "invalid_name")
			return
		}
		if _, ok, err := a.store.UpdateAccount(body.ID, func(x *Account) { x.Label = name }); err != nil || !ok {
			fail(w, http.StatusNotFound, "account_not_found")
			return
		}
	case "group":
		if _, ok, err := a.store.UpdateAccount(body.ID, func(x *Account) { x.Group = body.Value }); err != nil || !ok {
			fail(w, http.StatusNotFound, "account_not_found")
			return
		}
	case "enable":
		if _, ok, err := a.store.UpdateAccount(body.ID, func(x *Account) { x.Status = "active"; x.LastError = "" }); err != nil || !ok {
			fail(w, http.StatusNotFound, "account_not_found")
			return
		}
	case "disable":
		if _, ok, err := a.store.UpdateAccount(body.ID, func(x *Account) { x.Status = "disabled" }); err != nil || !ok {
			fail(w, http.StatusNotFound, "account_not_found")
			return
		}
	case "checkin_off", "checkin_on":
		if _, err := a.store.SetCheckinOff(body.ID, body.Action == "checkin_off"); err != nil {
			fail(w, http.StatusInternalServerError, "storage_write_failed")
			return
		}
	case "delete":
		ok, err := a.store.RemoveAccount(body.ID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "storage_write_failed")
			return
		}
		if !ok {
			fail(w, http.StatusNotFound, "account_not_found")
			return
		}
	default:
		fail(w, http.StatusBadRequest, "invalid_action")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminImport 支持两种导入：管理页导出文件、Qoder 客户端登录态。
func (a *App) adminImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Edition  string `json:"edition"`
		Mode     string `json:"mode"` // backup | client
		Accounts []struct {
			Name          string `json:"name"`
			Email         string `json:"email"`
			Phone         string `json:"phone"`
			UID           string `json:"uid"`
			AID           string `json:"aid"`
			SecurityToken string `json:"security_token"`
			RefreshToken  string `json:"refresh_token"`
			PAT           string `json:"pat"`
			UserType      string `json:"user_type"`
			Group         string `json:"group"`
		} `json:"accounts"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	edition := body.Edition
	if edition == "" {
		edition = a.cfg.DefaultEdition
	}
	imported := 0
	var firstErr string
	for _, item := range body.Accounts {
		acc := Account{
			Edition:       edition,
			Name:          item.Name,
			Email:         item.Email,
			Phone:         item.Phone,
			UID:           item.UID,
			AID:           item.AID,
			SecurityToken: item.SecurityToken,
			RefreshToken:  item.RefreshToken,
			PAT:           item.PAT,
			UserType:      item.UserType,
			Group:         item.Group,
			AuthSource:    "import",
		}
		if acc.SecurityToken == "" && acc.PAT == "" {
			firstErr = "导入项缺少 security_token 或 pat"
			continue
		}
		if _, err := a.store.UpsertAccount(acc); err != nil {
			firstErr = err.Error()
			continue
		}
		imported++
	}
	if imported == 0 && firstErr != "" {
		fail(w, http.StatusBadRequest, firstErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported})
}

// adminExport 导出账号备份（含凭证，仅供用户本地保存）。
func (a *App) adminExport(w http.ResponseWriter, r *http.Request) {
	edition := r.URL.Query().Get("edition")
	accounts := a.store.Accounts(edition)
	out := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, map[string]any{
			"edition":        acc.Edition,
			"name":           acc.Name,
			"email":          acc.Email,
			"phone":          acc.Phone,
			"uid":            acc.UID,
			"aid":            acc.AID,
			"security_token": acc.SecurityToken,
			"refresh_token":  acc.RefreshToken,
			"pat":            acc.PAT,
			"user_type":      acc.UserType,
			"group":          acc.Group,
		})
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"qoder-accounts-%s.json\"", time.Now().Format("20060102")))
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  1,
		"exported": time.Now().UnixMilli(),
		"accounts": out,
	})
}

// adminRefresh 刷新账号资料与登录态。
func (a *App) adminRefresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	acc, ok := a.store.Account(body.ID)
	if !ok {
		fail(w, http.StatusNotFound, "account_not_found")
		return
	}
	ed := editionOf(acc.Edition)
	updated, refreshed := a.refreshAccount(r.Context(), acc)
	if !refreshed {
		// 没有 refresh_token 时至少刷新一次资料，确认登录态是否仍有效。
		name, email, phone, ok := FetchProfile(r.Context(), ed, acc)
		if !ok {
			a.store.MarkFailure(acc.ID, http.StatusUnauthorized, "登录态已失效，请重新登录")
			fail(w, http.StatusBadGateway, "token_invalid")
			return
		}
		updated, _, _ = a.store.UpdateAccount(acc.ID, func(x *Account) {
			if name != "" {
				x.Name = name
			}
			if email != "" {
				x.Email = email
			}
			if phone != "" {
				x.Phone = phone
			}
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": publicAccount(updated)})
}

// adminCredit 查询额度与订阅计划。
func (a *App) adminCredit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	acc, ok := a.store.Account(body.ID)
	if !ok {
		fail(w, http.StatusNotFound, "account_not_found")
		return
	}
	ed := editionOf(acc.Edition)
	usage, err := GetQuotaUsage(r.Context(), ed, acc)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	plan, planErr := GetUserPlan(r.Context(), ed, acc)
	updated, _, _ := a.store.UpdateAccount(acc.ID, func(x *Account) {
		if usage.Quota != nil {
			x.Quota = usage.Quota
		}
		if planErr == nil && plan.PlanTierName != "" {
			x.Plan = &Plan{UserType: plan.UserType, PlanTierName: plan.PlanTierName, EndDate: plan.EndDate}
		}
	})
	payload := map[string]any{"ok": true, "usage": usage, "account": publicAccount(updated)}
	if planErr == nil {
		payload["plan"] = plan
	}
	writeJSON(w, http.StatusOK, payload)
}

// adminCheckin 手动触发签到。
func (a *App) adminCheckin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if body.ID == "" {
		a.runCheckin(r.Context(), "")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	acc, ok := a.store.Account(body.ID)
	if !ok {
		fail(w, http.StatusNotFound, "account_not_found")
		return
	}
	result, credits, message := a.checkinAccount(r.Context(), acc)
	a.store.SetCheckinState(acc.ID, localDayKey(time.Now()), result, 0)
	a.store.AppendCheckin(CheckinLogEntry{
		Time:    time.Now().UnixMilli(),
		Account: accountDisplayName(acc),
		UID:     acc.UID,
		Edition: acc.Edition,
		Success: result == checkinClaimed || result == checkinAlready,
		Credits: credits,
		Message: message,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      result == checkinClaimed || result == checkinAlready,
		"result":  result,
		"credits": credits,
		"message": message,
	})
}

/* ---------------- OAuth ---------------- */

func (a *App) adminOAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Edition string `json:"edition"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if body.Edition == "" {
		body.Edition = a.cfg.DefaultEdition
	}
	start, err := StartLogin(editionOf(body.Edition))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, start)
}

func (a *App) adminOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LoginID string `json:"login_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	res := PollLogin(r.Context(), body.LoginID)
	if res.Status != "done" || res.Account == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "error": res.Error})
		return
	}
	acc, err := a.store.UpsertAccount(*res.Account)
	if err != nil {
		fail(w, http.StatusInternalServerError, "storage_write_failed")
		return
	}
	// 登录成功后立刻拉一次额度，管理页可直接看到剩余量。
	if usage, err := GetQuotaUsage(r.Context(), editionOf(acc.Edition), acc); err == nil && usage.Quota != nil {
		if updated, ok, err := a.store.UpdateAccount(acc.ID, func(x *Account) { x.Quota = usage.Quota }); err == nil && ok {
			acc = updated
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "done", "account": publicAccount(acc)})
}

func (a *App) adminOAuthCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LoginID string `json:"login_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	CancelLogin(body.LoginID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ---------------- 密钥 / 模型 / 代理 ---------------- */

func (a *App) adminKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action  string `json:"action"` // create | update | delete
		ID      string `json:"id"`
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	switch body.Action {
	case "create":
		key, err := a.store.AddKey(body.Name)
		if err != nil {
			fail(w, http.StatusInternalServerError, "storage_write_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
	case "update":
		key, ok, err := a.store.UpdateKey(body.ID, &body.Name, body.Enabled)
		if err != nil {
			fail(w, http.StatusInternalServerError, "storage_write_failed")
			return
		}
		if !ok {
			fail(w, http.StatusNotFound, "key_not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
	case "delete":
		ok, err := a.store.RemoveKey(body.ID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "storage_write_failed")
			return
		}
		if !ok {
			fail(w, http.StatusNotFound, "key_not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		fail(w, http.StatusBadRequest, "invalid_action")
	}
}

func (a *App) adminModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string       `json:"id"`
		Setting ModelSetting `json:"setting"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.ID == "" || len(body.Setting.Alias) > 120 {
		fail(w, http.StatusBadRequest, "invalid_model_setting")
		return
	}
	if err := a.store.SetModel(body.ID, body.Setting); err != nil {
		fail(w, http.StatusInternalServerError, "storage_write_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) adminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	edition, _ := a.resolveEdition(r, map[string]any{})
	acc, ok := a.store.PickAccount(edition, false)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "no_enabled_account")
		return
	}
	models := fetchUpstreamModels(r.Context(), editionOf(edition), acc)
	if len(models) == 0 {
		fail(w, http.StatusBadGateway, "model_catalog_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(models)})
}

func (a *App) adminProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled           *bool  `json:"enabled"`
		Host              string `json:"host"`
		DomesticPort      int    `json:"domestic_port"`
		InternationalPort int    `json:"international_port"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	cfg := a.store.ProxyConfig()
	if body.Enabled != nil {
		cfg.Enabled = *body.Enabled
	}
	if strings.TrimSpace(body.Host) != "" {
		cfg.Host = strings.TrimSpace(body.Host)
	}
	if body.DomesticPort > 0 && body.DomesticPort < 65536 {
		cfg.DomesticPort = body.DomesticPort
	}
	if body.InternationalPort > 0 && body.InternationalPort < 65536 {
		cfg.InternationalPort = body.InternationalPort
	}
	if cfg.DomesticPort == cfg.InternationalPort {
		fail(w, http.StatusBadRequest, "port_conflict")
		return
	}
	if err := a.store.SetProxyConfig(cfg); err != nil {
		fail(w, http.StatusInternalServerError, "storage_write_failed")
		return
	}
	a.syncProxy()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy": cfg, "running": a.proxyRunning()})
}

func (a *App) adminUsageClear(w http.ResponseWriter, r *http.Request) {
	if err := a.store.ClearUsage(); err != nil {
		fail(w, http.StatusInternalServerError, "storage_write_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
