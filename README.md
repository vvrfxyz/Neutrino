# Neutrino

Neutrino 是一个基于 **Go + SQLite + SSR Templates + HTMX/Alpine.js** 的代理控制面，默认运行模式为：

- **panel-only**：集中式管理面板
- **node-only**：每台代理节点运行 `node-agent + xray`
- **Panel <-> node-agent**：仅通过 **mTLS** 通信，采用 pull model
- **主协议**：`VLESS + REALITY + xtls-rprx-vision`

> `Xray-core/` 目录仅作参考源码，不参与生产运行。

## 当前功能面

### 管理后台页面

- `/login` / `/logout`：管理员会话登录
- `/users`：创建用户、查看在线统计、生成链接、启用/禁用/删除
- `/users/{id}`：用户详情、订阅 URL、Telegram 绑定码、配额操作、异步流量图、在线会话、访问事件
- `/nodes`：节点列表、启停、删除、版本对齐状态
- `/nodes/{id}/deploy`：Enroll Code、节点一键部署脚本、托管 Xray 配置、证书吊销、Job 历史
- `/traffic`：按时间范围 / 用户 / 节点查看流量趋势、Top 用户、Top 目标、Top SNI
- `/enforcements`：过期、超限、IP 超限等执行记录
- `/ops`：主机指标、在线用户、节点运行态、队列状态、节点自然月 RX/TX 累计
- `/ops-v2`：可选预览版运维前端（`ENABLE_OPS_V2=true` 且已构建 `frontend/ops-demo/dist` 时启用）

### 用户与配额

- 创建用户、生成/轮换代理链接
- 启用 / 禁用 / 删除用户
- 配额窗口按 `day|week|month` + `quota_tz` 滚动
- 支持 `single` / `double` 计量模式
- 支持手动：
  - 重置当前配额窗口
  - 补偿流量
  - 延长到期时间
  - 修改 IP / 设备上限
- 自动执行：
  - 到期自动禁用（`expired`）
  - 超量自动禁用（`over_limit`）
  - IP 超限连续命中后自动禁用（`over_ip_limit`）
  - `over_limit` 用户会在下一个 `quota_cycle` 新窗口（`day|week|month`）自动恢复；`expired` / `over_ip_limit` 不会靠窗口滚动自动恢复

### 节点、agent 与托管 Xray

- 多节点 `node-agent` 接入，panel 不直连节点 Xray gRPC
- 节点首次通过一次性 `ENROLL_CODE` 自动换取 mTLS 证书
- 节点证书支持 allowlist / pin 校验与单节点吊销
- durable job 模型：
  - `users_sync`
  - `xray_apply`
  - `xray_rollback`
- 节点 runtime report 同时包含：
  - panel 接收的 heartbeat 时刻（用于 online / stale 判断）
  - agent 本地 probe 采样时刻
  - 节点本地自然月 `RX/TX` 累计
- `/ops` 使用 latest cache + WebSocket 快照；节点 report、job、metadata 变更会按单节点刷新 cache。
- runtime 历史样本异步批量写入；内存队列满时先落 panel 本地磁盘队列，磁盘也不可用时才丢历史样本并写 ops alert。
- 安全 probe job 支持 `probe_dns` / `probe_tcp` / `probe_http`，`probe_ping` 仅保留为 legacy alias。
- 托管 Xray 仅下发模板与变量；真正执行的 reload / test argv 固定在节点本地环境变量中
- 节点部署页可直接生成一键脚本；脚本会优先检查并自动安装 `docker compose`

### 流量、订阅与通知

- 统一用量入口：`POST /api/v1/usage`
- 幂等键：`source + source_event_id`
- 节点上报：Xray stats delta + access log 事件
- 订阅：`/sub/{token}?target=clash|singbox|v2rayn|shadowrocket`
- Telegram：
  - 用户命令：`/bind <code>`、`/me`、`/usage`、`/sub`
  - 管理员命令：`/summary`、`/user <name>`、`/enable <name>`、`/disable <name>`、`/quota_reset <name>`
  - 用户自助命令必须先完成 `/bind <code>`；当前实现不再按 Telegram username 猜测本地用户

