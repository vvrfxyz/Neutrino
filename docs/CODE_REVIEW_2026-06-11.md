# Neutrino 深度代码审查报告(2026-06-11)

> 审查方式:多 agent 并行审查 —— 8 个子系统 reviewer(panel 核心、HTTP handlers、repo 核心、repo 节点侧、node-agent、service/库、架构专项、测试/CI/文档)全量精读对应代码;每个 medium/high 发现再由独立验证 agent 读源码证实或证伪。共 50 个发现通过验证(或经多 reviewer 交叉佐证),3 个被证伪剔除。
> 工具链验证:`rtk go build ./...`、`rtk go vet ./...`、`rtk go test ./...` 全部通过(335 个测试 / 21 个包)。

## TL;DR

代码整体质量良好:认证分层一致、agent 端点有纵深防御(CN 与路径双校验)、usage 幂等设计扎实、job 状态机有 attempt fencing、测试覆盖面广。但存在 **5 个 high 级问题**(两个发布脚本静默失效、CI 无测试门禁、agent 证书续期回滚可能删除活体 mTLS 凭据、probe job 卡死会永久阻塞节点 job 队列),以及一批集中在 **job 队列竞态、agent usage 管道毒批次、错误字符串跨层契约、无界增长表** 上的 medium 问题。

---

## High(立即修复)

| # | 位置 | 问题 |
|---|------|------|
| H1 | `scripts/release/deploy_panel_remote.sh:197` | heredoc 终止符 `EOF` 粘在注释行尾(HEAD commit 7469ab8 引入),standalone 部署路径(默认路径)的 `docker compose pull/up`、healthz 检查全部被吞进 compose 文件,脚本 exit 0 报告成功但什么都没部署,且在远端留下损坏的 `docker-compose.release.yml`。 |
| H2 | `scripts/release/deploy_stack_remote.sh:125` | 同样的 heredoc 错误(`${TAG}EOF`),stack 部署完全失效、假报成功。 |
| H3 | `.github/workflows/build.yml:39` | CI 必需检查 `build`+`docker` 只编译不测试;`go test ./...` 从未在 CI 跑过,与 AGENTS.md Delivery Gates 第 1 条直接矛盾。测试失败的 PR 可以干净合入受保护的 main。 |
| H4 | `internal/agent/cert_renew.go:321` | 证书安装回滚闭包对 `u.had==false` 的路径无条件 `os.Remove(u.path)`——部分失败时可能删除 agent 正在使用的 mTLS key/cert/CA,节点永久掉线需重新 enroll。 |
| H5 | `internal/repo/node_jobs.go:674` | 超时清扫器的 kind→timeout map 不含 probe_*,卡在 running 的 probe job 永远不会被清扫;claim 是按节点串行的,该节点的整个 job 管道(users_sync/xray_apply)被永久卡死。 |

## Medium(按主题分组)

### 节点 job 队列(internal/repo/node_jobs.go)
- `:707` 超时清扫与 claim/finish 竞态:UPDATE 未按 attempts fencing,job 已 finish 后仍可能写入节点错误。
- `:43` `EnqueueNodeJob` 的 check-then-insert 去重不在事务里(直接跑在 `s.db` 上,绕过 `_txlock=immediate` 保护),并发下可产生重复 pending job。
- `:389` `FinishNodeJobForNode` 接受 agent 上报的任意 status 字符串(可写成 `pending`/`running`/非法值)。
- `:194` claim 长轮询每 400ms 开一个 BEGIN IMMEDIATE 写事务,即使无任务;N 个节点 × 每秒 2.5 次写锁竞争。
- `node_ops_extra.go:344` `CleanupOpsData` 每个 prune tick 每表只删一个批次(5000 行),高摄入下永远追不上。

