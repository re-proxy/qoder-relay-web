# Qoder 网关（qoder-relay-web）

把 [Qoder](https://qoder.com) 的账号池包装成 OpenAI / Anthropic 兼容的 HTTP 网关。单二进制、单容器、零外部依赖，内置深色运维控制台。

移植自 Qoder-Switch2api，前端风格遵循 `TRAE-RELAY-WEB-FRONTEND-STYLE.md`。

## 能力

- **双档位账号池**：国内版（qoder.com.cn）与国际版（qoder.sh）各自独立选号，互不干扰。
- **登录方式**：Qoder 设备码 OAuth（PKCE S256）、导入客户端登录态、导入账号备份、PAT 个人令牌。
- **对外接口**
  - `POST /v1/chat/completions` — OpenAI Chat Completions（流式 / 非流式）
  - `POST /v1/responses` — OpenAI Responses
  - `POST /v1/messages` — Anthropic Messages
  - `POST /v1/messages/count_tokens` — Anthropic token 预估
  - `GET /v1/models` — 模型目录（默认 + 上游 + 管理页别名）
  - `GET /healthz` — 健康检查
- **鉴权**：管理页用 cookie 会话（`RELAY_ADMIN_PASSWORD`）；`/v1` 用多 API Key（`Authorization: Bearer sk-...` 或 `X-API-Key`）。未创建任何密钥时 `/v1` 放行，创建后立即强制校验。
- **双后端**：有 PAT 走 Cloud 会话后端，有登录 token 走 Legacy SSE 后端，按账号自动分派。
- **额度与计划**：一键拉取剩余额度、订阅档位、到期时间；选号按剩余比例优先并轮转。
- **每日签到**：按设定时间自动领取积分活动，可在账号页手动触发，结果写入签到日志。
- **内置反代**：为两个档位各开一个端口（默认 7891 / 7892），客户端直接指向 `http://主机:端口/v1`，无需额外请求头。
- **加密落盘**：AES-256-GCM 单文件 `state.enc`，密钥来自 `RELAY_MASTER_KEY`。

## 快速开始（Docker）

官方镜像发布在 Docker Hub：`painwind/qoder-relay-web`（`linux/amd64` + `linux/arm64`）。

```bash
# 1. 生成状态加密密钥
docker run --rm painwind/qoder-relay-web:latest keygen
# 或本地：go run . keygen

# 2. 写 .env
cat > .env <<'EOF'
RELAY_ADMIN_PASSWORD=换成你的管理密码
RELAY_MASTER_KEY=上一步输出的base64密钥
EOF

# 3. 启动（使用已发布镜像）
docker compose up -d

# 或者直接 docker run
docker run -d --name qoder-relay-web \
  -p 8317:8317 -p 7891:7891 -p 7892:7892 \
  -v qoder-relay-data:/data \
  -e RELAY_ADMIN_PASSWORD=你的管理密码 \
  -e RELAY_MASTER_KEY=上一步输出的base64密钥 \
  painwind/qoder-relay-web:latest
```

打开 `http://主机:8317`，用管理密码登录。

只想部署、不需要仓库其余部分时，用 `deploy/` 目录（稀疏检出约 5 KB，含 compose、
`.env.example` 与部署说明）：

```bash
git clone --filter=blob:none --sparse https://github.com/re-proxy/qoder-relay-web.git
cd qoder-relay-web && git sparse-checkout set deploy && cd deploy
cp .env.example .env && docker compose up -d
```

想从源码构建，用 `docker compose up -d --build`；只想拉官方镜像，用 `docker compose pull && docker compose up -d`。

数据落在命名卷 `qoder-relay-data`（`state.enc` 属主 uid 10001）。若要改用宿主机目录挂载，
先执行 `chown -R 10001:10001 <目录>`，否则容器会因权限不足反复重启。

镜像标签：`latest`（main 分支）、`main`、`sha-<短提交>`、以及打 tag 时的 `v1.2.3` / `v1.2`。

### CI/CD

`.github/workflows/docker-publish.yml` 在每次 push 到 `main`、打 `v*` tag、或手动触发时运行：

1. `test` 作业：`gofmt -l` 无差异、`go vet ./...`、`go test ./...`、`node --check web/app.js`。
2. `publish` 作业（仅非 PR）：多架构构建并推送到 `painwind/qoder-relay-web`，带 GHA 层缓存。

需要在仓库 `Settings → Secrets and variables → Actions` 配置一个 Secret：

| Secret | 说明 |
| --- | --- |
| `DOCKERHUB_TOKEN` | Docker Hub 账号 `painwind` 的 Access Token（Read & Write）。缺失时 `publish` 作业会直接失败并提示，不会静默跳过。 |

## 本地运行

```bash
go build -o qoder-relay-web .
./qoder-relay-web keygen                       # 生成 RELAY_MASTER_KEY
RELAY_ADMIN_PASSWORD=dev \
RELAY_MASTER_KEY=<上一步输出> \
RELAY_DATA_DIR=./data \
./qoder-relay-web
```

需要 Go 1.25+。构建产物是单个静态二进制，前端资源通过 `//go:embed` 内嵌，无需 Node 与构建步骤。

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RELAY_ADMIN_PASSWORD` | 无（必填） | 管理页登录密码 |
| `RELAY_MASTER_KEY` | 无（必填） | base64 编码的 32 字节密钥，用于加密 `state.enc`；丢失后已存账号无法恢复 |
| `RELAY_ADDR` | `:8317` | 主监听地址 |
| `RELAY_DATA_DIR` | `/data` | 数据目录（容器内） |
| `RELAY_DEFAULT_EDITION` | `domestic` | 请求未指定档位时使用：`domestic` 或 `international` |
| `RELAY_CHECKIN_ENABLED` | `true` | 是否启用每日自动签到 |
| `RELAY_CHECKIN_HOUR` | `8` | 签到小时（0–23） |
| `RELAY_CHECKIN_MINUTE` | `30` | 签到分钟（0–59） |
| `RELAY_SECURE_COOKIE` | `false` | 反代到 HTTPS 后设为 `true`，让管理 cookie 带 Secure |
| `TZ` | `Asia/Shanghai` | 时区，影响签到与日志时间 |

机器身份相关（仅国际版签到需要，通常留空）：`QODER_MACHINE_TOKEN`、`QODER_MACHINE_TYPE`、`QODER_MACHINE_CODE`。

## 使用流程

1. **加账号**：账号页 →「登录新账号」→ 选择档位 → 浏览器完成 Qoder 登录，页面自动轮询并在成功后拉一次额度。
2. **建密钥**：密钥页 →「新建密钥」，列表会显示完整密钥并提供复制按钮。
3. **接客户端**：把 `http://主机:8317/v1` 作为 Base URL，密钥填进 API Key 字段。OpenAI 兼容客户端直接可用；Anthropic 客户端指向同一 Base URL 并勾选 `/v1/messages`。
4. **分档位（可选）**：设置页开启内置反代，把国内客户端指向 `:7891/v1`、国际客户端指向 `:7892/v1`。也可以在主端口上用请求头 `X-Qoder-Edition: international` 或模型前缀 `intl/` 指定档位。

## 接口细节

### 档位判定顺序

1. 请求头 `X-Qoder-Edition`
2. 请求体 `edition` 字段
3. 模型名前缀 `domestic/`、`cn/`、`international/`、`intl/`
4. `RELAY_DEFAULT_EDITION`

### 模型名

模型目录优先取上游 `/algo/api/v2/model/list`（缓存 10 分钟），取不到时回落到内置目录（auto、Qwen3.8-Max/Flash、DeepSeek-V4-Pro/Flash、GLM-5.3/Flash、Kimi-K3.1-Pro/K2.8-Preview、Step-5-Preview、MiniMax-M3.1-Flash、lite）。常见别名会归一化到上游 key，例如 `qwen3.8-max` → `qmodel_38max`、`deepseek-v4-pro` → `dmodel`、`glm-5.3` → `gmodel`、`kimi-k3` → `kmodel_latest`、`step-5-preview` → `jmodel`。也可在模型名后追加 `-high` / `-low` 等后缀指定推理强度。

管理页可给任意模型设置展示别名或隐藏它。

### 账号选择与重试

- 跳过：已停用、冷却中、无凭证、`needs_relogin` 且无刷新手段、反代场景下的「桌面端」分组。
- 排序：额度剩余比例高者优先，同分层内轮转。
- 冷却：普通失败 120 秒，401/403 冷却 10 分钟。
- 重试：单个请求最多 3 次换号；首个响应事件后不再换号；`400/404/413/415/422` 视为请求本身问题，直接返回且不冷却账号。
- 续期：token 5 分钟内过期或遇到 401/403 时用 refresh token / PAT 续期，成功后同一账号立即重试一次。

## 已知限制

- **国际版自动签到**：上游按 `Cosy-Machine*` 头过滤设备定向活动。容器内没有 Qoder 客户端原生桥，拿不到真实机器身份，因此国际版只能领到营销活动，无法领到「每日 100 Credits」。国内版不受影响。需要时可通过环境变量注入真实机器身份。
- **不移植的能力**：写 Qoder 客户端本地登录态、本机会话导出、托盘常驻、应用内自动更新、Tauri 桌面壳。
- **端点覆盖**：上游端点写死在 `editions.go`，暂不提供管理页覆盖入口。
- **Cloud 后端**：仅 PAT 账号可用，会先自动创建环境与 Agent，首轮请求延迟较高。

## 目录结构

```
main.go            入口、配置、//go:embed、控制台路由
editions.go        两档位端点与 RSA 公钥
cosy.go            Cosy 头签名（AES-CBC + RSA + MD5）与 legacy 编解码
http.go            JSON 请求与 SSE 流读取
models.go          模型 key / 别名 / 上游目录缓存
chat.go            legacy 与 cloud 双后端流式实现
oauth.go           设备码登录、token 续期
account_info.go    额度、计划、活动签到
store.go           AES-GCM 加密状态存储与账号选择
machine.go         机器身份（国际版签到）
gateway.go         对外 /v1 接口与错误映射
checkin.go         每日签到调度
admin.go           管理 API 与 cookie 会话
proxy.go           内置分档位反代监听
web/               零依赖控制台（index.html / style.css / app.js）
deploy/            快速部署（compose + .env.example + README，不含源码）
```

## 安全说明

- `state.enc` 内含完整登录凭证，权限 0600，目录 0700。备份请按密钥处理。
- 管理 API 只接受 JSON 写请求，并校验 `Origin` 与 `Host` 一致，避免 CSRF。
- `/admin/accounts/export` 导出明文凭证，仅用于本人迁移，不要上传或分享。
- 建议把控制台放在内网或反向代理鉴权之后，并设置 `RELAY_SECURE_COOKIE=true`。
