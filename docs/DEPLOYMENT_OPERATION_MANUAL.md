# Neutrino 部署操作手册（基于当前代码）

本文档以当前仓库实现为准，覆盖：

- panel-only（集中式控制面）
- node-only（每台节点运行 `agent + xray`）
- GitHub Actions 构建发布 + 远端 pull 的发布流程
- mTLS 证书、Enroll Code、节点部署页、托管 Xray

> 说明：SQLite backup / restore HTTP 接口已存在，但 restore 会 stage
> `<DB_PATH>.pending-restore`，必须重启 panel 后才会应用。

## 1. 约束与部署原则

强制原则：

- panel 不直连节点 Xray gRPC
- node-agent 只通过 panel 的 **mTLS listener** 与 panel 交互
- Panel <-> node-agent 不使用 bearer token
- Panel/API 发布流程必须是：
  1. GitHub Actions `Docker Image` workflow 构建镜像
  2. PR 只 build 不 push；非 PR push 多架构 `linux/amd64,linux/arm64` 镜像到 `ghcr.io/<owner>/cli-proxy-api`
  3. 如配置 `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`，同时 push Docker Hub；`DOCKERHUB_IMAGE` repo variable 可覆盖 Docker Hub 镜像名
  4. 远端服务器只做 `docker compose pull && up -d --no-build`
- 节点托管操作只允许 agent 执行本地预配置 argv，panel 不下发 shell 命令和任意路径

关键默认值：

- panel HTTP：`:8080`
- panel agent mTLS listener：`:8443`
- node 默认部署目录：`/root/neutrino-node`
- node Xray 镜像：`ghcr.io/xtls/xray-core:26.2.6`

当前生产信息（文档记录用途）：

- panel server：`<panel-host>`（`root@<panel-ip>`）
- stack 文件：`/data/docker-compose.yml`
- panel 目录：`/data/neutrino`

## 2. 上线前检查清单

### 2.1 Panel `.env`

至少确认以下配置已经替换掉占位符：

- `ADMIN_PASS`
- `SUB_BASE_URL`
- `PROXY_PUBLIC_HOST`
- `REALITY_PUBLIC_KEY`
- `REALITY_SHORT_ID`
- `PANEL_AGENT_MTLS_ADDR`
- `PANEL_AGENT_MTLS_CA_CERT_PATHS`
- `PANEL_AGENT_MTLS_SERVER_CERT_PATH`
- `PANEL_AGENT_MTLS_SERVER_KEY_PATH`
- `PANEL_AGENT_MTLS_SIGNING_CA_CERT_PATH`
- `PANEL_AGENT_MTLS_SIGNING_CA_KEY_PATH`

建议：

- 默认保持 `ALLOW_BASIC_AUTH=false`
- 如需调整 ops runtime 历史队列容量，配置 `NODE_METRIC_HISTORY_QUEUE_CAPACITY`、`NODE_METRIC_HISTORY_QUEUE_DIR`、`NODE_METRIC_HISTORY_QUEUE_MAX_BYTES`；默认会在 `DB_PATH` 同目录建立 `node_metric_history_queue` 磁盘兜底目录。
- 如果后台域名走 CDN / 反代，而 `:8443` 需要节点直连源站，设置：
  - `NODE_DEFAULT_PANEL_MTLS_URL=https://mtls.example.com:8443`

### 2.2 Node `.env`

每台节点至少确认：

- `NODE_ID`
- `PANEL_URL`
- `PANEL_MTLS_URL`
- `ENROLL_CODE`
- `XRAY_REALITY_SHORT_ID`
- 如需自带密钥：`XRAY_REALITY_PRIVATE_KEY`

说明：

- 如果 `XRAY_REALITY_PRIVATE_KEY` 保持占位符，node-agent 会在本机生成并持久化到 `reality.json`
- 节点证书文件由首次 enroll 自动生成，无需预拷贝

### 2.3 主机时间同步基线

panel 主机和每台 node 主机都必须保持系统时钟准确，否则会直接影响：

