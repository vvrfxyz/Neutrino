# Neutrino 管理后台操作手册（基于当前代码）

本文档描述当前后台页面与常见运维流程。

## 1. 登录与认证

- 登录页：`/login`
- 默认首页：`/users`
- 默认认证方式：管理员 session 登录
- 可选 fallback：`ALLOW_BASIC_AUTH=true` 且管理员账号不是默认值时，可用于 curl / 自动化脚本

登录后侧边栏页面：

- `/users`
- `/nodes`
- `/traffic`
- `/enforcements`
- `/ops`

## 2. `/users`：用户列表与创建

### 2.1 创建用户

在 `/users` 页面顶部可创建用户，当前表单会写入：

- 用户名
- 月流量上限
- 计量模式（`single` / `double`）
- 配额周期（`day|week|month`）
- 配额时区
- IP / 设备上限
- 套餐有效天数

创建后系统会：

- 立即写库
- 触发用户同步请求（`users_sync`）
- 为该用户准备订阅 token（HTTP API 创建用户时会直接返回）

### 2.2 用户列表显示内容

列表页当前会展示：

- 基础用户信息
- 当前状态
- 在线会话 / 不同 IP 统计
- 当前链接复制入口
- 快速启用 / 禁用 / 删除操作

### 2.3 常见操作说明

- `新链接`：为用户生成新的活动链接
- `禁用`：用户状态切到 `disabled`
- `启用`：恢复到 `active`
- `删除`：删除用户及相关数据

说明：

- 手动禁用 / 删除后，系统会额外请求一次节点侧同步；对于 managed xray 节点，还会触发更快的节点刷新，减少“后台已停用、现场仍短暂可用”的窗口。

## 3. `/users/{id}`：用户详情页

详情页目前包含以下区块。

## 3.1 配额概览

展示：

- 月流量上限
- 计量模式
- 配额周期 / 时区
- IP 上限
- 当前窗口入站 / 出站
- 当前窗口有效计量 / 补偿
- 总入站 / 总出站
- 到期时间

## 3.2 订阅 URL

详情页会显示该用户当前订阅地址。

支持 target：

- `clash`
- `singbox`
- `v2rayn`
- `shadowrocket`

如果订阅不可用，优先排查：

- 用户是否有 active link
- 是否存在启用中的节点
- 如果没有节点，panel fallback 的 `PROXY_PUBLIC_HOST / REALITY_PUBLIC_KEY / REALITY_SHORT_ID` 是否配置完整

## 3.3 Telegram 绑定

页面会显示：

- 当前绑定状态
- 未绑定时的 `/bind <code>` 绑定命令

用户在 Telegram 里发送：

```text
/bind <code>
```

成功后，详情页会显示已绑定的 `chat_id` / `@username`。

说明：

- `/me`、`/usage`、`/sub` 等用户自助命令必须基于已验证的 chat 绑定；
- 当前实现不会再按 Telegram username 自动推断面板用户，避免越权读取别人的订阅或配额信息。

## 3.4 管理操作

详情页当前可直接执行：

- 更新 IP / 设备上限
- 重置当前配额窗口
- 补偿流量
- 延长到期时间

这些操作会记录审计日志，并影响后续配额判断。

## 3.5 流量图

详情页流量图是**异步加载**的，数据源：

- `GET /api/v1/users/{id}/traffic?period=hourly|daily|monthly`

支持：

- 小时 / 日 / 月视角切换
- loading / error / retry 状态
- 请求竞态保护

## 3.6 在线会话

会显示：

- 最近 `ONLINE_DISPLAY_WINDOW_SEC` 秒内活跃会话数
- 活跃不同 IP 数
- 每条在线会话的：
  - client IP
  - node id
  - 最后活跃时间

## 3.7 访问事件

详情页底部会显示最近访问事件（当前页面展示最近 120 条），字段包括：

- 时间
- 方向
- 目标（destination / host / ip）
- SNI
- inbound tag
- client IP

## 4. `/nodes`：节点列表

节点卡片目前会展示：

- 节点名称
- core type / protocol
- host:port
- 是否 enabled
- 是否 managed
- 最近心跳
- desired / applied users version
- desired / applied xray version
- 最近错误

可直接执行：