### node-agent(internal/agent)
- `agent.go:706` reload 失败 + job 重试时,canonical-JSON skip-if-unchanged 把第二次 apply 误判为成功 —— 配置写盘了但 Xray 从未 reload。
- `agent.go:1589` usage 队列无毒批次处理:队头批次被面板永久拒绝(或文件损坏)时,整个节点的用量上报永久停止(flush-before-sample 不变量反而放大了影响)。
- `agent.go:144` `state.json` 加载错误被忽略、保存不 fsync:崩溃可回退 `AckedStats` 导致重复计量(面板幂等只防完全相同的 event id,epoch 重算后 id 会变)。
- `agent.go:719` xray_apply 的备份写失败被忽略(削弱 rollback);`.bak.*` 文件无限累积。
- `agent.go:347` Xray 被外部重启后用户全部丢失且无感知:用户恢复只在 agent 启动时跑一次,后续仅靠用户集变化触发 users_sync。
- `agent.go:1020` 心跳(默认 2s)做重量级采集:全连接表扫描 + 每活跃用户一次阻塞 gRPC 拨号取在线 IP。
- `cert_renew_test.go` 证书安装/回滚、DiskQueue 失败路径、job-runner 循环无测试。

### Panel 核心(internal/app)
- `cmd/server/main.go:56` 关停只 Shutdown 两个 HTTP server,worker goroutine 从不 join;metric-history 落盘 flush 与进程退出/DB 关闭竞态。
- `workers_ext.go:26` 告警派发在唯一的 enforcement worker 循环内同步发 SMTP(无超时),失败告警每 5s 永久重试。
- `audit_helpers.go:80` XFF 取最左(客户端可控)条目:在可信代理后可轮换伪造 IP 绕过 enroll/basic-auth 限速、污染审计日志。应改为 rightmost-untrusted 算法。
- `csrf.go:114` 带 Basic Authorization 头的请求跳过 CSRF:`ALLOW_BASIC_AUTH=true` 且浏览器缓存过 Basic 凭据时构成经典 Basic-auth CSRF。
- `ratelimit.go:33` 限速器 map 永不淘汰,key 可被未认证方(用户名、IP)无限撑大。
- `handlers_backups.go:259` restore 解压无解压后大小上限(压缩包上限 500MB,gzip ~1032:1 可写出 ~500GB 到 DB 同目录,撑爆面板磁盘)。
- `handlers_backups.go` 备份创建/下载(整库含凭据)/删除/恢复 staging 完全没有审计日志,是全面板唯一不审计的高危操作组。
- `metric_history_queue.go:229` 用 `strings.Contains(err.Error(), "details node_id=")` 判定"事务是否已提交"进而决定磁盘队列出队还是重试 —— 改个错误文案就会静默改变重复写入/无限重试行为。

### Repo / DB
- `internal/db/db.go:503` Migrate 在补列(compat ADD COLUMN)之前就对这些列建索引 —— 旧库升级直接失败(新库不受影响)。
- `internal/repo/usage.go:284` `usage_event_keys` 永不清理,以全量事件摄入速率无限增长(同类:`node_jobs`、`audit_logs`、`alerts`、`enforcement_logs`、`quota_windows`、过期 `admin_sessions` 也无清理)。
- `internal/repo/subscriptions.go:178` 公网 `/sub/{token}` 每次请求(含无效 token)都跑一个 `SweepExpiredUsers` 写事务,且该端点无限速。

### Service / 库
- `internal/subscription/render.go:166` 原始 `node.ExtraJSON` 被整体塞进 hysteria2/tuic 订阅 URI —— 服务端节点配置泄露给订阅者。
- `internal/service/ops.go:277` 单节点 upsert 会续整张列表的 TTL,且 health 是构建时预计算的 —— 静默节点可无限显示 online。
- `internal/bot/telegram.go:230` Telegram 管理员命令直连 repo.Store 绕过 UserService:`/disable` 不触发 users_sync 和 managed-xray reload(被禁用户连接不会断)。
- `internal/monitor/host.go:100` NIC 读取瞬时失败记 0,下个采样产生假 BPS 尖峰进入指标历史。
- `internal/xrayapi/client.go:213` 每次调用新建阻塞 gRPC 连接(3s 超时):agent 每个轮询周期 N 次拨号。

