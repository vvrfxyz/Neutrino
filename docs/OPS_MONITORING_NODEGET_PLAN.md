# Ops Monitoring NodeGet-Inspired Plan

## 背景

本计划来自对 NodeGet 监控/任务设计的学习，但不复制其代码。Neutrino 的目标更窄：代理面板、节点 agent、Xray 运维与用户计费。因此这里吸收的是可落地的工程模式，而不是通用探针的全部功能。

核心原则：

- 用户计费、配额、封禁仍以现有 usage pipeline 为准。
- `/ops` 的节点流量是主机网卡运维遥测，不是 Xray-only 或用户计费流量。
- node-agent 不执行任意 shell，不接受 panel 下发任意 argv/path。
- 优先收稳权限边界、读放大、写放大，再扩展诊断能力。

## 当前结论

这 6 个方向不是都要从零建设。仓库里已经有不少基础：

- latest durable 表已有：`node_runtime_metrics`。
- 历史样本/详情已有：`node_metric_samples`、`node_metric_details`。
- 静态信息 hash 去重已有：`node_static_facts` 的 `UNIQUE(node_id, data_hash)`。
- 探测任务已有：`probe_dns`、`probe_tcp`、`probe_http`，并复用 `node_jobs` claim/finish/result 流程；`probe_ping` 仅作为 legacy alias。
- 探测结果已有：`node_probe_results`。
- ops 告警已有：`ops_alerts`。

## 当前落地状态

- Phase 0 已完成：只读 API key 不再能写 ops alert、probe、metadata。
- Phase 1 已完成：`OpsLatestCache` 已接入 warmup、WS/ops 读路径、单节点精确刷新；report/job/metadata/node 变更会刷新对应 node，删除会移除 cache 项。
- Phase 2 已完成：runtime latest 同步落库，历史 sample/detail 改为有界内存队列 + 批量 flush；内存满时落 panel 本地磁盘队列，磁盘也满或不可写时才记 drop 并由 worker 同步 `metric_history_dropped` alert。
- Phase 3 已完成：static facts canonical hash 已稳定。
- Phase 4 保持决策门：暂未新增 rollup 表，继续使用 `node_metric_samples` + bucket 聚合。
- Phase 5 已完成安全化与基础 UI：`probe_dns` 语义、target policy、HTTP redirect/方法/body 限制、按 target/correlation 去重、关键 job 优先级、ops-v2 节点抽屉手动 probe 管理均已落地。周期 probe 仍为后续扩展。

因此实施重点调整为：

1. 先修只读权限破口。
2. 再做 latest-cache 和读路径收敛。
3. 再做历史监控写入缓冲。
4. 静态 facts 只补 canonical hash。
5. summary/rollup 先压测再决定是否新增表。
6. probe 先补安全策略和 job 粒度，再产品化周期探测。

## Phase 0: 权限边界修复

目标：只读 API key 真正只读，避免 `nodes:read` 写入 ops 数据或创建节点任务。

主要文件：

- `internal/app/auth.go`
- `internal/app/auth_test.go`
- `internal/app/handlers_nodes.go`
- `internal/app/handlers_ops.go`

任务：

- 拆分 `/api/v1/ops/alerts` 权限：
  - `GET` 需要读权限。
  - `POST` 需要写权限。
- 建议新增 `ops:read` / `ops:write`；短期可用 `nodes:read` / `nodes:write` 过渡。
- 确认 `POST /api/v1/nodes/{id}/probes` 需要写权限。
- 确认 metadata 更新继续需要写权限。
- 保持 admin session 的完整权限。
- 保持 node-agent mTLS 只具备 `nodes:report` / `usage:write`。

验收：

- `nodes:read` 可以读 `/api/v1/ops/nodes` 和 `GET /api/v1/ops/alerts`。
- `nodes:read` 不能 `POST /api/v1/ops/alerts`。
- `nodes:read` 不能创建 probe job。
- `nodes:read` 不能更新 metadata。
- 现有 admin UI 不受影响。

测试：

- `requiredScope` 覆盖 ops alerts 的 GET/POST 分支。
- API key `nodes:read` 对写接口返回 403。
- admin session 对写接口仍可通过。

## Phase 1: latest-cache 与读路径收敛

目标：`/ops` 和 WebSocket 快照不再每帧重复拼多张表，降低 SQLite 读压力。

主要文件：