- 添加节点
- 启用 / 禁用节点
- 删除节点
- 跳转到 `/nodes/{id}/deploy`
- 跳转到证书 / Jobs 区块

## 5. `/nodes/{id}/deploy`：节点部署页

这是当前节点运维的核心页面。

## 5.1 页面包含的能力

- 查看节点当前 desired/applied version
- 生成 / 轮换 Enroll Code
- 查看 panel 公网 URL 与 panel mTLS URL
- 查看推荐部署目录与默认镜像
- 复制节点一键部署脚本
- 编辑 managed xray 的 `extra_json`
- 手动触发 `Deploy Xray`
- 手动触发 `Rollback`
- 查看 / 吊销节点证书 pin
- 查看节点 Job 历史

## 5.2 一键部署脚本

页面内脚本适用于首次接入节点。它会：

- 检查本机是否有 `docker compose`
- 必要时自动安装 compose 插件
- 写入部署目录、env 与 compose 文件
- 启动 `agent + xray`
- 使用一次性 Enroll Code 换取长期 mTLS 证书

如果页面提示配置错误，通常是：

- `SUB_BASE_URL` 不是可用公网地址
- `PANEL_AGENT_MTLS_ADDR` 不合理
- 缺少 `NODE_DEFAULT_PANEL_MTLS_URL`，导致从 UI 域名错误推导出 mTLS 地址

## 5.3 托管 Xray 配置

当前页面支持直接编辑 `node.extra_json` 下的：

- `rollback_on_fail`
- `xray.vars` 键值对

注意：

- `extra_json` 中不应写入 REALITY 私钥
- 真正执行 reload / test 的 argv 固定来自节点本地 `.env`

## 5.4 证书吊销

页面支持：

- 吊销单张节点证书
- 吊销当前节点全部证书

效果：

- 被吊销证书后续请求会立即被 panel 拒绝（403）

## 5.5 Job 历史

可查看：

- `users_sync`
- `xray_apply`
- `xray_rollback`

以及：

- status
- attempts
- started / finished 时间
- last error

## 6. `/traffic`：流量分析

当前页面支持：

- 时间范围切换：`1h / 24h / 7d / 30d`
- 按用户筛选
- 按节点筛选
- KPI：
  - 入站流量
  - 出站流量
  - 事件数
  - 活跃用户数
- 趋势图
- Top 用户
- Top 目标
- Top SNI

接口来源：

- `GET /api/v1/traffic/summary`

## 7. `/enforcements`：执行记录

该页展示最近执行记录，主要来源于：

- 到期自动禁用
- 超量自动禁用
- IP 超限自动禁用
- `over_limit` 在下一个 `quota_cycle` 新窗口自动恢复
- 相关 detail / reason 记录

适合排查：

- 为什么某用户突然变成 `expired`
- 为什么用户变成 `over_limit`
- 为什么用户变成 `over_ip_limit`

## 8. `/ops`：运维监控

页面每 5 秒自动刷新，当前包含：

### 8.1 主机 KPI

- CPU
- 内存
- 入站带宽
- 出站带宽
- 面板本月流量（月时区由 `PANEL_TZ` 决定，默认 `UTC`）

### 8.2 在线用户

显示最近在线连接：

- 用户名
- client IP
- first seen
- last seen

### 8.3 节点状态

节点现在按“**每节点一张卡片**”展示：

- 卡片头部：节点名、ID、health、异常提示
- 左列：控制面信息（最近心跳 / 上报状态 / jobs / users & xray version 对齐）
- 右列：运行态信息（CPU、内存、磁盘、带宽、队列、goroutines）
- 抽屉：历史趋势、静态信息、最近 probe 结果和手动 probe job
- 底部：如有 `last_error`，以错误横幅显示

每个节点会显示：

- health（online / stale / disabled / unknown）
- 最近心跳
  - 语义：panel 实际收到节点心跳的时间
  - 用途：health 判断
- 最近上报
  - 语义：agent 本地 probe 采样并上报 runtime 的时间
- pending jobs
- running job
- users version 对齐情况
- xray version 对齐情况
- agent runtime 指标：
  - cpu_percent
  - memory_bytes
  - inbound / outbound bps
  - disk 使用率
  - uptime
  - queue bytes / batches
  - goroutines