### 架构性(重构机会,均已验证)
- host-net 读取逻辑在 `internal/app/host_net.go`、`internal/agent/host_net.go`、`internal/monitor/host.go` 三处逐字重复(`internal/hostnet` 才是该去的地方)。
- service 层抽取只完成一半:节点控制面、证书、备份、ops-alerts、订阅 handler 仍直连 `repo.Store`。
- `repo.Store` 是无接口缝隙的 god object;`agent.go` 2117 行混五个子系统(seam 清晰:usage 管道 / 心跳 / job / xray apply / 证书)。
- 跨层错误映射靠子串匹配(`api_v1.go:636` 的 "not found"、metric-history 的提交语义、agent↔panel 的 usage 永久拒绝六个英文字符串契约 `panel_client.go:148`)。
- 死代码:`internal/singboxapi`(含 `sh -lc`,与 argv-only 原则冲突,幸而无人引用)、`internal/cryptoutil`、`internal/backup` 的 S3/restore 助手、`docker/xray/`(pin 1.8.24 与全仓 26.2.6 矛盾)、repo 多个无调用函数、agent State 的 Pending* 字段。
- SSR 模板从 CWD 相对路径 `template.Must` 加载,二进制必须在仓库根运行(测试 chdir、Dockerfile 靠 WORKDIR 兜底);应 go:embed。
- 日志统一是 `log.Printf` 无级别;HTTP 500 个别端点泄露原始 SQLite 错误文本(`handlers_traffic.go:246`)。

## Low(摘要)

输入校验类:traffic range 非法值静默回退 24h、ops alert severity 无枚举校验、agent mTLS 端点 JSON body 无大小限制、usage 批次服务端无条数上限、deploy 脚本用 Go `%q` 而非 shell 单引号转义(`$`/反引号会被 sh 展开)。
正确性类:`trafficBucketKey` monthly 布局 `2006-01-01` 重复月份、`SetUserStatus` 过期标记写在被回滚的事务里、`ResetUserQuota` 同秒二次重置撞 UNIQUE、Telegram poll offset 重启归零重放旧命令、节点名含空格在 clash/sing-box 中渲染成 `+`、access log 半行被消费偏移越过。
泄漏类:probe 的 throwaway http.Transport 不关 idle 连接、PanelClient 热替换不关旧 transport、enroll code 明文存储 + 非常数时间比较、ops cache 浅拷贝共享嵌套 map。
CI/打包类:workflows 在 push+PR 双触发(多架构 QEMU 构建每 commit 跑两遍)、panel 镜像 root 运行无 HEALTHCHECK、Playwright/vitest 配置了 CI reporter 但无 workflow 运行、backups 功能测试的 BackupDir 覆盖在 `app.New` 之后是死代码。
文档漂移:部署手册引用不存在的 `scripts/mtls/*`(且默认要求中间 CA,openssl 替代步骤不平凡)、NODE_MONTHLY_USAGE_DESIGN 的 time.Local 说法已过时(实际 AGENT_MONTH_TZ > time.Local > UTC)、AGENTS.md 缺 AGENT_MONTH_TZ / probe job kinds / 完整 UI 基线、AGENTS.md 引用已不存在的 `Xray-core/` 目录、AGENTS.md 的 "RecordUsage -> enforceLimit" 实际路径是 `UsageService.RecordBatch → RecordUsageBatchIdempotent → applyUsageEventTx → enforceLimit`。

## 被证伪(不要修)

- "Migration PRAGMA 跑在连接池上不安全" —— 验证后判定不构成实际问题。
- "notifier.SendMail 无超时会卡死中央循环" —— 与 app-core 已确认的告警派发问题重叠,按 workers_ext.go 那条处理即可。
- "internal/service 基本无测试" —— service 逻辑被功能测试间接充分覆盖。

## 经验证为健壮的设计(审查正面结论)

- agent 端点纵深防御:中间件之外每个 handler 独立复核 mTLS CN `node-{id}` == 路径 id。
- usage 幂等双保险:`usage_event_keys` PK + `traffic_events` 唯一索引;node_id 由 mTLS 身份强制覆盖,不可伪造。
- job finish 的 attempt fencing、禁用节点只许 drain users_sync、staged node deletion。
- 备份 restore staging 的校验链(magic 检测、只读打开、integrity_check、必需表校验、启动时快照换入)。
- WS hub:有界 per-subscriber buffer、锁内 close、零订阅者时不查库;同源校验防跨站 WebSocket。
- probe 的 SSRF 防护(loopback/link-local/metadata/私网默认拒绝、解析后 IP 直连防 DNS rebinding)。
- repo 层 SQL 全参数化,list 查询服务端 clamp。
