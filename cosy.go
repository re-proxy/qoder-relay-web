package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

/*
Cosy 签名协议（Qoder IDE 登录态的鉴权头）。

六步：
 1. AES key = UUID 去横线后的前 16 字符
 2. AES-128-CBC 加密 userInfo JSON，IV 复用 key（Qoder 服务端约定，非标准实践）
 3. RSA PKCS#1 v1.5 用档位公钥加密 AES key，得到 cosy-key
 4. payload 以 base64 编码，作为 authorization 的第一段
 5. sig = md5(payloadB64 + "\n" + cosyKey + "\n" + cosyDate + "\n" + body + "\n" + path)
 6. authorization: Bearer COSY.<payloadB64>.<sig>
*/

const (
	legacyStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	legacyCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	legacyCustomPad      = '$'
)

var rsaKeyCache = map[string]*rsa.PublicKey{}

func cosyPublicKey(pemText string) (*rsa.PublicKey, error) {
	if k, ok := rsaKeyCache[pemText]; ok {
		return k, nil
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("cosy: 无法解析公钥 PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cosy: 解析公钥失败: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("cosy: 公钥不是 RSA")
	}
	rsaKeyCache[pemText] = rsaPub
	return rsaPub, nil
}

// aesKeyMaterial 生成 16 字符 AES key（UUID 去横线后前 16 位）。
func aesKeyMaterial() string {
	return uuidCompact()[:16]
}

// cosyAESEncrypt 使用 AES-128-CBC 加密，IV 复用 key。
func cosyAESEncrypt(plaintext, key string) ([]byte, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad([]byte(plaintext), block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(out, padded)
	return out, nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// legacyEncode 是 Qoder 的自定义 base64：先标准 base64，再「末段前置」重排，最后替换字母表。
func legacyEncode(plaintext string) string {
	std := base64.StdEncoding.EncodeToString([]byte(plaintext))
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	var b strings.Builder
	b.Grow(len(rearranged))
	for _, ch := range rearranged {
		if ch == '=' {
			b.WriteByte(legacyCustomPad)
			continue
		}
		if idx := strings.IndexRune(legacyStdAlphabet, ch); idx >= 0 {
			b.WriteByte(legacyCustomAlphabet[idx])
			continue
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// legacyDecode 是 legacyEncode 的逆变换。
func legacyDecode(encoded string) (string, error) {
	var std strings.Builder
	std.Grow(len(encoded))
	for _, ch := range encoded {
		if ch == '$' {
			std.WriteByte('=')
			continue
		}
		if idx := strings.IndexRune(legacyCustomAlphabet, ch); idx >= 0 {
			std.WriteByte(legacyStdAlphabet[idx])
			continue
		}
		std.WriteRune(ch)
	}
	s := std.String()
	n := len(s)
	a := n / 3
	// 重排是自逆的：同样的三段旋转即可还原
	restored := s[n-a:] + s[a:n-a] + s[:a]
	raw, err := base64.StdEncoding.DecodeString(restored)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// cosyUserInfo 是加密进 authorization 的用户身份载荷。
type cosyUserInfo struct {
	Name             string `json:"name"`
	AID              string `json:"aid"`
	UID              string `json:"uid"`
	YxUID            string `json:"yx_uid"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	UserType         string `json:"user_type"`
	SecurityToken    string `json:"security_oauth_token"`
	RefreshToken     string `json:"refresh_token"`
}

type cosyHeader struct {
	Authorization string
	CosyUser      string
	CosyKey       string
	CosyDate      string
	RequestID     string
}

// buildCosyHeader 构建签名头（与请求头组装解耦，便于模型列表复用）。
func buildCosyHeader(edition *Edition, account *Account, token, requestPath, requestBody string, extraUser map[string]any) (cosyHeader, error) {
	userType := account.UserType
	if userType == "" {
		userType = "personal_standard"
	}
	info := cosyUserInfo{
		Name:             account.Name,
		AID:              account.AID,
		UID:              account.UID,
		YxUID:            account.YxUID,
		OrganizationID:   account.OrgID,
		OrganizationName: account.OrgName,
		UserType:         userType,
		SecurityToken:    token,
		RefreshToken:     account.RefreshToken,
	}
	if extraUser != nil {
		if v, ok := extraUser["name"].(string); ok && v != "" {
			info.Name = v
		}
		if v, ok := extraUser["aid"].(string); ok && v != "" {
			info.AID = v
		}
		if v, ok := extraUser["uid"].(string); ok && v != "" {
			info.UID = v
		}
	}

	userJSON, err := json.Marshal(info)
	if err != nil {
		return cosyHeader{}, err
	}
	aesKey := aesKeyMaterial()
	encryptedInfo, err := cosyAESEncrypt(string(userJSON), aesKey)
	if err != nil {
		return cosyHeader{}, fmt.Errorf("cosy: AES 加密失败: %w", err)
	}

	pub, err := cosyPublicKey(edition.ServerPubKey)
	if err != nil {
		return cosyHeader{}, err
	}
	encKey, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(aesKey))
	if err != nil {
		return cosyHeader{}, fmt.Errorf("cosy: RSA 加密失败: %w", err)
	}
	rsaKeyB64 := base64.StdEncoding.EncodeToString(encKey)

	requestID := uuidCompact()
	cosyDate := strconv.FormatInt(time.Now().Unix(), 10)
	payload := struct {
		Version     string `json:"version"`
		RequestID   string `json:"requestId"`
		Info        string `json:"info"`
		CosyVersion string `json:"cosyVersion"`
		IdeVersion  string `json:"ideVersion"`
	}{
		Version:     "v1",
		RequestID:   requestID,
		Info:        base64.StdEncoding.EncodeToString(encryptedInfo),
		CosyVersion: edition.CosyVersion,
		IdeVersion:  "",
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return cosyHeader{}, err
	}
	payloadB64 := base64.StdEncoding.EncodeToString(payloadJSON)
	sig := md5Hex(payloadB64 + "\n" + rsaKeyB64 + "\n" + cosyDate + "\n" + requestBody + "\n" + requestPath)

	return cosyHeader{
		Authorization: "Bearer COSY." + payloadB64 + "." + sig,
		CosyUser:      info.UID,
		CosyKey:       rsaKeyB64,
		CosyDate:      cosyDate,
		RequestID:     requestID,
	}, nil
}

// legacyStreamHeaders 返回推理请求的完整 Cosy 头。
func legacyStreamHeaders(edition *Edition, account *Account, requestPath, requestBody string) (map[string]string, error) {
	token := account.SecurityToken
	if token == "" {
		token = account.PAT
	}
	cosy, err := buildCosyHeader(edition, account, token, requestPath, requestBody, nil)
	if err != nil {
		return nil, err
	}
	mToken := account.MachineToken
	if mToken == "" {
		mToken = machineToken()
	}
	mType := account.MachineType
	if mType == "" {
		mType = randomHex(18)
	}
	mID := account.MachineID
	if mID == "" {
		mID = uuid()
	}
	return map[string]string{
		"cache-control":     "no-cache",
		"cosy-data-policy":  "AGREE",
		"authorization":     cosy.Authorization,
		"content-type":      "application/json",
		"cosy-clienttype":   "5",
		"cosy-clientip":     "169.254.198.161",
		"cosy-date":         cosy.CosyDate,
		"cosy-key":          cosy.CosyKey,
		"cosy-machineid":    mID,
		"cosy-machinetoken": mToken,
		"cosy-machinetype":  mType,
		"cosy-user":         cosy.CosyUser,
		"cosy-version":      edition.CosyVersion,
		"login-version":     "v2",
		"x-request-id":      cosy.RequestID,
	}, nil
}

// modelListHeaders 返回模型列表专用 Cosy 头。
// 与推理请求的差异（2026-10 实测）：签名路径剥离 /algo 前缀、cosy-version 为 1.0.0、
// data-policy 为 disagree、需要 bodyhash/bodylength/sigpath、GET 不能带请求体（带体会 404）。
func modelListHeaders(edition *Edition, account *Account, requestURL string) (map[string]string, error) {
	token := account.SecurityToken
	if token == "" {
		token = account.PAT
	}
	cosy, err := buildCosyHeader(edition, account, token, "", "", map[string]any{
		"name": account.Name,
		"uid":  account.UID,
		"aid":  account.AID,
	})
	if err != nil {
		return nil, err
	}
	// 上面用空路径签名，这里按模型列表规则重新计算 sigpath 与签名
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return nil, err
	}
	sigPath := parsed.Path
	if strings.HasPrefix(sigPath, "/algo") {
		sigPath = strings.TrimPrefix(sigPath, "/algo")
	}
	body := ""
	payloadB64 := strings.SplitN(strings.TrimPrefix(cosy.Authorization, "Bearer COSY."), ".", 2)[0]
	sig := md5Hex(payloadB64 + "\n" + cosy.CosyKey + "\n" + cosy.CosyDate + "\n" + body + "\n" + sigPath)

	mToken := account.MachineToken
	if mToken == "" {
		mToken = machineToken()
	}
	mType := account.MachineType
	if mType == "" {
		mType = randomHex(9)
	}
	mID := account.MachineID
	if mID == "" {
		mID = uuid()
	}
	return map[string]string{
		"authorization":          "Bearer COSY." + payloadB64 + "." + sig,
		"cosy-data-policy":       "disagree",
		"content-type":           "application/json",
		"accept":                 "application/json",
		"accept-encoding":        "identity",
		"cosy-clienttype":        "5",
		"cosy-clientip":          "127.0.0.1",
		"cosy-date":              cosy.CosyDate,
		"cosy-key":               cosy.CosyKey,
		"cosy-machineid":         mID,
		"cosy-machinetoken":      mToken,
		"cosy-machinetype":       mType,
		"cosy-machineos":         "x86_64_windows",
		"cosy-user":              account.UID,
		"cosy-version":           "1.0.0",
		"cosy-bodyhash":          md5Hex(body),
		"cosy-bodylength":        "0",
		"cosy-sigpath":           sigPath,
		"cosy-organization-id":   "",
		"cosy-organization-tags": "",
		"login-version":          "v2",
		"user-agent":             "Go-http-client/2.0",
		"x-request-id":           uuid(),
	}, nil
}

// billingHeaders 是配额 / 签到 / 计划查询使用的身份头。
//
// campaigns 接口缺少 Cosy-ClientType / Cosy-Version 时会静默返回空活动列表，
// 表现为「永远没有签到活动」。国际版的「每日领取 100 Credits」还要求真实机器身份：
// 缺机器头或发派生假头时，服务端会把签到活动整条过滤掉，列表只剩营销活动。
// 因此只有当 ident 非空（账号自带或部署注入的真实身份）时才补机器头。
func billingHeaders(edition *Edition, account *Account, ident *machineIdentity) map[string]string {
	token := account.SecurityToken
	if token == "" {
		token = account.PAT
	}
	h := map[string]string{
		"Authorization":   "Bearer " + token,
		"Accept":          "application/json",
		"User-Agent":      "Qoder",
		"Cosy-ClientType": "10",
		"Cosy-Version":    defaultDesktopVersion,
	}
	machineToken := account.MachineToken
	machineType := account.MachineType
	machineCode := ""
	if ident != nil {
		if machineToken == "" {
			machineToken = ident.Token
		}
		if machineType == "" {
			machineType = ident.Type
		}
		machineCode = ident.Code
	}
	if machineToken != "" && machineType != "" {
		h["Cosy-MachineId"] = machineIDForAccount(account)
		h["Cosy-MachineToken"] = machineToken
		h["Cosy-MachineType"] = machineType
		h["Cosy-MachineCode"] = machineCode
		h["Cosy-MachineOS"] = machineOS()
		h["Cosy-MachineHostname"] = machineHostname()
	}
	return h
}

const defaultDesktopVersion = "0.4.8"

// machineOS 返回与 Qoder 客户端一致的机器系统标识。
func machineOS() string {
	return runtimeArch() + "_" + runtimeOS()
}