- 节点自然月累计：
  - `RX` / `TX` 分开显示
  - 按节点本地时区滚动自然月
  - 这是节点主机 OS 级网络累计，不是用户配额汇总
  - node-agent 上报宿主机 raw counter，panel 侧负责月基线和累计
- last error

### 8.4 Probe

ops-v2 节点抽屉支持查看最近 probe 结果，并手动创建安全 probe job：

- `probe_dns`：DNS/解析可达性检查。
- `probe_tcp`：TCP 端口连通性检查。
- `probe_http`：HTTP/HTTPS GET 或 HEAD 检查。

示例 payload：

```json
{ "kind": "probe_dns", "target": "example.com", "timeout_ms": 3000 }
```

```json
{ "kind": "probe_tcp", "target": "example.com", "port": 443, "timeout_ms": 3000 }
```

```json
{ "kind": "probe_http", "url": "https://example.com/healthz", "method": "GET", "timeout_ms": 3000, "expect_status": [200] }
```

默认拒绝 localhost、link-local、cloud metadata IP 和私网地址；确需探测私网目标时加 `"allow_private": true`。`probe_ping` 仍作为 legacy alias 接受，但语义已归一为 `probe_dns`。

### 8.5 Runtime 历史队列

节点 report 的 latest 状态同步写入 `node_runtime_metrics` / `node_monthly_usage`。历史 sample/detail 通过 panel 内存队列异步批量写入；内存队列满时会写入 panel 本地磁盘兜底队列，worker 后续成功落库后删除文件。

相关环境变量：

- `NODE_METRIC_HISTORY_QUEUE_CAPACITY`：默认 `4096`。
- `NODE_METRIC_HISTORY_QUEUE_DIR`：默认位于 `DB_PATH` 同目录的 `node_metric_history_queue`。
- `NODE_METRIC_HISTORY_QUEUE_MAX_BYTES`：默认 `67108864`。

磁盘兜底也满或不可写时，历史样本会被丢弃，并由 worker 写入 `metric_history_dropped` ops alert；这不会影响节点 heartbeat/report。

排障提示：

- 如果“最近心跳”看起来比真实世界时间超前，优先检查 panel 主机系统时间，而不是先怀疑页面格式化。
- 如果“最近上报”或“自然月累计最近上报”异常，优先检查对应节点主机系统时间。

## 9. Telegram

## 9.1 需要的环境变量

panel `.env` 中配置：

```env
TELEGRAM_BOT_TOKEN=<bot token>
TELEGRAM_ADMIN_CHAT_IDS=<chat_id1,chat_id2>
```

修改后重启 panel 容器：

```bash
# panel-only
cd /root/neutrino && docker compose -f docker-compose.panel-only.yml restart neutrino || true

# /data/docker-compose.yml 托管
cd /data && docker compose -f docker-compose.yml restart neutrino-panel || true
```

## 9.2 获取管理员 chat_id

1. 给 bot 发 `/start`
2. 执行：

```bash
curl -s "https://api.telegram.org/bot<TELEGRAM_BOT_TOKEN>/getUpdates" | jq
```

3. 找到 `message.chat.id`

## 9.3 用户命令

- `/bind <code>`
- `/me`
- `/usage`
- `/sub`

> 这些自助命令要求当前 chat 已先成功完成 `/bind <code>`。

## 9.4 管理员命令

- `/summary`
- `/user <name>`
- `/enable <name>`
- `/disable <name>`
- `/quota_reset <name>`

## 9.5 轮询模式说明

当前实现使用 `getUpdates` 轮询，不使用 webhook。

如果之前给该 bot 配过 webhook，需要先清掉：

```bash
curl -s "https://api.telegram.org/bot<TELEGRAM_BOT_TOKEN>/deleteWebhook"
```

## 10. 常见排查

### 10.1 浏览器复制链接失败

浏览器剪贴板 API 通常要求 HTTPS 安全上下文。

- 如果你通过 `http://IP:8080` 访问后台，复制可能失败
- 建议通过 HTTPS 域名访问后台
- 页面已经提供降级复制逻辑，但最稳妥仍是 HTTPS

### 10.2 节点不在线 / Jobs 不推进

优先检查：

