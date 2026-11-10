# 快速部署

只需要这个目录，不需要仓库其余部分。

```sh
# 1. 只取部署目录（稀疏检出，约 5 KB）
git clone --filter=blob:none --sparse https://github.com/re-proxy/qoder-relay-web.git
cd qoder-relay-web
git sparse-checkout set deploy

# 2. 准备配置
cd deploy
cp .env.example .env
docker run --rm painwind/qoder-relay-web:latest keygen   # 把输出填进 RELAY_MASTER_KEY
# 再填好 RELAY_ADMIN_PASSWORD

# 3. 启动
docker compose up -d
```

浏览器打开 `http://主机:8317`，用 `RELAY_ADMIN_PASSWORD` 登录。

`docker-compose.yml` 拉取已发布镜像 `painwind/qoder-relay-web:latest`
（`linux/amd64` + `linux/arm64`），在本目录内不构建任何东西。已 clone 整个仓库时，
直接 `cd deploy` 再执行第 2、3 步即可。

## 必填配置

| 变量 | 说明 |
| --- | --- |
| `RELAY_ADMIN_PASSWORD` | 管理页登录密码 |
| `RELAY_MASTER_KEY` | 状态加密密钥（AES-256-GCM），`keygen` 输出，丢失即丢全部账号 |

## 常用可选变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `QODER_PORT` | `8317` | 管理页 + 通用 `/v1` 网关的宿主机端口 |
| `QODER_PORT_CN` | `7891` | 国内版反代端口，设置页开启反代后生效 |
| `QODER_PORT_INTL` | `7892` | 国际版反代端口，设置页开启反代后生效 |
| `RELAY_DEFAULT_EDITION` | `domestic` | 未指定档位时的默认值 |
| `RELAY_CHECKIN_ENABLED` | `true` | 每日自动签到开关 |
| `RELAY_CHECKIN_HOUR` / `MINUTE` | `8` / `30` | 自动签到时间（容器本地时间，默认 `Asia/Shanghai`） |
| `RELAY_SECURE_COOKIE` | `false` | 用 HTTPS 反代暴露控制台时设为 `true` |
| `QODER_IMAGE` | `painwind/qoder-relay-web:latest` | 固定到某个版本或 `sha-xxxxxxx` |
| `QODER_MACHINE_TOKEN` / `TYPE` / `CODE` | 空 | 国际版签到所需的真实桌面端机器身份，三项需同时提供 |

## 首次使用

1. 登录后到「账号」页点「登录新账号」，用 Qoder 设备码完成授权；也可导入账号备份或 PAT。
2. 点「刷新额度」确认账号可用；把需要参与反代的账号分组设为 `proxy`。
3. 到「密钥」页创建 API Key，客户端用 `Authorization: Bearer sk-...` 调用。
4. 需要固定端口给客户端时，到「设置」页开启反代，客户端直接指向
   `http://主机:7891/v1`（国内版）或 `http://主机:7892/v1`（国际版），无需额外请求头。

## 运维

```sh
docker compose logs -f          # 跟踪日志
docker compose ps               # 容器与健康状态
docker compose pull && docker compose up -d   # 升级到最新镜像
docker compose down             # 停止（数据保留在命名卷中）
```

数据全部落在命名卷 `${QODER_VOLUME:-qoder-relay-data}` 的 `/data/state.enc`（0600，属主 uid 10001）。备份这个卷即备份账号池与密钥：

```sh
docker run --rm -v qoder-relay-data:/data -v "$PWD:/backup" alpine tar czf /backup/qoder-data.tar.gz -C /data .
```

容器以非 root 用户 `relay`（uid 10001）运行。命名卷由镜像初始化，属主自动正确；
若改用宿主机目录（`-v "$PWD/data:/data"`），必须先 `chown -R 10001:10001 data`，
否则容器会因 `open /data/state.tmp: permission denied` 反复重启。

## 反向代理示例（可选）

用 Nginx 套 HTTPS 时，把 `RELAY_SECURE_COOKIE` 设为 `true`，并转发到 `127.0.0.1:8317`：

```nginx
location / {
    proxy_pass http://127.0.0.1:8317;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;          # /v1 流式响应需要关闭缓冲
}
```

## 从源码构建

不用本目录，回到仓库根目录：

```sh
docker compose up -d --build
```