## 本地运行

### Panel

```bash
go run ./cmd/server
```

默认管理员（未设置环境变量时）：

- 用户名：`admin`
- 密码：`admin123`

后台入口：

- [http://127.0.0.1:8080/login](http://127.0.0.1:8080/login)

> 只有在设置了 `PANEL_AGENT_MTLS_*` 相关变量后，panel 才会额外开启 agent mTLS listener（默认地址通常是 `:8443`）。

### Node-agent

```bash
go run ./cmd/node-agent
```

## 认证方式

### 管理员认证

- 默认：管理员 session cookie（Web 登录）
- 可选：`ALLOW_BASIC_AUTH=true` 且管理员账号不是默认值时，可用 Basic Auth 作为 curl / 自动化 fallback

### `/api/v1/*` 机器接口

当前代码支持以下认证路径：

- 管理员 session / Basic Auth
- 预先写入数据库的 API Key（请求头默认为 `X-API-Key`）
- node-agent 专用 mTLS（仅用于 agent listener）

其中 `POST /api/v1/usage` 只接受 node-agent mTLS，不接受管理员 session、Basic Auth 或 API Key。

当前 scope 映射见 `internal/app/auth.go`，主要包括：

- `users:read` / `users:write`
- `nodes:read` / `nodes:write` / `nodes:report`
- `usage:write`（node-agent mTLS 专用）
- `traffic:read`
- `online:read`
- `metrics:read`
- `backups:read` / `backups:write`
- `admin` / `*`

其中：

- `GET /api/v1/ops/nodes` 归到 `nodes:read`
- `GET /api/v1/metrics/host` 归到 `metrics:read`

> 当前仓库里**没有**暴露 API Key 生命周期的 UI 或 HTTP 管理接口；HTTP 层只实现了验证与 scope 校验。

## 关键 HTTP 接口

### 面板 / 管理接口

- `GET /healthz`
- `GET|POST /api/v1/users`
- `GET|PATCH|DELETE /api/v1/users/{id}`
- `GET /api/v1/users/{id}/traffic?period=hourly|daily|monthly`
- `GET /api/v1/users/{id}/events?limit=100&source=xray-access&node_id=1`
- `POST /api/v1/users/{id}/quota/reset`
- `POST /api/v1/users/{id}/quota/credit`
- `POST /api/v1/users/{id}/plan/extend`
- `GET /api/v1/users/{id}/subscription`
- `POST /api/v1/users/{id}/subscription/rotate`
- `GET /api/v1/users/{id}/telegram-bind`
- `POST /api/v1/users/{id}/telegram-bind/rotate`
- `GET|POST /api/v1/nodes`
- `GET|PUT|PATCH|DELETE /api/v1/nodes/{id}`
- `POST /api/v1/nodes/{id}/enable`
- `POST /api/v1/nodes/{id}/disable`
- `POST /api/v1/nodes/{id}/managed/xray/deploy`
- `POST /api/v1/nodes/{id}/managed/xray/rollback`
- `GET /api/v1/nodes/{id}/jobs`
- `POST /api/v1/nodes/{id}/cert/revoke`
- `GET /api/v1/nodes/{id}/metrics?range=1h&step=raw|1m|5m|1h`
- `GET|PUT|POST /api/v1/nodes/{id}/metadata`
- `GET /api/v1/nodes/{id}/probe-results`
- `POST /api/v1/nodes/{id}/probes`
- `GET /api/v1/traffic/summary?range=1h|24h|7d|30d[&user_id=...][&node_id=...]`
- `GET /api/v1/online-users`
- `GET /api/v1/metrics/host?range=1h|6h|24h`
- `GET /api/v1/ops/nodes`
- `GET|POST /api/v1/backups`
- `GET|DELETE /api/v1/backups/{id}`
- `GET /api/v1/backups/{id}/download`
- `POST /api/v1/backups/restore`（multipart：`backup` 字段，`.sqlite` 或 `.sqlite.gz`；通过校验后 stage 为 `<DBPath>.pending-restore`，需重启面板生效）

### 订阅接口

- `GET /sub/{token}?target=clash|singbox|v2rayn|shadowrocket`
  - `target` 省略时按客户端 `User-Agent` 自动识别（Clash/Stash → `clash`，sing-box/SFA/SFI → `singbox`，Shadowrocket/Quantumult/Surge/Loon → `shadowrocket`，其他 → `v2rayn`）。
  - 响应 header：
    - `Subscription-Userinfo: upload=<bytes>; download=<bytes>; total=<bytes>; expire=<unix>` — 上下行用量、配额上限（含信用额度）、到期时间，便于客户端订阅面板直接展示。
    - `Profile-Update-Interval: 24` — 推荐刷新间隔（小时）。

### agent mTLS listener（专用控制面）

这些路由由 panel 的独立 mTLS listener 提供，供 node-agent 使用：

- `POST /api/v1/usage`
- `POST /api/v1/nodes/{id}/report`
  - heartbeat / liveness
  - runtime metrics
  - 节点本地自然月 `RX/TX` 累计 telemetry
- `POST /api/v1/nodes/{id}/jobs/claim?wait=25`
- `POST /api/v1/nodes/{id}/jobs/{job_id}/finish`（body 必须带 claim 返回的 `attempt`）
- `POST /api/v1/nodes/{id}/cert/renew`

### Probe job 示例

```json
{ "kind": "probe_dns", "target": "example.com", "timeout_ms": 3000 }
```

```json
{ "kind": "probe_tcp", "target": "example.com", "port": 443, "timeout_ms": 3000 }
```

```json
{ "kind": "probe_http", "url": "https://example.com/healthz", "method": "GET", "timeout_ms": 3000, "expect_status": [200] }
```

默认拒绝 localhost、link-local、cloud metadata IP 和私网地址；确需探测私网目标时显式加 `"allow_private": true`。

### Ops 监控队列配置

- `NODE_METRIC_HISTORY_QUEUE_CAPACITY`：历史 sample/detail 内存队列容量，默认 `4096`。
- `NODE_METRIC_HISTORY_QUEUE_DIR`：磁盘兜底目录；默认跟随 `DB_PATH` 所在目录下的 `node_metric_history_queue`。
- `NODE_METRIC_HISTORY_QUEUE_MAX_BYTES`：磁盘兜底上限，默认 `67108864`。

runtime latest 写入仍是同步关键路径；历史 sample/detail 写入失败、队列满或磁盘兜底满不会影响 heartbeat/report 成功。

### 用量写入示例

```json
{
  "events": [
    {
      "user_id": 1,
      "node_id": 1,
      "direction": "outbound",
      "bytes": 12345,
      "source": "xray-stats",
      "source_event_id": "evt-001",
      "target_host": "example.com",
      "target_ip": "93.184.216.34",
      "target_port": 443,
      "sni": "example.com",
      "destination": "tcp:example.com:443",
      "client_ip": "10.0.0.2",
      "inbound_tag": "vless-reality",
      "at": "2026-02-07T00:00:00Z"
    }
  ]
}
```

## 测试

```bash
go test ./...
```

测试结构：

- 单元测试：与包同目录放置
- 功能测试：`tests/functional/`
  - 用户生命周期
  - 用户详情流量接口
  - 订阅 / Telegram 绑定
  - 节点 CRUD / deploy 页
  - mTLS enroll / renew / jobs 流程

## Compose / 容器拓扑

已提供：

- `docker-compose.panel-only.yml`：仅 panel
- `docker-compose.node-only.yml`：仅 `agent + xray`
- `docker-compose.yml`：all-in-one / stack（开发或单机验证）
- `docker-compose.hostnet.yml`：stack 的 host network override
- `docker-compose.panel-hostnet.yml`：panel-only host network override
- `docker-compose.node-hostnet.yml`：node-only host network override
- `Dockerfile`：panel 镜像
- `docker/node-agent/Dockerfile`：node-agent 镜像

Pinned 组件：

- node 侧 Xray 镜像：`ghcr.io/xtls/xray-core:26.2.6`

典型职责：

- `neutrino`：panel
- `agent`：node-agent
- `xray`：节点代理核心

## 环境文件

```bash
cp .env.example .env         # panel-only / panel-stack
cp .env.node.example .env    # node-only
```

注意：

- `.env.example` 与 `.env.node.example` 都包含 `REPLACE_WITH_*` 占位符，生产必须替换
- `.env.node.example` 中如果 `XRAY_REALITY_PRIVATE_KEY` 仍是占位符，node-agent 会在节点本机生成并持久化到 `reality.json`
- node-only 默认启用 host network：`xray` 直接监听宿主代理端口以保留真实客户端 IP，Xray API 与 agent health 只绑定 `127.0.0.1`
- `scripts/release/deploy_panel_remote.sh` 与 `scripts/release/deploy_node_remote.sh` 会拒绝明显的占位符配置

## 发布与部署（强制策略）

生产发布必须遵守：

- Panel/API 镜像由 GitHub Actions `Docker Image` workflow 构建。
- PR 只 build 镜像，不 push。
- 非 PR 会 push 多架构镜像到 `ghcr.io/<owner>/neutrino-panel` 和 `ghcr.io/<owner>/neutrino-node`。
- 如果配置了 `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`，还会 push Docker Hub。Docker Hub 镜像名优先使用 repo variable `DOCKERHUB_PANEL_IMAGE` / `DOCKERHUB_NODE_IMAGE`；`DOCKERHUB_IMAGE` 仍作为 panel 的兼容覆盖；默认分别为 `<DOCKERHUB_USERNAME>/neutrino-panel` 和 `<DOCKERHUB_USERNAME>/neutrino-node`。
- 默认分支会打 `latest`，分支/tag/sha 都会生成对应镜像 tag。
- 远程服务器只允许 `pull + up`，不允许直接构建 release 镜像。

常用部署脚本：

```bash
# 使用 GitHub Actions 已发布的 panel/API tag
scripts/release/deploy_panel_remote.sh <TAG>
scripts/release/deploy_stack_remote.sh <TAG>

# 使用 GitHub Actions 已发布的 node-agent tag
scripts/release/deploy_node_remote.sh <TAG>

# 本地 fallback：构建并推送 panel/API 或 node-agent 镜像
scripts/release/push_panel.sh <TAG>
scripts/release/push_agent.sh <TAG>
```

本地发布脚本会在 registry 操作前自动取消代理环境变量，避免推送流程被本地代理污染。

## 分支保护

`main` 分支保护规则：

- 必须通过状态检查：`build`、`docker`
- 合入前必须与最新 `main` 同步（strict）
- 要求线性历史
- 要求 conversation resolved
- 禁止 force push 和删除分支
- PR review 规则开启，批准数为 `0`
- 管理员不强制执行：`enforce_admins=false`

## 文档索引

- `AGENTS.md`：项目硬约束、运行决策、部署政策
- `docs/DEPLOYMENT_OPERATION_MANUAL.md`：面板 / 节点部署与发布手册
- `docs/OPERATION_MANUAL.md`：后台日常操作手册
- `docs/USAGE_PIPELINE_DESIGN.md`：node-agent usage pipeline 设计约束
- `docs/NODE_MONTHLY_USAGE_DESIGN.md`：节点自然月 RX/TX 探针设计
- `docs/OPS_MONITORING_DESIGN.md`：`/ops` 实时监控、历史指标、probe 与告警设计
- `docs/XBOARD_LEARNINGS_UPGRADE_MODULES.md`：向 Xboard / Xboard-Node 学习后的升级模块拆分
- `docs/ONLINE_STATUS_MODULE_PLAN.md`：在线状态模块的节点快照改造方案
- `docs/POSTMORTEM_2026-03-28_NODE14_USAGE_DUPLICATION.md`：node 14 用量重复事故复盘

## 当前实现边界

- 当前仓库里**没有**暴露 API Key 生命周期的 UI 或 HTTP 管理接口；HTTP 层只实现了验证与 scope 校验。
- `/ops-v2` 是可选预览入口，默认关闭；稳定运维入口仍是 `/ops`。