- `/ops` 页面里的“最近心跳”
- 节点 runtime 的“最近上报”
- 节点自然月累计的“最近上报”
- 依赖事件时间的运维排障判断

生产建议：

- 优先使用 `chrony`，不要依赖未验证可用性的默认时间源
- 要求 `timedatectl status` 中 `System clock synchronized: yes`
- 要求 `chronyc tracking` 可返回正常 source 和微秒到毫秒级 offset
- 修复系统时间后，同步核对容器时间与硬件时钟（RTC）

建议在每台新机器初始化后执行：

```bash
timedatectl status
date -u
docker exec neutrino-panel date -u 2>/dev/null || true
docker exec neutrino-agent date -u 2>/dev/null || true
```

## 3. mTLS 证书准备（panel 侧）

## 3.1 本地生成 CA 与 panel server cert

在仓库根目录执行：

```bash
mkdir -p ./mtls-out
scripts/mtls/gen_ca.sh ./mtls-out
scripts/mtls/gen_panel_server.sh ./mtls-out "panel.example.com,127.0.0.1"
```

产物：

- `./mtls-out/ca.crt`
- `./mtls-out/ca.key`
- `./mtls-out/panel-agent-server.crt`
- `./mtls-out/panel-agent-server.key`

生产建议：

- `ca.key` 最好不要长期以 root CA 形式常驻面板机
- 更推荐离线 root + 在线 issuing CA / intermediate CA

## 3.2 拷贝到 panel 主机

以 panel-only 目录为例：

```bash
REMOTE_HOST=root@<panel-host>
REMOTE_DIR=/root/neutrino

ssh "$REMOTE_HOST" "mkdir -p $REMOTE_DIR/data/mtls"
scp ./mtls-out/ca.crt "$REMOTE_HOST:$REMOTE_DIR/data/mtls/"
scp ./mtls-out/panel-agent-server.crt ./mtls-out/panel-agent-server.key "$REMOTE_HOST:$REMOTE_DIR/data/mtls/"
scp ./mtls-out/ca.crt "$REMOTE_HOST:$REMOTE_DIR/data/mtls/issuing-ca.crt"
scp ./mtls-out/ca.key "$REMOTE_HOST:$REMOTE_DIR/data/mtls/issuing-ca.key"
```

对应 panel `.env`：

```env
PANEL_AGENT_MTLS_ADDR=:8443
PANEL_AGENT_MTLS_CA_CERT_PATHS=/data/mtls/ca.crt
PANEL_AGENT_MTLS_SERVER_CERT_PATH=/data/mtls/panel-agent-server.crt
PANEL_AGENT_MTLS_SERVER_KEY_PATH=/data/mtls/panel-agent-server.key
PANEL_AGENT_MTLS_SIGNING_CA_CERT_PATH=/data/mtls/issuing-ca.crt
PANEL_AGENT_MTLS_SIGNING_CA_KEY_PATH=/data/mtls/issuing-ca.key
```

## 4. Panel-only 部署

## 4.1 初始化远端目录

```bash
export REMOTE_HOST="root@<panel-host>"
export REMOTE_DIR="/root/neutrino"
scripts/release/bootstrap_remote.sh
```

脚本行为：

- 创建目录与 `data/`、`data/mtls/`
- 上传 compose / env 模板
- 如果 `.env` 不存在，则由 `.env.example` 初始化
- 如果 `.env` 已存在，只补齐缺失 key，不覆盖已有值

## 4.2 编辑 panel `.env`

重点修改：

- `ADMIN_PASS`
- `SUB_BASE_URL`
- `PROXY_PUBLIC_HOST`
- `PROXY_PUBLIC_PORT`
- `REALITY_PUBLIC_KEY`
- `REALITY_SHORT_ID`
- 所有 `PANEL_AGENT_MTLS_*`
- 如需部署页默认展示独立 mTLS 域名：`NODE_DEFAULT_PANEL_MTLS_URL`

## 4.3 构建与部署

### 发布镜像

Panel/API 镜像由 GitHub Actions `Docker Image` workflow 发布：