- `internal/service/ops.go`
- `internal/app/ws_ops_publisher.go`
- `internal/monitor/host.go`
- `frontend/ops-demo/src/adapters/liveAdapter.ts`
- `frontend/ops-demo/src/App.tsx`

任务：

- 新增 `OpsLatestCache`，缓存 `BuildNodeItems` 的结果。
- 缓存策略采用短 TTL 或事件驱动失效：
  - node report 成功后失效或更新对应 node。
  - node job claim/finish 后失效对应 node。
  - metadata 更新后失效对应 node。
- App 启动时从 DB warm up cache。
- `HostMonitor` 增加 `Latest()`，快照路径不要为了最后一个点调用 `Query("1h")`。
- `BuildNodeItems` 去掉 metadata N+1：
  - 新增批量 `ListNodeMetadata`，或
  - 将 metadata 纳入 latest cache。
- ops-v2 / frontend 确保 WS open 后停止 polling，只有断线后 fallback polling。

验收：

- WS 有订阅者时，快照构建不再每帧全量查多张表。
- 节点 report 后，下一帧能看到最新 runtime metrics。
- panel 重启后 cache 能从 DB warm up。
- WS 断线才 polling，恢复后停止 polling。

测试：

- cache warmup 后可返回节点 latest。
- report/update 后对应 node cache 更新或失效。
- metadata 不再 N+1，或 cache 命中时不重复查库。
- frontend WS open/close polling 行为测试。

## Phase 2: 监控历史写入缓冲

目标：高频 runtime report 不被历史样本写入拖慢。latest 状态仍同步落库，历史样本和 detail 有界异步写入。

主要文件：

- `internal/repo/node_report.go`
- `internal/repo/node_metrics_history.go`
- `internal/app/app.go`

任务：

- 同步写：
  - `node_runtime_metrics`
  - `node_monthly_usage`
  - 订阅渲染所需 node fields
- 异步写：
  - `node_metric_samples`
  - `node_metric_details`
- 新增 panel 侧有界内存队列和 flush worker。
- 队列满时允许丢历史 sample/detail，但不能影响 heartbeat/report。
- 增加 drop 计数日志或 ops alert。
- graceful shutdown 尽量 flush 剩余批次。
- 内存队列满时写入 panel 本地磁盘兜底队列；磁盘上限可配置，worker 成功落库后删除 backlog 文件。

验收：

- latest 写失败才让 report 失败。
- sample/detail 写失败或队列满不影响 node liveness。
- 内存队列满但磁盘队列可写时不计入 dropped。
- 磁盘队列满或不可写时计入 dropped，并由 worker 写入 ops alert。
- 高并发 report 下 SQLite 写锁等待明显下降。

测试：

- latest 写失败返回错误。
- sample/detail 队列满仍返回 report 成功。
- flush worker 批量写入成功。
- 内存满时磁盘 fallback 可被 flush 回 DB。
- 内存和磁盘都满时记录 drop alert。
- shutdown flush 覆盖剩余队列。
- detail invalid JSON 不影响 latest。

## Phase 3: 静态 facts canonical hash

目标：已有 static facts 去重更稳定。相同事实因为 JSON key 顺序或空白不同，不应产生新 hash。

主要文件：

- `internal/repo/node_metrics_history.go`
- `internal/repo/node_metrics_history_test.go`

任务：

- 保留 `node_static_facts UNIQUE(node_id, data_hash)`。
- `facts_json` 在 hash 前做 canonical JSON。
- 结构化字段和 `facts_json` 一起进入 hash。
- 内容无变化时只更新 `reported_at`。
- 内容变化时新增历史行。

验收：

- 相同 facts JSON key 顺序不同，不新增行。
- OS/kernel/xray version 变化，新增行。
- latest static facts 查询行为不变。

测试：

- canonical JSON key 顺序测试。
- 相同内容重复上报只保留一条 hash 行。
- 内容变化新增行。
- invalid facts JSON 行为明确。

## Phase 4: summary / rollup 决策门

目标：先用现有 `node_metric_samples`，压测后再决定是否新增 rollup 表。

主要文件：

- `internal/repo/node_metrics_history.go`
- `internal/db/db.go`
- `internal/app/handlers_nodes.go`

已有基础：

- `node_metric_samples` 已是扁平 summary 样本表。
- `ListNodeMetricSeries` 已支持按 bucket 聚合。
- `node_metric_details` 已承载长尾 JSON。

任务：

