package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Plan 是账号订阅套餐快照。
type Plan struct {
	UserType     string `json:"user_type"`
	PlanTierName string `json:"plan_tier_name"`
	EndDate      int64  `json:"end_date,omitempty"`
}

// Quota 是账号额度快照（percentage 语义为「已用比例」）。
type Quota struct {
	Total           *float64 `json:"total,omitempty"`
	Used            *float64 `json:"used,omitempty"`
	Remaining       *float64 `json:"remaining,omitempty"`
	Percentage      *float64 `json:"percentage,omitempty"`
	Unit            string   `json:"unit,omitempty"`
	IsQuotaExceeded bool     `json:"is_quota_exceeded,omitempty"`
	ExpiresAt       int64    `json:"expires_at,omitempty"`
}

// Account 是一个 Qoder 账号（含登录态，整体加密落盘）。
type Account struct {
	ID        string `json:"id"`
	Edition   string `json:"edition"`
	Label     string `json:"label,omitempty"`
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	UID       string `json:"uid"`
	AID       string `json:"aid,omitempty"`
	YxUID     string `json:"yx_uid,omitempty"`
	OrgID     string `json:"org_id,omitempty"`
	OrgName   string `json:"org_name,omitempty"`
	UserType  string `json:"user_type"`
	Group     string `json:"group,omitempty"`

	// 登录态。SecurityToken 对应 Qoder 的 security_oauth_token，PAT 为个人令牌。
	SecurityToken        string `json:"security_token,omitempty"`
	RefreshToken         string `json:"refresh_token,omitempty"`
	PAT                  string `json:"pat,omitempty"`
	TokenExpireAt        int64  `json:"token_expire_at,omitempty"`
	RefreshTokenExpireAt int64  `json:"refresh_token_expire_at,omitempty"`
	AuthSource           string `json:"auth_source,omitempty"`

	// 机器身份（Cosy 头）。
	MachineID    string `json:"machine_id,omitempty"`
	MachineToken string `json:"machine_token,omitempty"`
	MachineType  string `json:"machine_type,omitempty"`

	Plan  *Plan  `json:"plan,omitempty"`
	Quota *Quota `json:"quota,omitempty"`

	Status     string `json:"status"` // active | needs_relogin | disabled
	LastError  string `json:"last_error,omitempty"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`

	// 签到状态（持久化，跨进程重启保留）
	CheckinDay    string `json:"checkin_day,omitempty"`
	CheckinResult int    `json:"checkin_result,omitempty"`
	CheckinAt     int64  `json:"checkin_retry_at,omitempty"`
	CheckinOff    bool   `json:"checkin_off,omitempty"`

	// 运行期字段，不落盘
	coolUntil int64
}

// APIKey 是一个对外暴露的网关密钥。
type APIKey struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Secret  string `json:"secret"`
	Enabled bool   `json:"enabled"`
	Created int64  `json:"created"`
	// LastUsedAt 记录最近一次成功调用时间，用于管理页展示。
	LastUsedAt int64 `json:"last_used_at,omitempty"`
	Calls      int64 `json:"calls,omitempty"`
}

// ModelSetting 是模型目录的管理覆盖项。
type ModelSetting struct {
	Disabled bool   `json:"disabled,omitempty"`
	Alias    string `json:"alias,omitempty"`
	// Note 记录自定义说明，管理页展示用。
	Note string `json:"note,omitempty"`
}