- PR：只 build，不 push
- 非 PR：push `ghcr.io/<owner>/cli-proxy-api:<TAG>`
- 如配置 `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN`，额外 push Docker Hub
- 默认分支会打 `latest`，分支/tag/sha 都会生成对应镜像 tag

### 部署已发布 tag

```bash
scripts/release/deploy_panel_remote.sh <TAG>
```

如果需要本地 fallback 构建并推送 panel/API 镜像：

```bash
scripts/release/push_panel.sh <TAG>
```

远端不会构建镜像，只会：

- 校验 `.env` 是否仍存在占位符
- 校验 mTLS 文件路径是否存在
- `pull` 指定 tag
- `up -d --no-build`
- 通过 `curl http://127.0.0.1:8080/healthz` 做健康检查

## 4.4 验证 panel

```bash
ssh root@<panel-host> '
  docker ps
  curl -fsS http://127.0.0.1:8080/healthz
'
```

如果走反代域名，还应验证：

- 能正常访问 `/login`
- cookie 能正常生效
- `/nodes/{id}/deploy` 页面生成的 `PANEL_MTLS_URL` 是节点可直连的地址

## 4.5 嵌入已有 `/data/docker-compose.yml`

如果面板已经托管在 `/data/docker-compose.yml`：

- 面板目录建议：`/data/neutrino`
- 面板数据目录：`/data/neutrino/data`
- 反向代理 service 名可用 `neutrino-panel`

典型 service：

```yaml
  neutrino-panel:
    image: ghcr.io/vvrfxyz/cli-proxy-api:<TAG>
    container_name: neutrino-panel
    restart: unless-stopped
    env_file:
      - /data/neutrino/.env
    environment:
      - VIRTUAL_HOST=proxy.example.com
      - LETSENCRYPT_HOST=proxy.example.com
      - VIRTUAL_PORT=8080
    ports:
      - "127.0.0.1:8080:8080"
      - "8443:8443"
    volumes:
      - /data/neutrino/data:/data
```

## 5. Node-only 部署

## 5.1 推荐方式：从 `/nodes/{id}/deploy` 页面下发

当前代码最推荐的接入方式：

1. 在 panel 创建节点（通常勾选 `managed`）
2. 打开 `/nodes/{id}/deploy`
3. 点击生成 / 轮换 Enroll Code
4. 复制页面里的“一键部署脚本”到节点执行

该脚本会：

- 检查 `docker compose` 是否可用
- 不可用时优先尝试系统包安装
- 仍不可用时自动下载安装官方 compose 插件
- 写入部署目录、compose 文件与 `.env`
- 启动 `agent + xray`
- 使用 Enroll Code 自动换取节点 mTLS 证书

## 5.2 备选方式：手动 bootstrap 远端目录

```bash
export REMOTE_HOST="root@<node-host>"
export REMOTE_DIR="/root/neutrino-node"
scripts/release/bootstrap_node_remote.sh
```

之后手动编辑远端 `.env`。

## 5.3 Node `.env` 关键项

```env
NODE_ID=1
PANEL_URL=https://panel.example.com
PANEL_MTLS_URL=https://mtls.example.com:8443
ENROLL_CODE=REAL_ONE_TIME_CODE
XRAY_VLESS_PORT=24443
HOSTNET_ENABLE=true
XRAY_API_LISTEN=127.0.0.1
XRAY_API_ADDR=127.0.0.1:10085
AGENT_HTTP_ADDR=127.0.0.1:9090
AGENT_ACCESS_LOG_TZ=UTC
XRAY_CONFIG_PATH=/usr/local/etc/xray/config.json
XRAY_RELOAD_ARGS_JSON=["docker","restart","neutrino-xray"]
```

关键说明：

- `PANEL_URL` 用于首次 enroll
- `PANEL_MTLS_URL` 用于后续长期 mTLS 控制面通信
- `XRAY_RELOAD_ARGS_JSON` / `XRAY_TEST_ARGS_JSON` 只在节点本地生效，panel 不会下发 shell 命令
- 如果使用 `docker` 作为 reload/test argv，必须挂载 `/var/run/docker.sock`