- 节点是否 enabled
- `PANEL_MTLS_URL` 是否能直连 panel `:8443`
- 证书是否已吊销
- `/nodes/{id}/deploy` 页面的 last error 和 job history
- `/ops` 页面里最近心跳、最近上报、queue / runtime / 自然月累计是否正常刷新

### 10.3 Managed Xray 发布失败

重点检查：

- 节点 `.env` 中 `XRAY_CONFIG_PATH`
- `XRAY_RELOAD_ARGS_JSON`
- 是否挂载 `/var/run/docker.sock`
- `extra_json` 是否少了必要变量

### 10.4 订阅不可用

常见错误与含义：

- `subscription unavailable: no active link`
  - 用户还没有活动链接
- `subscription unavailable: no enabled nodes`
  - 没有启用中的节点，且 panel fallback 代理参数也不可用

### 10.5 怀疑节点流量重复 / 丢失

优先参考：

- `docs/USAGE_PIPELINE_DESIGN.md`
- `docs/POSTMORTEM_2026-03-28_NODE14_USAGE_DUPLICATION.md`

重点检查：

- queue depth / queue bytes
- 节点 `state.json` 里的 `acked_stats`
- panel 侧相同 `source_event_id` 的入库情况

### 10.6 节点自然月流量不更新 / 看起来不对

先分清楚你在看的是什么：

- `/ops` 顶部 KPI 的“面板本月流量”是 panel 主机自己的月累计
- 节点卡片里的“自然月累计”是节点主机自己的月累计
- 用户流量 / 配额请看 `/traffic`、`/users/{id}`、`quota_windows`

如果节点自然月累计没有变化，优先检查：

- 节点 agent 是否持续 heartbeat
- 节点卡片里的“最近心跳”是否更新
- 节点卡片里的“最近上报”是否更新
- `node_monthly_usage` 里的 `counter_source` 是否来自宿主机计数源
- 节点时区配置（`AGENT_MONTH_TZ` / `TZ`）是否符合预期

如果节点自然月累计小于你预期，但没有回退：

- 先确认是否发生过 panel DB 月累计状态丢失或计数源迁移
- 当前实现会在计数源变化 / raw counter 重置时保留已累计值并重设基线，但无法回填迁移前缺失的宿主机流量历史

### 10.7 `/ops` 时间看起来超前 / 落后真实时间

先分清语义：

- “最近心跳”来自 panel 主机收到心跳时的系统时间
- “最近上报”来自节点 agent 上报 probe 时的系统时间
- “自然月累计最近上报”同样来自节点 agent 的上报时间

因此如果 `/ops` 里时间超前，通常不是前端时区格式化问题，而是某台主机系统时钟漂移：

- “最近心跳”异常：优先检查 panel 主机
- “最近上报”或“自然月累计最近上报”异常：优先检查对应节点主机

建议基线：

- panel 与 node 主机都保持 `System clock synchronized: yes`
- 生产环境优先使用 `chrony`
- 修复后同时核对主机时间、容器时间、`/ops` 页面和 `node_monthly_usage` / `node_runtime_metrics` 的最新时间戳

常用命令：

```bash
timedatectl status
chronyc tracking
chronyc sources -v
date -u
docker exec neutrino-panel date -u
docker exec neutrino-agent date -u
```

## 11. 常用 API（给运维 / 自动化）

如果你需要用 curl 自动化，推荐先确保：

- `ALLOW_BASIC_AUTH=true`
- 管理员账号不是默认值

常用接口：

```bash
# 用户列表
curl -u "$ADMIN_USER:$ADMIN_PASS" http://127.0.0.1:8080/api/v1/users

# 节点列表
curl -u "$ADMIN_USER:$ADMIN_PASS" http://127.0.0.1:8080/api/v1/nodes

# 节点 jobs
curl -u "$ADMIN_USER:$ADMIN_PASS" http://127.0.0.1:8080/api/v1/nodes/<NODE_ID>/jobs

# 流量总览
curl -u "$ADMIN_USER:$ADMIN_PASS" "http://127.0.0.1:8080/api/v1/traffic/summary?range=24h"

# 主机指标
curl -u "$ADMIN_USER:$ADMIN_PASS" "http://127.0.0.1:8080/api/v1/metrics/host?range=1h"
```