// Usage 是一条调用记录。
type Usage struct {
	Time       int64  `json:"time"`
	Model      string `json:"model"`
	Account    string `json:"account"`
	AccountID  string `json:"account_id"`
	Edition    string `json:"edition"`
	KeyName    string `json:"key_name,omitempty"`
	Input      int64  `json:"input_tokens"`
	Output     int64  `json:"output_tokens"`
	CacheRead  int64  `json:"cache_read_tokens,omitempty"`
	Success    bool   `json:"success"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// State 是整体加密落盘的网关状态。
type State struct {
	Accounts []Account               `json:"accounts"`
	Keys     []APIKey                `json:"keys"`
	Models   map[string]ModelSetting `json:"models"`
	Usage    []Usage                 `json:"usage"`
	// CheckinLog 保存最近的签到结果，供管理页展示。
	CheckinLog []CheckinLogEntry `json:"checkin_log,omitempty"`
	// Proxy 保存反代监听配置。
	Proxy ProxyConfig `json:"proxy"`
}

// ProxyConfig 是内置反向代理的配置。
type ProxyConfig struct {
	Enabled           bool   `json:"enabled"`
	Host              string `json:"host"`
	DomesticPort      int    `json:"domestic_port"`
	InternationalPort int    `json:"international_port"`
}

// CheckinLogEntry 是一条签到记录。
type CheckinLogEntry struct {
	Time    int64   `json:"time"`
	Account string  `json:"account"`
	UID     string  `json:"uid"`
	Edition string  `json:"edition"`
	Success bool    `json:"success"`
	Credits float64 `json:"credits,omitempty"`
	Message string  `json:"message"`
}

// Store 是加密状态存储。
type Store struct {
	mu    sync.Mutex
	state State
	dir   string
	aead  cipher.AEAD
}

const storeAAD = "qoder-relay-web:v1"

// openStore 打开（或初始化）加密状态文件。
func openStore(dir, masterKey string) (*Store, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(masterKey))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("RELAY_MASTER_KEY 必须是 base64 编码的 32 字节密钥")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, aead: aead, state: newState()}
	file := filepath.Join(dir, "state.enc")
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.saveLocked()
	}
	if err != nil {
		return nil, err
	}
	if len(b) < aead.NonceSize() {
		return nil, errors.New("状态文件已损坏：长度不足")
	}
	plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte(storeAAD))
	if err != nil {
		return nil, errors.New("无法解密状态文件，请检查 RELAY_MASTER_KEY 是否与创建时一致")
	}
	if err := json.Unmarshal(plain, &s.state); err != nil {
		return nil, err
	}
	s.normalizeLocked()
	return s, nil
}

func newState() State {
	return State{
		Keys:   []APIKey{},
		Models: map[string]ModelSetting{},
		Usage:  []Usage{},
		Proxy: ProxyConfig{
			Enabled:           false,
			Host:              "0.0.0.0",
			DomesticPort:      7891,
			InternationalPort: 7892,
		},
	}
}

// normalizeLocked 修正反序列化后的缺省值。
func (s *Store) normalizeLocked() {
	if s.state.Models == nil {
		s.state.Models = map[string]ModelSetting{}
	}
	if s.state.Keys == nil {
		s.state.Keys = []APIKey{}
	}
	for i := range s.state.Accounts {
		a := &s.state.Accounts[i]
		if a.Edition == "" {
			a.Edition = "domestic"
		}
		if a.Status == "" {
			a.Status = "active"
		}
		if a.UserType == "" {
			a.UserType = "personal_standard"
		}
	}
	if s.state.Proxy.DomesticPort == 0 {
		s.state.Proxy.DomesticPort = 7891
	}
	if s.state.Proxy.InternationalPort == 0 {
		s.state.Proxy.InternationalPort = 7892
	}
	if s.state.Proxy.Host == "" {
		s.state.Proxy.Host = "0.0.0.0"
	}
}

// saveLocked 原子写入加密状态。调用方必须已持有 s.mu。
func (s *Store) saveLocked() error {
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, b, []byte(storeAAD))
	tmp := filepath.Join(s.dir, "state.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(sealed); err == nil {
		err = f.Sync()
		if err != nil {
			f.Close()
			return err
		}
	} else {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.enc"))
}

// Save 在持锁状态下持久化。
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

/* ---------------- 账号 ---------------- */

// Accounts 返回账号副本（按创建时间倒序）。
func (s *Store) Accounts(edition string) []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Account, 0, len(s.state.Accounts))
	for _, a := range s.state.Accounts {
		if edition != "" && a.Edition != edition {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Account 按 ID 取账号。
func (s *Store) Account(id string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.state.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// AccountByUID 按档位 + UID 取账号。
func (s *Store) AccountByUID(edition, uid string) (Account, bool) {
	if uid == "" {
		return Account{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.state.Accounts {
		if a.Edition == edition && a.UID == uid {
			return a, true
		}
	}
	return Account{}, false
}

// UpsertAccount 写入账号；同档位同 UID 时合并（保留既有登录态与额度快照）。
func (s *Store) UpsertAccount(a Account) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Edition == "" {
		a.Edition = "domestic"
	}
	if a.UserType == "" {
		a.UserType = "personal_standard"
	}
	now := time.Now().UnixMilli()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if a.Status == "" {
		if a.SecurityToken != "" || a.PAT != "" {
			a.Status = "active"
		} else {
			a.Status = "needs_relogin"
		}
	}

	if a.UID != "" {
		for i := range s.state.Accounts {
			cur := &s.state.Accounts[i]
			if cur.Edition != a.Edition || cur.UID != a.UID {
				continue
			}
			// 合并：新值优先，缺省沿用旧值
			a.ID = cur.ID
			a.CreatedAt = cur.CreatedAt
			if a.SecurityToken == "" {
				a.SecurityToken = cur.SecurityToken
			}
			if a.RefreshToken == "" {
				a.RefreshToken = cur.RefreshToken
			}
			if a.PAT == "" {
				a.PAT = cur.PAT
			}
			if a.TokenExpireAt == 0 {
				a.TokenExpireAt = cur.TokenExpireAt
			}
			if a.RefreshTokenExpireAt == 0 {
				a.RefreshTokenExpireAt = cur.RefreshTokenExpireAt
			}
			if a.MachineID == "" {
				a.MachineID = cur.MachineID
			}
			if a.MachineToken == "" {
				a.MachineToken = cur.MachineToken
			}
			if a.MachineType == "" {
				a.MachineType = cur.MachineType
			}
			if a.Label == "" {
				a.Label = cur.Label
			}
			if a.Group == "" {
				a.Group = cur.Group
			}
			if a.Quota == nil {
				a.Quota = cur.Quota
			}
			if a.Plan == nil {
				a.Plan = cur.Plan
			}
			if a.AuthSource == "" {
				a.AuthSource = cur.AuthSource
			}
			if a.SecurityToken != "" || a.PAT != "" {
				a.Status = "active"
				a.LastError = ""
			} else {
				a.Status = "needs_relogin"
				a.LastError = "备份不含登录态，请重新登录"
			}
			a.CheckinDay = cur.CheckinDay
			a.CheckinResult = cur.CheckinResult
			a.CheckinAt = cur.CheckinAt
			a.CheckinOff = cur.CheckinOff
			s.state.Accounts[i] = a
			return a, s.saveLocked()
		}
	}

	if a.ID == "" {
		a.ID = randomID("acc")
	}
	if a.SecurityToken == "" && a.PAT == "" {
		a.Status = "needs_relogin"
		a.LastError = "缺少登录凭证"
	}
	s.state.Accounts = append(s.state.Accounts, a)
	return a, s.saveLocked()
}

// UpdateAccount 对账号应用补丁函数。
func (s *Store) UpdateAccount(id string, fn func(*Account)) (Account, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID != id {
			continue
		}
		fn(&s.state.Accounts[i])
		s.state.Accounts[i].UpdatedAt = time.Now().UnixMilli()
		acc := s.state.Accounts[i]
		return acc, true, s.saveLocked()
	}
	return Account{}, false, nil
}

// RemoveAccount 删除账号。
func (s *Store) RemoveAccount(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.state.Accounts {
		if a.ID == id {
			s.state.Accounts = append(s.state.Accounts[:i], s.state.Accounts[i+1:]...)
			return true, s.saveLocked()
		}
	}
	return false, nil
}

// Counts 返回各档位账号数量统计。
func (s *Store) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{"domestic": 0, "international": 0, "proxy_domestic": 0, "proxy_international": 0}
	for _, a := range s.state.Accounts {
		out[a.Edition]++
		if a.Group != "desktop" && a.Status != "disabled" && (a.SecurityToken != "" || a.PAT != "") {
			if a.Edition == "international" {
				out["proxy_international"]++
			} else {
				out["proxy_domestic"]++
			}
		}
	}
	return out
}

/* ---------------- 密钥 ---------------- */

// Keys 返回全部 API Key。
func (s *Store) Keys() []APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]APIKey{}, s.state.Keys...)
}

// AddKey 新建一个密钥。
func (s *Store) AddKey(name string) (APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		name = "新密钥"
	}
	k := APIKey{
		ID:      randomID("key"),
		Name:    name,
		Secret:  "sk-" + secret(),
		Enabled: true,
		Created: time.Now().UnixMilli(),
	}
	s.state.Keys = append(s.state.Keys, k)
	return k, s.saveLocked()
}

// UpdateKey 修改密钥名称 / 启停状态。
func (s *Store) UpdateKey(id string, name *string, enabled *bool) (APIKey, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Keys {
		if s.state.Keys[i].ID != id {
			continue
		}
		if name != nil && strings.TrimSpace(*name) != "" {
			s.state.Keys[i].Name = strings.TrimSpace(*name)
		}
		if enabled != nil {
			s.state.Keys[i].Enabled = *enabled
		}
		k := s.state.Keys[i]
		return k, true, s.saveLocked()
	}
	return APIKey{}, false, nil
}

// RemoveKey 删除密钥。
func (s *Store) RemoveKey(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.state.Keys {
		if k.ID == id {
			s.state.Keys = append(s.state.Keys[:i], s.state.Keys[i+1:]...)
			return true, s.saveLocked()
		}
	}
	return false, nil
}

// AuthorizeKey 校验网关密钥并返回其名称。空密钥集时返回拒绝。
func (s *Store) AuthorizeKey(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Keys {
		k := &s.state.Keys[i]
		if k.Enabled && subtleEqual(k.Secret, key) {
			k.LastUsedAt = time.Now().UnixMilli()
			k.Calls++
			return k.Name, true
		}
	}
	return "", false
}

/* ---------------- 模型设置 ---------------- */

// Models 返回模型设置副本。
func (s *Store) Models() map[string]ModelSetting {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ModelSetting, len(s.state.Models))
	for k, v := range s.state.Models {
		out[k] = v
	}
	return out
}

// SetModel 写入单个模型设置。
func (s *Store) SetModel(id string, setting ModelSetting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if setting == (ModelSetting{}) {
		delete(s.state.Models, id)
	} else {
		s.state.Models[id] = setting
	}
	return s.saveLocked()
}

/* ---------------- 用量 ---------------- */

const maxUsageEntries = 2000

// Record 追加一条用量记录（自动裁剪到上限）。
func (s *Store) Record(u Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.Time == 0 {
		u.Time = time.Now().UnixMilli()
	}
	s.state.Usage = append(s.state.Usage, u)
	if len(s.state.Usage) > maxUsageEntries {
		s.state.Usage = s.state.Usage[len(s.state.Usage)-maxUsageEntries:]
	}
	_ = s.saveLocked()
}

// Usage 返回最近 n 条用量记录（n<=0 表示全部）。
func (s *Store) Usage(n int) []Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.state.Usage
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	out := append([]Usage{}, all...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// ClearUsage 清空用量记录。
func (s *Store) ClearUsage() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Usage = []Usage{}
	return s.saveLocked()
}

/* ---------------- 签到日志 ---------------- */

const maxCheckinLog = 300

// AppendCheckin 追加签到日志。
func (s *Store) AppendCheckin(e CheckinLogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Time == 0 {
		e.Time = time.Now().UnixMilli()
	}
	s.state.CheckinLog = append(s.state.CheckinLog, e)
	if len(s.state.CheckinLog) > maxCheckinLog {
		s.state.CheckinLog = s.state.CheckinLog[len(s.state.CheckinLog)-maxCheckinLog:]
	}
	_ = s.saveLocked()
}

// CheckinLog 返回签到日志（新→旧）。
func (s *Store) CheckinLog(n int) []CheckinLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.state.CheckinLog
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	out := append([]CheckinLogEntry{}, all...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

/* ---------------- 反代配置 ---------------- */

// ProxyConfig 返回反代配置。
func (s *Store) ProxyConfig() ProxyConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Proxy
}

// SetProxyConfig 覆盖反代配置。
func (s *Store) SetProxyConfig(p ProxyConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.DomesticPort == 0 {
		p.DomesticPort = 7891
	}
	if p.InternationalPort == 0 {
		p.InternationalPort = 7892
	}
	if p.Host == "" {
		p.Host = "0.0.0.0"
	}
	s.state.Proxy = p
	return s.saveLocked()
}

/* ---------------- 账号选择 ---------------- */

const (
	cooldownMS     = 120 * 1000
	authCooldownMS = 10 * 60 * 1000
)

var roundRobin = struct {
	sync.Mutex
	next map[string]int
}{next: map[string]int{}}

// PickAccount 在档位内选一个可用账号。
// 规则：跳过禁用 / 冷却中 / 无凭证 / 桌面端专用（反代场景）；优先额度剩余比例最高，同级轮转。
func (s *Store) PickAccount(edition string, forProxy bool) (Account, bool) {
	s.mu.Lock()
	candidates := make([]Account, 0, len(s.state.Accounts))
	now := time.Now().UnixMilli()
	for _, a := range s.state.Accounts {
		if a.Edition != edition {
			continue
		}
		if a.Status == "disabled" {
			continue
		}
		if a.Status == "needs_relogin" && a.RefreshToken == "" && a.PAT == "" {
			continue
		}
		if a.coolUntil > now {
			continue
		}
		if forProxy && a.Group == "desktop" {
			continue
		}
		if a.SecurityToken == "" && a.PAT == "" {
			continue
		}
		candidates = append(candidates, a)
	}
	s.mu.Unlock()

	if len(candidates) == 0 {
		return Account{}, false
	}

	// 过期且无续期手段的账号排除
	usable := candidates[:0:0]
	for _, a := range candidates {
		if a.TokenExpireAt == 0 || a.TokenExpireAt > now || a.RefreshToken != "" || a.PAT != "" {
			usable = append(usable, a)
		}
	}
	if len(usable) == 0 {
		usable = candidates
	}

	// 额度用尽的排最后
	notExhausted := make([]Account, 0, len(usable))
	for _, a := range usable {
		if a.Quota == nil || !a.Quota.IsQuotaExceeded {
			notExhausted = append(notExhausted, a)
		}
	}
	pool := usable
	if len(notExhausted) > 0 {
		pool = notExhausted
	}

	sort.SliceStable(pool, func(i, j int) bool {
		si, sj := quotaScore(pool[i]), quotaScore(pool[j])
		if si != sj {
			return si > sj
		}
		return pool[i].LastUsedAt < pool[j].LastUsedAt
	})

	top := quotaScore(pool[0])
	tier := make([]Account, 0, len(pool))
	for _, a := range pool {
		if quotaScore(a) == top {
			tier = append(tier, a)
		}
	}

	roundRobin.Lock()
	idx := roundRobin.next[edition]
	chosen := tier[idx%len(tier)]
	roundRobin.next[edition] = (idx + 1) % len(tier)
	roundRobin.Unlock()

	return chosen, true
}

// quotaScore 返回账号额度剩余比例，用于排序。
func quotaScore(a Account) float64 {
	q := a.Quota
	if q == nil || q.IsQuotaExceeded {
		return 0
	}
	if q.ExpiresAt > 0 && q.ExpiresAt < time.Now().UnixMilli() {
		return 0
	}
	if q.Remaining != nil && q.Total != nil && *q.Total > 0 {
		return *q.Remaining / *q.Total
	}
	return 0
}

// MarkSuccess 清理失败标记与冷却。
func (s *Store) MarkSuccess(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Accounts {
		a := &s.state.Accounts[i]
		if a.ID != id {
			continue
		}
		changed := a.LastError != "" || a.Status == "needs_relogin"
		a.coolUntil = 0
		if changed {
			a.LastError = ""
			if a.Status != "disabled" {
				a.Status = "active"
			}
			a.UpdatedAt = time.Now().UnixMilli()
			_ = s.saveLocked()
		}
		return
	}
}

// MarkFailure 标记失败并进入冷却。401/403 升级为更长冷却并可能标记需重新登录。
func (s *Store) MarkFailure(id string, status int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cooldown := int64(cooldownMS)
	if status == 401 || status == 403 {
		cooldown = authCooldownMS
	}
	now := time.Now().UnixMilli()
	for i := range s.state.Accounts {
		a := &s.state.Accounts[i]
		if a.ID != id {
			continue
		}
		a.coolUntil = now + cooldown
		code := "?"
		if status > 0 {
			code = fmt.Sprint(status)
		}
		a.LastError = "HTTP " + code + ": " + truncateRunes(message, 200)
		a.UpdatedAt = now
		if (status == 401 || status == 403) && a.RefreshToken == "" && a.PAT == "" {
			a.Status = "needs_relogin"
		}
		_ = s.saveLocked()
		return
	}
}

// SetCheckinState 记录账号的签到结果。
func (s *Store) SetCheckinState(id, day string, result int, retryAt int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Accounts {
		a := &s.state.Accounts[i]
		if a.ID != id {
			continue
		}
		a.CheckinDay = day
		a.CheckinResult = result
		a.CheckinAt = retryAt
		_ = s.saveLocked()
		return
	}
}

// SetCheckinOff 开关某账号的自动签到。
func (s *Store) SetCheckinOff(id string, off bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Accounts {
		if s.state.Accounts[i].ID == id {
			s.state.Accounts[i].CheckinOff = off
			return true, s.saveLocked()
		}
	}
	return false, nil
}

// HasAnyKey 报告是否已配置至少一个启用中的密钥。
func (s *Store) HasAnyKey() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.state.Keys {
		if k.Enabled {
			return true
		}
	}
	return false
}

// subtleEqual 做恒定时间比较，避免密钥逐字节泄露。
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