## 5.4 首次 enroll 后会生成的文件

节点数据目录下会自动出现：

```text
/root/neutrino-node/data/mtls/
  ca-bundle.crt
  node.crt
  node.key
```

另外，若 REALITY 私钥使用自动生成模式，还会在 agent state 目录旁生成 `reality.json`。

## 5.5 发布节点镜像

node-agent 仍是独立节点镜像，不由 `Docker Image` workflow 发布；节点镜像继续使用本地 `push_agent.sh` / `release_node.sh` 流程。

### 一键发布

```bash
scripts/release/release_node.sh <TAG>
```

### 分步发布

```bash
scripts/release/push_agent.sh <TAG>
scripts/release/deploy_node_remote.sh <TAG>
```

`deploy_node_remote.sh` 会：

- 校验 `PANEL_URL` / `PANEL_MTLS_URL` / `ENROLL_CODE`
- 同步 `docker-compose.node-hostnet.yml`
- 生成 `docker-compose.release.yml`
- `pull` agent / xray 镜像
- `up -d --no-build xray agent`

## 5.6 Host network 场景

节点推荐开启 `HOSTNET_ENABLE=true`。部署脚本会自动同步并叠加
`docker-compose.node-hostnet.yml`，使 `xray` 直接使用宿主网络监听代理端口，
避免 Docker bridge / userland-proxy 把真实客户端 IP 改写成 `172.x` 网关地址。

此时请确保：

```env
XRAY_API_LISTEN=127.0.0.1
XRAY_API_ADDR=127.0.0.1:10085
AGENT_HTTP_ADDR=127.0.0.1:9090
```

否则部署脚本会直接拒绝继续。不要把 Xray API 监听到 `0.0.0.0`。

## 6. 节点生命周期与托管 Xray

## 6.1 创建节点

可以在 `/nodes` 页面创建，也可通过 API：

```bash
curl -u "$ADMIN_USER:$ADMIN_PASS" \
  -H 'Content-Type: application/json' \
  -d @- http://127.0.0.1:8080/api/v1/nodes <<'JSON'
{
  "name": "node-1",
  "core_type": "xray",
  "protocol": "vless_reality",
  "host": "node.example.com",
  "port": 24443,
  "enabled": true,
  "managed": true,
  "extra_json": "{\"xray\":{\"rollback_on_fail\":true,\"vars\":{\"XRAY_INBOUND_TAG\":\"vless-reality\",\"XRAY_VLESS_PORT\":\"24443\",\"XRAY_API_PORT\":\"10085\",\"XRAY_REALITY_DEST\":\"www.microsoft.com:443\",\"XRAY_REALITY_SERVER_NAME\":\"www.microsoft.com\",\"XRAY_REALITY_SHORT_ID\":\"REPLACE_WITH_SHORT_ID\"}}}"
}
JSON
```

> 若使用 Basic Auth，必须满足：`ALLOW_BASIC_AUTH=true` 且管理员账号不是默认值。

## 6.2 Deploy / Rollback

部署托管 Xray：

```bash
curl -u "$ADMIN_USER:$ADMIN_PASS" \
  -X POST \
  http://127.0.0.1:8080/api/v1/nodes/<NODE_ID>/managed/xray/deploy
```

查看 jobs：

```bash
curl -u "$ADMIN_USER:$ADMIN_PASS" \
  http://127.0.0.1:8080/api/v1/nodes/<NODE_ID>/jobs
```

回滚：

```bash
curl -u "$ADMIN_USER:$ADMIN_PASS" \
  -H 'Content-Type: application/json' \
  -d '{"backup_name":"config.json.bak.20260207T000000Z"}' \
  http://127.0.0.1:8080/api/v1/nodes/<NODE_ID>/managed/xray/rollback
```

如果不提供 `backup_name`，agent 会按节点本机备份命名规则选择最新备份。

## 6.3 吊销节点证书

节点部署页支持：

- 吊销单张证书
- 吊销该节点全部证书

