package main

// Edition 描述一个 Qoder 部署档位（国内版 / 国际版）。
// 两个版本是两套独立部署：域名、账号体系、Credits 全部不互通。
type Edition struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	ProductName string `json:"product_name"`

	LoginBaseURL   string `json:"login_base_url"`
	OpenAPIBaseURL string `json:"openapi_base_url"`
	ClientID       string `json:"client_id"`
	ClientIDGuess  bool   `json:"client_id_inferred"`

	PollPath        string `json:"poll_path"`
	RefreshPath     string `json:"refresh_path"`
	UserInfoPath    string `json:"user_info_path"`
	UserStatusPath  string `json:"user_status_path"`
	UserPlanPath    string `json:"user_plan_path"`
	QuotaUsagePath  string `json:"quota_usage_path"`
	MeUsagePath     string `json:"me_usage_path"`
	CampaignsPath   string `json:"campaigns_path"`
	ClaimPath       string `json:"claim_path"`
	PATExchangePath string `json:"pat_exchange_path"`

	StreamURL          string   `json:"stream_url"`
	StreamPath         string   `json:"stream_path"`
	ModelListURL       string   `json:"model_list_url"`
	ModelListFallbacks []string `json:"model_list_fallbacks"`
	CosyVersion        string   `json:"cosy_version"`
	ServerPubKey       string   `json:"-"`

	CloudBaseURL string `json:"cloud_base_url"`

	ProxyDefaultPort int `json:"proxy_default_port"`
}

// cosyServerPubKey 是两个档位共用的 Cosy 签名公钥（1024-bit RSA）。
const cosyServerPubKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var editionList = []string{"domestic", "international"}

var editions = map[string]*Edition{
	"domestic": {
		Value:       "domestic",
		Label:       "国内版",
		ProductName: "Qoder CN",

		LoginBaseURL:   "https://qoder.com.cn/device/selectAccounts",
		OpenAPIBaseURL: "https://openapi.qoder.com.cn",
		ClientID:       "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
		ClientIDGuess:  true,

		PollPath:        "/api/v1/deviceToken/poll",
		RefreshPath:     "/api/v1/deviceToken/refresh",
		UserInfoPath:    "/api/v1/userinfo",
		UserStatusPath:  "/api/v3/user/status",
		UserPlanPath:    "/api/v2/user/plan",
		QuotaUsagePath:  "/api/v2/quota/usage",
		MeUsagePath:     "/sash/api/v2/me/usage",
		CampaignsPath:   "/sash/api/v1/me/campaigns",
		ClaimPath:       "/sash/api/v1/me/campaigns",
		PATExchangePath: "/api/v1/jobToken/exchange",

		StreamURL:    "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
		StreamPath:   "/api/v2/service/pro/sse/agent_chat_generation",
		ModelListURL: "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1",
		CosyVersion:  "0.1.43",
		ServerPubKey: cosyServerPubKey,

		CloudBaseURL:     "https://api.qoder.com.cn",
		ProxyDefaultPort: 7891,
	},
	"international": {
		Value:       "international",
		Label:       "国际版",
		ProductName: "Qoder",

		LoginBaseURL:   "https://qoder.com/device/selectAccounts",
		OpenAPIBaseURL: "https://openapi.qoder.sh",
		ClientID:       "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		ClientIDGuess:  false,

		PollPath:        "/api/v1/deviceToken/poll",
		RefreshPath:     "/api/v1/deviceToken/refresh",
		UserInfoPath:    "/api/v1/userinfo",
		UserStatusPath:  "/api/v3/user/status",
		UserPlanPath:    "/api/v2/user/plan",
		QuotaUsagePath:  "/api/v2/quota/usage",
		MeUsagePath:     "/sash/api/v2/me/usage",
		CampaignsPath:   "/sash/api/v1/me/campaigns",
		ClaimPath:       "/sash/api/v1/me/campaigns",
		PATExchangePath: "/api/v1/jobToken/exchange",

		StreamURL:    "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
		StreamPath:   "/api/v2/service/pro/sse/agent_chat_generation",
		ModelListURL: "https://api1.qoder.sh/algo/api/v2/model/list?Encode=1",
		ModelListFallbacks: []string{
			"https://api2.qoder.sh/algo/api/v2/model/list?Encode=1",
			"https://api3.qoder.sh/algo/api/v2/model/list?Encode=1",
		},
		CosyVersion:  "0.1.43",
		ServerPubKey: cosyServerPubKey,

		CloudBaseURL:     "https://api.qoder.com",
		ProxyDefaultPort: 7892,
	},
}

func editionOf(value string) *Edition {
	if e, ok := editions[value]; ok {
		return e
	}
	return editions["domestic"]
}

// accountGroups 是账号分组（沿用 WorkBuddy-Switch2api 的语义）。
var accountGroups = []struct {
	Value string `json:"value"`
	Label string `json:"label"`
}{
	{"", "未分组"},
	{"desktop", "桌面端"},
	{"proxy", "反代API"},
}
