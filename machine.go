package main

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

/*
机器身份（Cosy-Machine*）。

服务端按 Cosy-Machine* 头过滤设备定向活动：只带 Cosy-ClientType / Cosy-Version 时
「每日领取 100 Credits」会被静默过滤，列表里只剩营销活动；派生出来的假机器身份
同样会被整条过滤。因此本网关只有在确实持有真实身份时才发送机器头：

  - 账号自带 machine_token / machine_type（从官方客户端导入，或手工填写）
  - 或部署时通过环境变量提供：QODER_MACHINE_TOKEN / QODER_MACHINE_TYPE /
    QODER_MACHINE_CODE（容器内没有官方 runtime-info 原生桥）

缺失时不发机器头，签到只可能拿到营销活动，管理页会明说这一点。
*/

func runtimeOS() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

func runtimeArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// machineIDForAccount 返回与官方客户端同形的机器 ID：uid + 固定盐派生，同账号稳定。
func machineIDForAccount(a *Account) string {
	if a.MachineID != "" {
		return a.MachineID
	}
	uid := a.UID
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte("machine:" + uid))
	h := hex.EncodeToString(sum[:])
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

// machineIdentity 是活动链路需要的一组机器凭据。
type machineIdentity struct {
	Token string `json:"machine_token"`
	Type  string `json:"machine_type"`
	Code  string `json:"machine_code"`
	At    int64  `json:"at"`
}

var machineCache = struct {
	sync.Mutex
	byEdition map[string]*machineIdentity
}{byEdition: map[string]*machineIdentity{}}

const machineIdentityTTL = 30 * time.Minute

// envMachineIdentity 读取部署时注入的机器身份。
func envMachineIdentity() *machineIdentity {
	token := strings.TrimSpace(os.Getenv("QODER_MACHINE_TOKEN"))
	typ := strings.TrimSpace(os.Getenv("QODER_MACHINE_TYPE"))
	code := strings.TrimSpace(os.Getenv("QODER_MACHINE_CODE"))
	if token == "" || typ == "" {
		return nil
	}
	return &machineIdentity{Token: token, Type: typ, Code: code, At: time.Now().UnixMilli()}
}

// getMachineIdentity 返回档位可用的真实机器身份；没有则返回 nil。
// 只有国际版的设备定向活动需要它，但身份本身与档位无关，环境变量对所有档位生效。
func getMachineIdentity(edition string) *machineIdentity {
	if edition != "international" {
		return nil
	}
	machineCache.Lock()
	defer machineCache.Unlock()
	if hit, ok := machineCache.byEdition[edition]; ok {
		if time.Since(time.UnixMilli(hit.At)) < machineIdentityTTL {
			return hit
		}
		delete(machineCache.byEdition, edition)
	}
	ident := envMachineIdentity()
	if ident == nil {
		return nil
	}
	machineCache.byEdition[edition] = ident
	return ident
}

// machineHostname 返回容器主机名，用于 Cosy-MachineHostname。
func machineHostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}