吊销后该节点后续：

- 无法继续上报 usage
- 无法继续 claim jobs
- 会收到 `403`

## 7. 发布流程总览

### GitHub Actions 镜像发布

`Docker Image` workflow 负责 panel/API 镜像：

- PR 只 build，不 push。
- 非 PR push `ghcr.io/<owner>/cli-proxy-api`。
- 如果配置 `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`，同时 push Docker Hub。
- Docker Hub 镜像名优先使用 repo variable `DOCKERHUB_IMAGE`，否则为 `<DOCKERHUB_USERNAME>/cli-proxy-api`。
- 多架构：`linux/amd64,linux/arm64`。
- 默认分支会打 `latest`，分支/tag/sha 都会生成对应镜像 tag。

### 常用部署脚本

```bash
scripts/release/bootstrap_remote.sh
scripts/release/bootstrap_node_remote.sh

# 部署 GitHub Actions 已发布的 panel/API tag
scripts/release/deploy_panel_remote.sh <TAG>
scripts/release/deploy_stack_remote.sh <TAG>

# node-agent 仍使用独立节点镜像流程
scripts/release/deploy_node_remote.sh <TAG>

# 本地 fallback：构建并推送 panel/API 镜像
scripts/release/push_panel.sh <TAG>
```

### 代理变量说明

仓库约定本地网络命令常用：

```bash
export https_proxy=http://127.0.0.1:6152
export http_proxy=http://127.0.0.1:6152
export all_proxy=socks5://127.0.0.1:6153
```

但本地发布脚本会在 registry 操作前主动 `unset` 这些代理变量，避免推送被代理环境污染。

## 8. 验证与验收

至少检查：

1. `go test ./...`
2. 面板登录成功：`/login`
3. `/users` 创建用户、生成链接、启用/禁用/删除正常
4. `/users/{id}` 的配额操作、订阅 URL、Telegram 绑定码、流量图正常
5. `/nodes/{id}/deploy` 可生成 Enroll Code 和部署脚本
6. 节点上线后：
   - `/ops` 能看到节点运行态、最近心跳、最近上报、自然月 RX/TX 累计
   - `/nodes` 的 desired/applied version 能逐步对齐
   - `/api/v1/nodes/{id}/jobs` 有 claim / finish 记录
7. 用量上报正常：
   - `/api/v1/usage` 成功入库
   - `/traffic` 和 `/users/{id}` 图表有数据
8. 如果启用 managed Xray：deploy / rollback 至少演练一次
9. 如需验证备份：创建一次 `/api/v1/backups`，下载备份文件；restore
   演练应在测试环境执行，避免误替换生产 DB。

## 9. 常见问题

### 9.1 节点部署页生成的 mTLS 地址不对

优先检查：

- `SUB_BASE_URL`
- `PANEL_AGENT_MTLS_ADDR`
- `NODE_DEFAULT_PANEL_MTLS_URL`

如果 panel 域名经过 CDN / 代理，而 `:8443` 必须直连源站，就不要让节点沿用 `SUB_BASE_URL` 推导出的地址，必须显式设置 `NODE_DEFAULT_PANEL_MTLS_URL`。

### 9.2 节点一直不能上线

排查顺序：

- panel `:8443` 是否开放入站
- 节点是否能直连 `PANEL_MTLS_URL`
- Enroll Code 是否已过期 / 已使用
- 节点是否被 disable
- 节点证书是否已被 revoke
- `/nodes/{id}/deploy` 页面的 Job 历史与 last error

### 9.3 Managed Xray deploy 失败

重点排查：

- `XRAY_CONFIG_PATH`
- `XRAY_RELOAD_ARGS_JSON`
- 是否挂载 `/var/run/docker.sock`
- `extra_json.xray.vars` 是否缺少必要变量

### 9.4 Basic Auth 的 curl 示例不能用

只有在下面两个条件同时满足时 Basic Auth 才有效：

- `ALLOW_BASIC_AUTH=true`
- 管理员账号 / 密码不再是默认值 `admin` / `admin123`