- 明确 metric API step：
  - `raw`
  - `1m`
  - `5m`
  - `1h`
- 压测节点规模：
  - 10 节点，1 秒采样，24h 查询。
  - 100 节点，2 秒采样，1h/24h 查询。
  - 如需 7d 查询，单独压测。
- 如果压测不可接受，再新增：
  - `node_metric_rollups_1m`
  - `node_metric_rollups_5m`
  - `node_metric_rollups_1h`
- rollup worker 从 samples 聚合，detail 保持短保留。

验收：

- 现有 samples 查询在目标规模内可接受。
- 只有压测失败才进入新 rollup 表实施。
- API 返回 shape 保持向后兼容。

测试：

- `raw/1m/5m/1h` step 边界测试。
- 空数据、乱序时间、bucket 边界测试。
- retention 后查询行为测试。

## Phase 5: 安全 probe 产品化

目标：复用已有 probe job/result 链路，但先补安全策略、job 粒度和控制面优先级。

主要文件：

- `internal/probe/probe.go`
- `internal/app/handlers_nodes.go`
- `internal/repo/node_jobs.go`
- `internal/agent/agent.go`
- `internal/repo/node_metrics_history.go`

现状：

- `probe_ping` 实际是 DNS lookup，`count` 被校验但未使用。
- `probe_tcp` / `probe_http` 已实现。
- probe 复用 `node_jobs`。
- probe 结果写入 `node_probe_results`。
- probe 失败可同步 `ops_alerts`。

任务：

- 语义修正：
  - 将 `probe_ping` 改名为 `probe_dns`，或
  - 实现真正 ICMP ping。
  - 建议先改名，避免误导。
- target policy：
  - 默认拒绝 localhost。
  - 默认拒绝 link-local。
  - 默认拒绝 cloud metadata IP。
  - 默认拒绝私网段，除非显式 allow。
- HTTP probe：
  - 禁止自动跟随 redirect，或每跳重新校验目标。
  - 只允许 GET/HEAD。
  - 限制 response body 读取或不读取 body。
- job 粒度：
  - pending 去重从 `node_id + kind` 改为 `node_id + kind + target/correlation`。
  - 不同目标 probe 不互相覆盖。
- job 调度：
  - probe 不能饿死 `users_sync` / `xray_apply` / `xray_rollback`。
  - 增加 priority 或 probe 独立并发限制。
- UI：
  - 在 ops-v2 节点抽屉展示最近 probe results 和失败状态。
  - 支持手动创建 `probe_dns` / `probe_tcp` / `probe_http` job。
  - 周期探测作为后续扩展，不和安全策略同批做。

验收：

- 不同目标的 `probe_http` 不互相覆盖。
- probe 不能访问敏感内网地址。
- HTTP redirect 不绕过 target policy。
- probe 失败写 `node_probe_results` 并触发 ops alert。
- probe 恢复后 resolve 对应 ops alert。
- 控制面关键 job 不被大量 probe 阻塞。

测试：

- 私网/localhost/link-local/metadata IP 拒绝。
- redirect 到敏感地址拒绝。
- timeout clamp 测试。
- 不同 target 同 kind job 不覆盖。
- probe job 不阻塞关键 job。
- probe result 写入和 alert sync 测试。

## 推荐里程碑

### M1: 权限修复

范围：Phase 0。

输出：只读权限不再能写 ops alert、probe、metadata。

### M2: latest-cache

范围：Phase 1。

输出：`/ops` 和 WS 快照读路径收敛，前端 WS fallback 正常。

### M3: 写入缓冲

范围：Phase 2。

输出：历史 sample/detail 异步批量写入，report 只依赖 latest 关键写。

### M4: static facts hash 稳定化

范围：Phase 3。

输出：canonical hash 去重稳定。

### M5: metric 查询压测与 rollup 决策

范围：Phase 4。

输出：明确是否需要 rollup 表。如不需要，保留现有 samples 方案。

### M6: probe 安全化

范围：Phase 5。

输出：安全 target policy、job 粒度修复、基础 UI/结果展示稳定。

## 暂缓事项

- 暂缓 1 秒默认刷新，等 latest-cache 和写入缓冲完成。
- 暂缓新增 node metric rollup 表，等压测数据证明需要。
- 暂缓周期 probe；基础手动 probe UI 已完成，更复杂的探测编排等后续需求。
- 暂缓任意命令执行、terminal、JS worker 类能力。这些不符合 Neutrino 的安全边界。
