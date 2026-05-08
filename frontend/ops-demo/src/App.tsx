import {
  Activity,
  AlertTriangle,
  ArrowDownToLine,
  ArrowUpFromLine,
  Bell,
  Boxes,
  CheckCircle2,
  ChevronRight,
  CircleDot,
  Clock3,
  Cpu,
  Database,
  Gauge,
  Globe2,
  HardDrive,
  History,
  Layers3,
  LoaderCircle,
  MapPin,
  Network,
  PauseCircle,
  RefreshCw,
  Search,
  Server,
  ShieldAlert,
  SlidersHorizontal,
  X,
  Zap
} from "lucide-react";
import { useEffect, useId, useMemo, useState } from "react";
import { Button, Chip } from "@heroui/react";
import { connectLiveStream, enqueueNodeProbe, fetchLiveConfig, fetchLiveDataset, mergeLiveDataset } from "./adapters/liveAdapter";
import { jitterDataset, mockDataset } from "./adapters/mockAdapter";
import type { MonthUsage, NodeMetadata, OpsAlert, OpsDataset, OpsNode, ProbeJobPayload, ScenarioName } from "./types";
import {
  absoluteTime,
  compactVersion,
  fleetCounts,
  formatBps,
  formatBytes,
  formatDuration,
  formatPct,
  healthTone,
  metricMemoryPercent,
  nodeHasDrift,
  relativeTime
} from "./utils";

type DataMode = "mock" | "live";

const healthLabel: Record<string, string> = {
  online: "在线",
  stale: "滞后",
  disabled: "停用",
  error: "异常",
  drift: "漂移",
  unknown: "未知"
};

const SHOW_DEMO_CONTROLS = import.meta.env.DEV || (typeof window !== "undefined" && new URLSearchParams(window.location.search).has("demo"));

type TrendSeries = {
  label: string;
  color: string;
  values: number[];
};

const trendPalette = {
  cpu: "#2563eb",
  memory: "#059669",
  disk: "#d97706"
};

function App() {
  const [mode, setMode] = useState<DataMode>("live");
  const [scenario, setScenario] = useState<ScenarioName>("fleet");
  const [dataset, setDataset] = useState<OpsDataset>(() => mockDataset("empty"));
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [selectedNodeID, setSelectedNodeID] = useState<number | null>(null);
  const [liveIntervalMs, setLiveIntervalMs] = useState(2000);

  useEffect(() => {
    if (mode !== "mock") return;
    setError(scenario === "error" ? "snapshot stream disconnected" : "");
    setLoading(scenario === "loading");
    setDataset(mockDataset(scenario === "loading" ? "fleet" : scenario));
    const timer = window.setInterval(() => {
      setDataset((current) => jitterDataset(current));
    }, 2000);
    return () => window.clearInterval(timer);
  }, [mode, scenario]);

  useEffect(() => {
    if (mode !== "live") return;
    let stopped = false;
    let pollingTimer = 0;
    let streamOpen = false;
    let pollEveryMs = liveIntervalMs;
    let enrichEveryMs = 30000;
    let nextEnrichAt = 0;
    setLoading(true);
    setError("");

    const poll = async (forceEnrich = false) => {
      try {
        const now = Date.now();
        const enrich = forceEnrich || now >= nextEnrichAt;
        const live = await fetchLiveDataset({ enrich });
        if (enrich) nextEnrichAt = now + enrichEveryMs;
        if (!stopped) {
          setDataset((current) => mergeLiveDataset(current, live, { preserveEnrichment: !enrich }));
          setLoading(false);
          setError("");
        }
      } catch (err) {
        if (!stopped) {
          setLoading(false);
          setError(err instanceof Error ? err.message : "live polling failed");
        }
      }
    };

    const stopPolling = () => {
      if (pollingTimer) {
        window.clearInterval(pollingTimer);
        pollingTimer = 0;
      }
    };

    const startPolling = (forceEnrich = false) => {
      if (pollingTimer || stopped || streamOpen) return;
      void poll(forceEnrich);
      pollingTimer = window.setInterval(() => void poll(), pollEveryMs);
    };

    const start = async () => {
      const config = await fetchLiveConfig();
      if (stopped) return;
      pollEveryMs = config.poll_interval_ms;
      enrichEveryMs = config.enrich_interval_ms;
      setLiveIntervalMs(pollEveryMs);
      startPolling(true);
    };

    void start();
    const disconnect = connectLiveStream(
      (live) => {
        if (stopped) return;
        setDataset((current) => mergeLiveDataset(current, live, { preserveEnrichment: true }));
        setLoading(false);
        setError("");
      },
      (message) => {
        if (!stopped) {
          streamOpen = false;
          setError(message);
          startPolling(false);
        }
      },
      () => {
        streamOpen = true;
        stopPolling();
      }
    );
    return () => {
      stopped = true;
      stopPolling();
      disconnect();
    };
  }, [mode]);

  const filteredNodes = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return dataset.nodes;
    return dataset.nodes.filter((node) => {
      const location = nodeLocationSearchText(node);
      const haystack = [
        node.name,
        node.host,
        node.observed_ip,
        node.health,
        location,
        node.static_facts?.hostname,
        ...(node.metadata?.tags || [])
      ]
        .filter(Boolean)
        .join(" ")
        .toLowerCase();
      return haystack.includes(q);
    });
  }, [dataset.nodes, query]);

  const selectedNode = selectedNodeID == null ? null : dataset.nodes.find((node) => node.id === selectedNodeID) || null;
  const counts = useMemo(() => fleetCounts(dataset.nodes), [dataset.nodes]);
  const activeAlerts = dataset.alerts.filter((alert) => alert.status === "active");
  const failedProbes = dataset.probes.filter((probe) => !probe.success);
  const totals = useMemo(() => fleetTotals(dataset.nodes), [dataset.nodes]);

  const refreshLiveDataset = async () => {
    if (mode !== "live") return;
    try {
      const live = await fetchLiveDataset({ enrich: true });
      setDataset((current) => mergeLiveDataset(current, live));
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "live refresh failed");
    }
  };

  return (
    <main className="ops-shell">
      <div className="mx-auto max-w-[1760px] space-y-4">
        <OpsHeader
          mode={mode}
          scenario={scenario}
          dataset={dataset}
          counts={counts}
          loading={loading}
          error={error}
          liveIntervalMs={liveIntervalMs}
          showDemoControls={SHOW_DEMO_CONTROLS}
          onMode={setMode}
          onScenario={setScenario}
        />

        <div className="ops-layout">
          <section className="min-w-0 space-y-4">
            <HostStrip dataset={dataset} totals={totals} />
            <NodesPanel
              nodes={filteredNodes}
              selectedNodeID={selectedNode?.id || null}
              loading={loading}
              query={query}
              onQuery={setQuery}
              onSelect={setSelectedNodeID}
            />
          </section>

          <aside className="min-w-0 space-y-4">
            <FleetSummary counts={counts} totalNodes={dataset.nodes.length} totals={totals} />
            <FleetTrend dataset={dataset} />
            <AlertStack alerts={activeAlerts} />
            <OnlineUsers dataset={dataset} />
            {dataset.probes.length > 0 ? <ProbeStack failedCount={failedProbes.length} nodes={dataset.nodes} /> : null}
          </aside>
        </div>
      </div>

      <NodeDrawer node={selectedNode} onClose={() => setSelectedNodeID(null)} onProbeQueued={mode === "live" ? refreshLiveDataset : undefined} />
    </main>
  );
}

function OpsHeader({
  mode,
  scenario,
  dataset,
  counts,
  loading,
  error,
  liveIntervalMs,
  showDemoControls,
  onMode,
  onScenario
}: {
  mode: DataMode;
  scenario: ScenarioName;
  dataset: OpsDataset;
  counts: ReturnType<typeof fleetCounts>;
  loading: boolean;
  error: string;
  liveIntervalMs: number;
  showDemoControls: boolean;
  onMode: (mode: DataMode) => void;
  onScenario: (scenario: ScenarioName) => void;
}) {
  return (
    <header className="ops-panel rounded-[8px] px-4 py-4 lg:px-5">
      <div className="flex flex-col gap-4 xl:flex-row xl:items-center xl:justify-between">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="text-2xl font-bold leading-tight text-slate-900">运维监控</h1>
            <StatusBadge error={error} loading={loading} />
          </div>
          <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-slate-500">
            <span>节点 {counts.total} 个</span>
            <span>在线用户 {dataset.online.length} 个</span>
            <span>活动告警 {dataset.alerts.filter((alert) => alert.status === "active").length} 个</span>
            <span>最近同步 {absoluteTime(dataset.updated_at)}</span>
            <span>刷新间隔 {Math.max(1, Math.round(liveIntervalMs / 1000))} 秒</span>
          </div>
        </div>

        <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
          {showDemoControls ? (
            <>
              <Segmented
                value={mode}
                options={[
                  ["live", "实时"],
                  ["mock", "模拟"]
                ]}
                onChange={(value) => onMode(value as DataMode)}
              />
              <Segmented
                value={scenario}
                disabled={mode === "live"}
                options={[
                  ["fleet", "节点"],
                  ["loading", "加载"],
                  ["empty", "空态"],
                  ["error", "错误"]
                ]}
                onChange={(value) => onScenario(value as ScenarioName)}
              />
            </>
          ) : null}
          <Button className="h-9 rounded-[8px] bg-slate-900 px-3 text-white" size="sm" onClick={() => (mode === "live" ? window.location.reload() : onScenario(scenario))}>
            <span className="inline-flex items-center gap-1.5"><RefreshCw size={15} />刷新</span>
          </Button>
        </div>
      </div>
      {error ? (
        <div className="mt-3 rounded-[8px] border border-rose-200 bg-rose-50 px-3 py-2 text-xs text-rose-700">
          <span className="inline-flex min-w-0 items-center gap-1.5"><ShieldAlert className="shrink-0" size={14} /><span className="break-all">{error}</span></span>
        </div>
      ) : null}
    </header>
  );
}

function StatusBadge({ error, loading }: { error: string; loading: boolean }) {
  const className = error ? "bg-rose-50 text-rose-700" : loading ? "bg-sky-50 text-sky-700" : "bg-emerald-50 text-emerald-700";
  return (
    <span className={`inline-flex h-6 items-center gap-1.5 rounded-full px-2.5 text-xs font-medium ${className}`}>
      {loading ? <LoaderCircle className="animate-spin" size={13} /> : <CircleDot size={13} />}
      {loading ? "加载中" : error ? "数据源异常" : "实时"}
    </span>
  );
}

function HostStrip({ dataset, totals }: { dataset: OpsDataset; totals: ReturnType<typeof fleetTotals> }) {
  return (
    <section className="grid gap-3 md:grid-cols-2 xl:grid-cols-5">
      <StatTile icon={<Cpu size={18} />} label="CPU" value={formatPct(dataset.host.cpu_percent)} detail="面板主机" tone="slate" />
      <StatTile icon={<Database size={18} />} label="Memory" value={formatBytes(dataset.host.memory_bytes)} detail="当前占用" tone="slate" />
      <StatTile icon={<ArrowDownToLine size={18} />} label="Inbound" value={formatBps(dataset.host.inbound_bps)} detail="实时入站" tone="blue" />
      <StatTile icon={<ArrowUpFromLine size={18} />} label="Outbound" value={formatBps(dataset.host.outbound_bps)} detail="实时出站" tone="blue" />
      <StatTile icon={<Network size={18} />} label="节点本月流量" value={formatBytes(totals.monthTotal)} detail={`${totals.nodesWithMonth} 个节点已上报`} tone="emerald" />
    </section>
  );
}

function NodesPanel({
  nodes,
  selectedNodeID,
  loading,
  query,
  onQuery,
  onSelect
}: {
  nodes: OpsNode[];
  selectedNodeID: number | null;
  loading: boolean;
  query: string;
  onQuery: (value: string) => void;
  onSelect: (id: number) => void;
}) {
  return (
    <section className="ops-panel overflow-hidden rounded-[8px]">
      <div className="flex flex-col gap-3 border-b border-slate-200 px-4 py-4 lg:flex-row lg:items-center lg:justify-between">
        <div className="min-w-0">
          <h2 className="text-sm font-semibold text-slate-900">节点运行状态</h2>
          <p className="mt-1 text-xs text-slate-500">点击任意节点行打开详情，供应商 / 地区来自节点元数据。</p>
        </div>
        <label className="relative block w-full lg:max-w-sm">
          <Search className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-slate-400" size={16} />
          <input
            className="h-10 w-full rounded-[8px] border border-slate-200 bg-white pl-9 pr-3 text-sm text-slate-800 outline-none transition placeholder:text-slate-400 focus:border-slate-400 focus:ring-2 focus:ring-slate-100"
            placeholder="搜索节点、供应商、地区、标签"
            value={query}
            onChange={(event) => onQuery(event.target.value)}
          />
        </label>
      </div>

      <div className="hidden grid-cols-[minmax(260px,1.25fr)_minmax(230px,1fr)_minmax(190px,0.85fr)_minmax(170px,0.75fr)_48px] gap-3 border-b border-slate-100 bg-slate-50/80 px-4 py-2 text-[10px] font-bold uppercase tracking-[0.16em] text-slate-400 xl:grid">
        <div>Node</div>
        <div>Resources</div>
        <div>Network</div>
        <div>Month</div>
        <div />
      </div>

      <div className="space-y-3 bg-slate-50/70 p-3">
        {loading ? <NodeSkeleton /> : null}
        {!loading && nodes.length === 0 ? <EmptyBlock icon={<Boxes size={24} />} label="暂无节点" /> : null}
        {!loading && nodes.map((node) => (
          <NodeRow key={node.id} node={node} selected={node.id === selectedNodeID} onSelect={onSelect} />
        ))}
      </div>
    </section>
  );
}

function NodeSkeleton() {
  return (
    <div className="space-y-3">
      {Array.from({ length: 4 }, (_, index) => (
        <div key={index} className="grid animate-pulse gap-3 rounded-[8px] border border-slate-200 bg-white p-4 xl:grid-cols-[minmax(260px,1.25fr)_minmax(230px,1fr)_minmax(190px,0.85fr)_minmax(170px,0.75fr)_48px]">
          <div className="h-12 rounded bg-slate-100" />
          <div className="h-12 rounded bg-slate-100" />
          <div className="h-12 rounded bg-slate-100" />
          <div className="h-12 rounded bg-slate-100" />
          <div className="h-12 rounded bg-slate-100" />
        </div>
      ))}
    </div>
  );
}

function NodeRow({ node, selected, onSelect }: { node: OpsNode; selected: boolean; onSelect: (id: number) => void }) {
  const metrics = node.agent_metrics;
  const memoryPct = metricMemoryPercent(metrics);
  const usage = node.month_usage;
  const drift = nodeHasDrift(node);
  const tone = node.last_error ? "bad" : healthTone(node.health);
  const rowTone = {
    ok: "hover:border-emerald-200 hover:bg-emerald-50/30",
    warn: "hover:border-amber-200 hover:bg-amber-50/35",
    bad: "hover:border-rose-200 hover:bg-rose-50/35",
    muted: "hover:border-slate-300 hover:bg-slate-50"
  }[tone];
  const accent = {
    ok: "bg-emerald-500",
    warn: "bg-amber-500",
    bad: "bg-rose-500",
    muted: "bg-slate-300"
  }[tone];

  return (
    <button
      className={`node-card relative block w-full overflow-hidden rounded-[8px] border text-left shadow-sm transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sky-500 ${rowTone} ${selected ? "border-sky-400 bg-sky-50/40 ring-2 ring-sky-100" : "border-slate-200 bg-white"}`}
      type="button"
      onClick={() => onSelect(node.id)}
    >
      <span className={`absolute inset-y-3 left-0 w-1 rounded-r-full ${accent}`} />
      <div className="grid gap-4 px-4 py-4 pl-5 xl:grid-cols-[minmax(260px,1.25fr)_minmax(230px,1fr)_minmax(190px,0.85fr)_minmax(170px,0.75fr)_48px] xl:items-center">
        <div className="min-w-0">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <StatusChip node={node} />
            <span className="truncate text-base font-semibold text-slate-900">{node.name}</span>
            <span className="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-[11px] text-slate-500">#{node.id}</span>
          </div>
          <NodeLocation node={node} />
          <div className="mt-2 flex flex-wrap items-center gap-2 text-[11px] text-slate-500">
            <span>最近心跳 {heartbeatLabel(node.last_seen_at)}</span>
            {node.pending_jobs > 0 ? <span className="rounded bg-sky-50 px-1.5 py-0.5 text-sky-700">{node.pending_jobs} 个任务</span> : null}
            {drift ? <span className="rounded bg-amber-50 px-1.5 py-0.5 text-amber-700">版本不一致</span> : null}
            {node.last_error ? <span className="max-w-full truncate rounded bg-rose-50 px-1.5 py-0.5 text-rose-700">{node.last_error}</span> : null}
          </div>
        </div>

        <div className="space-y-2">
          <Meter label="CPU" value={metrics?.cpu_percent || 0} warn={85} />
          <Meter label="Memory" value={memoryPct} warn={85} />
          <Meter label="Disk" value={metrics?.disk_used_percent || 0} warn={90} />
        </div>

        <div className="grid grid-cols-2 gap-3 text-sm">
          <SmallReadout icon={<ArrowDownToLine size={14} />} label="入站" value={metrics ? formatBps(metrics.inbound_bps) : "-"} />
          <SmallReadout icon={<ArrowUpFromLine size={14} />} label="出站" value={metrics ? formatBps(metrics.outbound_bps) : "-"} />
        </div>

        <div className="min-w-0">
          <div className="font-mono text-lg font-semibold text-slate-900">{usage ? formatBytes(usage.total_bytes || usage.rx_bytes + usage.tx_bytes) : "-"}</div>
          <div className="mt-1 truncate text-[11px] text-slate-500">{usage ? `RX ${formatBytes(usage.rx_bytes)} / TX ${formatBytes(usage.tx_bytes)}` : "暂无自然月累计"}</div>
          <div className="mt-1 truncate text-[11px] text-slate-400">{monthUsageLabel(usage)}</div>
        </div>

        <div className="hidden justify-end xl:flex">
          <ChevronRight className="text-slate-400" size={18} />
        </div>
      </div>
    </button>
  );
}

function NodeLocation({ node }: { node: OpsNode }) {
  const location = nodeLocationDisplay(node);
  return (
    <div className="mt-2 flex min-w-0 flex-wrap items-center gap-2 text-xs">
      <span className={`inline-flex max-w-full items-center gap-1.5 rounded-full px-2 py-0.5 ${location.configured ? "bg-slate-100 text-slate-700" : "bg-slate-50 text-slate-500 ring-1 ring-inset ring-slate-200"}`}>
        <MapPin size={12} />
        <span className="truncate">{location.provider}</span>
      </span>
      {location.region ? <span className="max-w-full truncate text-slate-500">{location.region}</span> : null}
      {location.place ? <span className="max-w-full truncate text-slate-400">{location.place}</span> : null}
      {!location.configured && location.source ? <span className="max-w-full truncate text-slate-400">{location.source}</span> : null}
    </div>
  );
}

function SmallReadout({ icon, label, value }: { icon: React.ReactNode; label: string; value: string }) {
  return (
    <div className="min-w-0 rounded-[8px] border border-slate-100 bg-slate-50 px-3 py-2">
      <div className="flex items-center gap-1.5 text-[11px] text-slate-500">
        {icon}
        <span>{label}</span>
      </div>
      <div className="mt-1 truncate font-mono text-sm font-semibold text-slate-900">{value}</div>
    </div>
  );
}

function FleetSummary({ counts, totalNodes, totals }: { counts: ReturnType<typeof fleetCounts>; totalNodes: number; totals: ReturnType<typeof fleetTotals> }) {
  return (
    <section className="ops-panel rounded-[8px] p-4">
      <div className="mb-3 flex items-center justify-between">
        <h2 className="text-sm font-semibold text-slate-900">集群摘要</h2>
        <span className="text-xs text-slate-500">{totalNodes} nodes</span>
      </div>
      <div className="grid grid-cols-5 gap-2">
        <FleetPill label="在线" value={counts.online} tone="ok" />
        <FleetPill label="滞后" value={counts.stale} tone="warn" />
        <FleetPill label="停用" value={counts.disabled} tone="muted" />
        <FleetPill label="异常" value={counts.error} tone="bad" />
        <FleetPill label="漂移" value={counts.drift} tone="drift" />
      </div>
      <div className="mt-4 border-t border-slate-100 pt-4">
        <div className="text-[11px] font-medium text-slate-500">节点本月累计</div>
        <div className="mt-2 font-mono text-2xl font-semibold text-slate-900">{formatBytes(totals.monthTotal)}</div>
        <div className="mt-1 text-xs text-slate-500">RX {formatBytes(totals.monthRX)} / TX {formatBytes(totals.monthTX)}</div>
      </div>
    </section>
  );
}

function FleetTrend({ dataset }: { dataset: OpsDataset }) {
  const points = useMemo(() => {
    const sampledNodes = dataset.nodes.filter((node) => node.samples.length > 0);
    const maxLength = Math.max(0, ...sampledNodes.map((node) => node.samples.length));
    return Array.from({ length: maxLength }, (_, index) => {
      const samples = sampledNodes
        .map((node) => node.samples[index])
        .filter((point): point is OpsNode["samples"][number] => Boolean(point));
      return {
        cpu: average(samples.map((point) => point.cpu_percent)),
        memory: average(samples.map((point) => point.memory_used_percent)),
        disk: average(samples.map((point) => point.disk_used_percent))
      };
    });
  }, [dataset.nodes]);
  const series: TrendSeries[] = [
    { label: "CPU", color: trendPalette.cpu, values: points.map((point) => point.cpu) },
    { label: "Memory", color: trendPalette.memory, values: points.map((point) => point.memory) },
    { label: "Disk", color: trendPalette.disk, values: points.map((point) => point.disk) }
  ];

  return (
    <section className="ops-panel rounded-[8px] p-4">
      <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex items-center gap-2">
          <History size={17} className="text-sky-700" />
          <h2 className="text-sm font-semibold text-slate-900">短周期趋势</h2>
        </div>
        <TrendLegend series={series} />
      </div>
      <div className="chart-frame min-w-0">
        {points.length === 0 ? (
          <EmptyBlock icon={<Activity size={22} />} label="暂无采样" compact />
        ) : (
          <TrendSVG series={series} />
        )}
      </div>
    </section>
  );
}

function Segmented({
  value,
  options,
  disabled,
  onChange
}: {
  value: string;
  options: Array<[string, string]>;
  disabled?: boolean;
  onChange: (value: string) => void;
}) {
  return (
    <div className={`inline-grid grid-flow-col rounded-[8px] border border-slate-200 bg-slate-50 p-1 ${disabled ? "opacity-45" : ""}`}>
      {options.map(([key, label]) => (
        <button
          key={key}
          className={`h-8 min-w-14 rounded-[6px] px-3 text-xs font-semibold transition ${value === key ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}
          disabled={disabled}
          type="button"
          onClick={() => onChange(key)}
        >
          {label}
        </button>
      ))}
    </div>
  );
}

function FleetPill({ label, value, tone }: { label: string; value: number; tone: "ok" | "warn" | "muted" | "bad" | "drift" }) {
  const classes = {
    ok: "bg-emerald-50 text-emerald-700",
    warn: "bg-amber-50 text-amber-700",
    muted: "bg-slate-100 text-slate-500",
    bad: "bg-rose-50 text-rose-700",
    drift: "bg-sky-50 text-sky-700"
  }[tone];
  return (
    <div className={`rounded-[8px] px-2 py-2 ring-1 ring-inset ring-black/5 ${classes}`}>
      <div className="font-mono text-lg font-semibold leading-none">{value}</div>
      <div className="mt-1 text-[11px] font-medium">{label}</div>
    </div>
  );
}

function StatTile({ icon, label, value, detail, tone }: { icon: React.ReactNode; label: string; value: string; detail: string; tone: "slate" | "blue" | "emerald" }) {
  const color = {
    slate: "bg-slate-900 text-white",
    blue: "bg-sky-50 text-sky-700",
    emerald: "bg-emerald-50 text-emerald-700"
  }[tone];
  return (
    <div className="metric-card ops-panel rounded-[8px] p-4">
      <div className="flex items-start justify-between gap-3">
        <div className={`grid h-9 w-9 place-items-center rounded-[8px] ${color}`}>{icon}</div>
        <span className="truncate text-[10px] font-bold uppercase tracking-[0.16em] text-slate-400">{label}</span>
      </div>
      <div className="mt-4 truncate font-mono text-xl font-semibold text-slate-900">{value}</div>
      <div className="mt-1 truncate text-xs text-slate-500">{detail}</div>
    </div>
  );
}

function StatusChip({ node }: { node: OpsNode }) {
  const tone = node.last_error ? "bad" : healthTone(node.health);
  const className = {
    ok: "bg-emerald-50 text-emerald-700",
    warn: "bg-amber-50 text-amber-700",
    bad: "bg-rose-50 text-rose-700",
    muted: "bg-slate-100 text-slate-500"
  }[tone];
  const icon = tone === "ok" ? <CheckCircle2 size={13} /> : tone === "bad" ? <ShieldAlert size={13} /> : tone === "warn" ? <AlertTriangle size={13} /> : <PauseCircle size={13} />;
  return (
    <span className={`inline-flex h-6 shrink-0 items-center gap-1.5 rounded-full px-2 text-xs font-medium ${className}`}>
      {icon}
      {healthLabel[node.health] || "未知"}
    </span>
  );
}

function Meter({ label, value, warn }: { label: string; value: number; warn: number }) {
  const clamped = Math.max(0, Math.min(100, value));
  const color = clamped >= warn ? "bg-rose-500" : clamped >= warn * 0.75 ? "bg-amber-500" : "bg-emerald-500";
  return (
    <div>
      <div className="mb-1 flex items-center justify-between text-xs">
        <span className="text-slate-500">{label}</span>
        <span className="font-mono text-slate-900">{formatPct(clamped)}</span>
      </div>
      <div aria-label={label} className="h-1.5 overflow-hidden rounded-[4px] bg-slate-100">
        <div className={`h-full transition-none ${color}`} style={{ width: `${clamped}%` }} />
      </div>
    </div>
  );
}

function AlertStack({ alerts }: { alerts: OpsAlert[] }) {
  return (
    <section className="ops-panel rounded-[8px] p-4">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <Bell size={17} className="text-rose-700" />
          <h2 className="text-sm font-semibold text-slate-900">告警</h2>
        </div>
        <Chip className="rounded-[6px] bg-slate-100 text-slate-500" size="sm">{alerts.length}</Chip>
      </div>
      <div className="space-y-2">
        {alerts.length === 0 ? <EmptyBlock icon={<Bell size={20} />} label="暂无活动告警" compact /> : null}
        {alerts.slice(0, 5).map((alert) => (
          <div key={alert.id} className="rounded-[8px] border border-slate-200 bg-white p-3">
            <div className="flex items-start justify-between gap-2">
              <div className="min-w-0">
                <div className="truncate text-sm font-semibold text-slate-900">{alert.kind}</div>
                <div className="mt-1 break-words text-xs text-slate-600">{alert.message}</div>
              </div>
              <Chip className={`rounded-[6px] ${alert.severity === "critical" ? "bg-rose-50 text-rose-700" : "bg-amber-50 text-amber-700"}`} size="sm">
                {alert.severity}
              </Chip>
            </div>
            <div className="mt-2 text-[11px] text-slate-400">{stableAgo(alert.last_seen_at)}</div>
          </div>
        ))}
      </div>
    </section>
  );
}

function OnlineUsers({ dataset }: { dataset: OpsDataset }) {
  return (
    <section className="ops-panel rounded-[8px] p-4">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <Globe2 size={17} className="text-sky-700" />
          <h2 className="text-sm font-semibold text-slate-900">在线用户</h2>
        </div>
        <Chip className="rounded-[6px] bg-sky-50 text-sky-700" size="sm">{dataset.online.length}</Chip>
      </div>
      <div className="space-y-2">
        {dataset.online.length === 0 ? <EmptyBlock icon={<Globe2 size={20} />} label="暂无在线用户" compact /> : null}
        {dataset.online.slice(0, 8).map((item) => (
          <div key={`${item.user_id}-${item.client_ip}-${item.node_id || 0}`} className="grid grid-cols-[minmax(0,1fr)_auto] gap-3 rounded-[8px] border border-slate-200 bg-white p-3">
            <div className="min-w-0">
              <div className="truncate text-sm font-semibold text-slate-900">{item.username}</div>
              <div className="mt-1 break-all font-mono text-xs text-slate-600">{item.client_ip}</div>
            </div>
            <div className="text-right text-xs text-slate-500">
              <div>#{item.node_id || "-"}</div>
              <div className="mt-1">{stableAgo(item.last_seen_at)}</div>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function ProbeStack({ failedCount, nodes }: { failedCount: number; nodes: OpsNode[] }) {
  const probes = nodes.flatMap((node) => node.probes.map((probe) => ({ ...probe, nodeName: node.name }))).slice(0, 6);
  return (
    <section className="ops-panel rounded-[8px] p-4">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <Zap size={17} className="text-amber-700" />
          <h2 className="text-sm font-semibold text-slate-900">探测结果</h2>
        </div>
        <Chip className={`rounded-[6px] ${failedCount > 0 ? "bg-amber-50 text-amber-700" : "bg-emerald-50 text-emerald-700"}`} size="sm">{failedCount} 失败</Chip>
      </div>
      <div className="space-y-2">
        {probes.length === 0 ? <EmptyBlock icon={<Zap size={20} />} label="暂无探测" compact /> : null}
        {probes.map((probe) => (
          <div key={probe.id} className="rounded-[8px] border border-slate-200 bg-white p-3">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="truncate text-sm font-semibold text-slate-900">{probe.kind.replace("probe_", "")}</div>
                <div className="mt-1 break-all text-xs text-slate-600">{probe.target}</div>
                <div className="mt-1 truncate text-[11px] text-slate-400">{probe.nodeName}</div>
              </div>
              <Chip className={`rounded-[6px] ${probe.success ? "bg-emerald-50 text-emerald-700" : "bg-rose-50 text-rose-700"}`} size="sm">
                {probe.success ? `${Math.round(probe.latency_ms)}ms` : "失败"}
              </Chip>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function NodeDrawer({ node, onClose, onProbeQueued }: { node: OpsNode | null; onClose: () => void; onProbeQueued?: (nodeID: number) => void | Promise<void> }) {
  if (!node) return null;
  const metrics = node.agent_metrics;
  const facts = node.static_facts;
  const meta = node.metadata;
  const usage = node.month_usage;
  const locationDetail = nodeLocationDetail(node);
  const historySeries: TrendSeries[] = [
    { label: "CPU", color: trendPalette.cpu, values: node.samples.map((point) => point.cpu_percent) },
    { label: "Memory", color: trendPalette.memory, values: node.samples.map((point) => point.memory_used_percent) },
    { label: "Disk", color: trendPalette.disk, values: node.samples.map((point) => point.disk_used_percent) }
  ];

  return (
    <div className="fixed inset-0 z-50 flex justify-end">
      <button className="drawer-backdrop absolute inset-0" type="button" aria-label="Close drawer" onClick={onClose} />
      <aside className="relative h-full w-full max-w-[760px] overflow-y-auto bg-slate-50 shadow-2xl">
        <div className="sticky top-0 z-10 border-b border-slate-200 bg-white/95 px-4 py-4 backdrop-blur">
          <div className="flex items-start justify-between gap-4">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <StatusChip node={node} />
                <Chip className="rounded-[6px] bg-slate-100 text-slate-500" size="sm">#{node.id}</Chip>
              </div>
              <h2 className="mt-2 break-words text-2xl font-semibold leading-tight text-slate-900">{node.name}</h2>
              <div className="mt-1 text-sm text-slate-500">最近心跳 {absoluteTime(node.last_seen_at)} · {heartbeatLabel(node.last_seen_at)}</div>
            </div>
            <Button aria-label="Close" className="h-9 min-w-9 rounded-[8px] bg-slate-100 px-0 text-slate-900" size="sm" onClick={onClose}>
              <X size={16} />
            </Button>
          </div>
        </div>

        <div className="space-y-4 p-4">
          {node.last_error ? (
            <div className="rounded-[8px] border border-rose-200 bg-rose-50 p-3">
              <div className="mb-1 flex items-center gap-2 text-sm font-semibold text-rose-700">
                <ShieldAlert size={16} />
                最近异常
              </div>
              <div className="whitespace-pre-wrap break-all font-mono text-xs leading-5 text-rose-700">{node.last_error}</div>
            </div>
          ) : null}

          <section className="grid gap-3 sm:grid-cols-2">
            <DetailPanel icon={<MapPin size={16} />} label="位置" value={locationDetail.value} detail={locationDetail.detail} />
            <DetailPanel icon={<HardDrive size={16} />} label="磁盘" value={metrics ? `${formatBytes(metrics.disk_used_bytes)} / ${formatBytes(metrics.disk_total_bytes)}` : "-"} detail={metrics ? formatPct(metrics.disk_used_percent) : "-"} />
            <DetailPanel icon={<Clock3 size={16} />} label="运行时长" value={formatDuration(metrics?.system_uptime_sec || metrics?.uptime_sec || 0)} detail={`agent ${formatDuration(metrics?.agent_uptime_sec || metrics?.uptime_sec || 0)}`} />
            <DetailPanel icon={<Network size={16} />} label="本月流量" value={usage ? formatBytes(usage.total_bytes || usage.rx_bytes + usage.tx_bytes) : "-"} detail={usage ? `RX ${formatBytes(usage.rx_bytes)} / TX ${formatBytes(usage.tx_bytes)}` : "暂无自然月累计"} />
          </section>

          <section className="ops-panel rounded-[8px] p-4 shadow-none">
            <div className="mb-3 flex items-center gap-2">
              <SlidersHorizontal size={17} className="text-sky-700" />
              <h3 className="text-sm font-semibold text-slate-900">运行态详情</h3>
            </div>
            <div className="grid gap-3 sm:grid-cols-3">
              <SmallReadout icon={<Cpu size={15} />} label="Load 1/5/15" value={metrics ? `${metrics.load1 ?? 0} / ${metrics.load5 ?? 0} / ${metrics.load15 ?? 0}` : "-"} />
              <SmallReadout icon={<Network size={15} />} label="TCP / UDP" value={metrics ? `${metrics.tcp_connections || 0} / ${metrics.udp_connections || 0}` : "-"} />
              <SmallReadout icon={<Activity size={15} />} label="Processes" value={String(metrics?.process_count || 0)} />
              <SmallReadout icon={<ArrowDownToLine size={15} />} label="Disk read" value={formatBps(metrics?.disk_read_bps || 0)} />
              <SmallReadout icon={<ArrowUpFromLine size={15} />} label="Disk write" value={formatBps(metrics?.disk_write_bps || 0)} />
              <SmallReadout icon={<Layers3 size={15} />} label="Queue bytes" value={formatBytes(metrics?.queue_bytes || 0)} />
            </div>
          </section>

          <section className="ops-panel rounded-[8px] p-4 shadow-none">
            <div className="mb-3 flex items-center gap-2">
              <Server size={17} className="text-emerald-700" />
              <h3 className="text-sm font-semibold text-slate-900">静态信息</h3>
            </div>
            <div className="grid gap-2 text-sm sm:grid-cols-2">
              <Fact label="OS" value={facts ? `${facts.os_name} ${facts.os_version}` : "-"} />
              <Fact label="Kernel" value={facts ? `${facts.kernel} ${facts.kernel_version}` : "-"} />
              <Fact label="Arch" value={facts?.arch || "-"} />
              <Fact label="Hostname" value={facts?.hostname || "-"} />
              <Fact label="Virtualization" value={facts?.virtualization || "-"} />
              <Fact label="CPU" value={facts ? `${facts.cpu_model} · ${facts.cpu_logical_cores} threads` : "-"} />
              <Fact label="Xray" value={facts?.xray_version || "-"} />
            </div>
          </section>

          <section className="ops-panel rounded-[8px] p-4 shadow-none">
            <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
              <div className="flex items-center gap-2">
                <Gauge size={17} className="text-amber-700" />
                <h3 className="text-sm font-semibold text-slate-900">历史</h3>
              </div>
              {node.samples.length > 0 ? <TrendLegend series={historySeries} /> : null}
            </div>
            <div className="chart-frame min-w-0">
              {node.samples.length === 0 ? (
                <EmptyBlock icon={<History size={20} />} label="暂无采样" compact />
              ) : (
                <TrendSVG series={historySeries} />
              )}
            </div>
          </section>

          <ProbeManager node={node} onProbeQueued={onProbeQueued} />

          {meta?.tags?.length ? (
            <div className="flex flex-wrap gap-2">
              {meta.tags.map((tag) => (
                <Chip key={tag} className="rounded-[6px] bg-slate-100 text-slate-500" size="sm">{tag}</Chip>
              ))}
            </div>
          ) : null}
        </div>
      </aside>
    </div>
  );
}

const probeKindOptions: Array<{ value: ProbeJobPayload["kind"]; label: string }> = [
  { value: "probe_dns", label: "DNS" },
  { value: "probe_tcp", label: "TCP" },
  { value: "probe_http", label: "HTTP" }
];

function ProbeManager({ node, onProbeQueued }: { node: OpsNode; onProbeQueued?: (nodeID: number) => void | Promise<void> }) {
  const [kind, setKind] = useState<ProbeJobPayload["kind"]>("probe_dns");
  const [target, setTarget] = useState(node.host || "");
  const [url, setURL] = useState(node.host ? `https://${node.host}/` : "");
  const [port, setPort] = useState("443");
  const [method, setMethod] = useState<"GET" | "HEAD">("GET");
  const [timeoutMS, setTimeoutMS] = useState("3000");
  const [allowPrivate, setAllowPrivate] = useState(false);
  const [expectStatus, setExpectStatus] = useState("200");
  const [submitting, setSubmitting] = useState(false);
  const [message, setMessage] = useState("");
  const [formError, setFormError] = useState("");
  const inputClass = "h-9 w-full rounded-[6px] border border-slate-200 bg-white px-3 text-sm text-slate-900 outline-none transition focus:border-sky-400 focus:ring-2 focus:ring-sky-100 disabled:bg-slate-100";
  const labelClass = "mb-1 block text-xs font-medium text-slate-500";
  const latestProbes = node.probes.slice(0, 8);
  const disabled = !onProbeQueued || submitting;

  useEffect(() => {
    setTarget(node.host || "");
    setURL(node.host ? `https://${node.host}/` : "");
    setMessage("");
    setFormError("");
  }, [node.id, node.host]);

  const submit = async (event: { preventDefault: () => void }) => {
    event.preventDefault();
    if (!onProbeQueued) return;
    setFormError("");
    setMessage("");
    setSubmitting(true);
    try {
      const timeout = normalizeProbeNumber(timeoutMS, 3000, 100, 30000, "timeout_ms");
      const payload: ProbeJobPayload = { kind, timeout_ms: timeout, allow_private: allowPrivate };
      if (kind === "probe_dns") {
        const value = target.trim();
        if (!value) throw new Error("target 不能为空");
        payload.target = value;
      }
      if (kind === "probe_tcp") {
        const value = target.trim();
        if (!value) throw new Error("target 不能为空");
        payload.target = value;
        payload.port = normalizeProbeNumber(port, 443, 1, 65535, "port");
      }
      if (kind === "probe_http") {
        const value = url.trim();
        if (!value) throw new Error("url 不能为空");
        payload.url = value;
        payload.method = method;
        const statuses = parseProbeStatusCodes(expectStatus);
        if (statuses.length > 0) payload.expect_status = statuses;
      }
      const result = await enqueueNodeProbe(node.id, payload);
      setMessage(result.enqueued ? `已创建 job #${result.job_id}` : `已复用 job #${result.job_id}`);
      await onProbeQueued(node.id);
    } catch (err) {
      setFormError(err instanceof Error ? err.message : "probe job 创建失败");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <section className="ops-panel rounded-[8px] p-4 shadow-none">
      <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-center gap-2">
          <Zap size={17} className="text-amber-700" />
          <h3 className="text-sm font-semibold text-slate-900">Probe</h3>
        </div>
        <Chip className={`rounded-[6px] ${latestProbes.some((probe) => !probe.success) ? "bg-amber-50 text-amber-700" : "bg-emerald-50 text-emerald-700"}`} size="sm">
          {latestProbes.filter((probe) => !probe.success).length} 失败
        </Chip>
      </div>

      <form className="grid gap-3" onSubmit={submit}>
        <div className="grid gap-3 sm:grid-cols-[120px_minmax(0,1fr)_120px]">
          <label>
            <span className={labelClass}>类型</span>
            <select className={inputClass} disabled={disabled} value={kind} onChange={(event) => setKind(event.target.value as ProbeJobPayload["kind"])}>
              {probeKindOptions.map((item) => (
                <option key={item.value} value={item.value}>{item.label}</option>
              ))}
            </select>
          </label>
          {kind === "probe_http" ? (
            <label>
              <span className={labelClass}>URL</span>
              <input className={inputClass} disabled={disabled} value={url} onChange={(event) => setURL(event.target.value)} placeholder="https://example.com/healthz" />
            </label>
          ) : (
            <label>
              <span className={labelClass}>Target</span>
              <input className={inputClass} disabled={disabled} value={target} onChange={(event) => setTarget(event.target.value)} placeholder="example.com" />
            </label>
          )}
          {kind === "probe_tcp" ? (
            <label>
              <span className={labelClass}>Port</span>
              <input className={inputClass} disabled={disabled} inputMode="numeric" value={port} onChange={(event) => setPort(event.target.value)} />
            </label>
          ) : (
            <label>
              <span className={labelClass}>Timeout</span>
              <input className={inputClass} disabled={disabled} inputMode="numeric" value={timeoutMS} onChange={(event) => setTimeoutMS(event.target.value)} />
            </label>
          )}
        </div>

        {kind === "probe_http" || kind === "probe_tcp" ? (
          <div className="grid gap-3 sm:grid-cols-3">
            {kind === "probe_http" ? (
              <>
                <label>
                  <span className={labelClass}>Method</span>
                  <select className={inputClass} disabled={disabled} value={method} onChange={(event) => setMethod(event.target.value as "GET" | "HEAD")}>
                    <option value="GET">GET</option>
                    <option value="HEAD">HEAD</option>
                  </select>
                </label>
                <label>
                  <span className={labelClass}>Expect</span>
                  <input className={inputClass} disabled={disabled} value={expectStatus} onChange={(event) => setExpectStatus(event.target.value)} placeholder="200, 204" />
                </label>
              </>
            ) : null}
            <label>
              <span className={labelClass}>Timeout</span>
              <input className={inputClass} disabled={disabled} inputMode="numeric" value={timeoutMS} onChange={(event) => setTimeoutMS(event.target.value)} />
            </label>
          </div>
        ) : null}

        <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <label className="inline-flex items-center gap-2 text-xs font-medium text-slate-600">
            <input className="h-4 w-4 rounded border-slate-300 text-sky-600" checked={allowPrivate} disabled={disabled} type="checkbox" onChange={(event) => setAllowPrivate(event.target.checked)} />
            allow_private
          </label>
          <Button className="h-9 rounded-[8px] bg-slate-900 px-3 text-sm font-semibold text-white disabled:bg-slate-300" isDisabled={disabled} size="sm" type="submit">
            {submitting ? <LoaderCircle className="animate-spin" size={15} /> : <Zap size={15} />}
            创建探测
          </Button>
        </div>
        {formError ? <div className="rounded-[6px] bg-rose-50 px-3 py-2 text-xs font-medium text-rose-700">{formError}</div> : null}
        {message ? <div className="rounded-[6px] bg-emerald-50 px-3 py-2 text-xs font-medium text-emerald-700">{message}</div> : null}
      </form>

      <div className="mt-4 space-y-2">
        {latestProbes.length === 0 ? <EmptyBlock icon={<Zap size={20} />} label="暂无探测" compact /> : null}
        {latestProbes.map((probe) => (
          <div key={probe.id} className="rounded-[8px] border border-slate-200 bg-white p-3">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-sm font-semibold text-slate-900">{probeKindName(probe.kind)}</span>
                  {probe.status_code ? <span className="font-mono text-[11px] text-slate-400">HTTP {probe.status_code}</span> : null}
                </div>
                <div className="mt-1 break-all text-xs text-slate-600">{probe.target}</div>
                <div className="mt-1 text-[11px] text-slate-400">{absoluteTime(probe.checked_at)}</div>
                {!probe.success && probe.error ? <div className="mt-1 break-all text-[11px] text-rose-600">{probe.error}</div> : null}
              </div>
              <Chip className={`rounded-[6px] ${probe.success ? "bg-emerald-50 text-emerald-700" : "bg-rose-50 text-rose-700"}`} size="sm">
                {probe.success ? <CheckCircle2 size={13} /> : <AlertTriangle size={13} />}
                {probe.success ? `${Math.round(probe.latency_ms)}ms` : "失败"}
              </Chip>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function normalizeProbeNumber(raw: string, fallback: number, min: number, max: number, label: string): number {
  const value = raw.trim() === "" ? fallback : Number(raw);
  if (!Number.isFinite(value)) throw new Error(`${label} 必须是数字`);
  const rounded = Math.round(value);
  if (rounded < min || rounded > max) throw new Error(`${label} 需在 ${min}-${max}`);
  return rounded;
}

function parseProbeStatusCodes(raw: string): number[] {
  const text = raw.trim();
  if (!text) return [];
  const out = text.split(/[,\s]+/).filter(Boolean).map((part) => {
    const value = Number(part);
    if (!Number.isInteger(value) || value < 100 || value > 599) {
      throw new Error("expect_status 需为 100-599");
    }
    return value;
  });
  return Array.from(new Set(out));
}

function probeKindName(kind: string): string {
  if (kind === "probe_dns" || kind === "probe_ping") return "DNS";
  if (kind === "probe_tcp") return "TCP";
  if (kind === "probe_http") return "HTTP";
  return kind.replace("probe_", "").toUpperCase();
}

function DetailPanel({ icon, label, value, detail }: { icon: React.ReactNode; label: string; value: string; detail: string }) {
  return (
    <div className="rounded-[8px] border border-slate-200 bg-white p-3">
      <div className="mb-2 flex items-center gap-2 text-xs text-slate-500">
        {icon}
        <span>{label}</span>
      </div>
      <div className="break-words text-sm font-semibold text-slate-900">{value}</div>
      <div className="mt-1 break-words text-xs text-slate-500">{detail}</div>
    </div>
  );
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid grid-cols-[92px_minmax(0,1fr)] gap-2 rounded-[6px] bg-slate-50 px-3 py-2">
      <span className="text-xs text-slate-500">{label}</span>
      <span className="break-words font-mono text-xs text-slate-900">{value}</span>
    </div>
  );
}

function EmptyBlock({ icon, label, compact }: { icon: React.ReactNode; label: string; compact?: boolean }) {
  return (
    <div className={`grid place-items-center rounded-[8px] border border-dashed border-slate-200 bg-slate-50 text-center text-slate-500 ${compact ? "min-h-24 p-4" : "min-h-48 p-8"}`}>
      <div>
        <div className="mx-auto mb-2 grid h-10 w-10 place-items-center rounded-[8px] bg-white text-slate-400">{icon}</div>
        <div className="text-sm font-semibold">{label}</div>
      </div>
    </div>
  );
}

function TrendLegend({ series }: { series: TrendSeries[] }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      {series.map((item) => (
        <span key={item.label} className="inline-flex h-7 items-center gap-1.5 rounded-[6px] border border-slate-200 bg-white px-2 text-[11px] text-slate-600 shadow-sm">
          <span className="h-1 w-5 rounded-full" style={{ backgroundColor: item.color }} />
          <span className="font-medium">{item.label}</span>
          <span className="font-mono text-slate-400">{formatPct(latestValue(item.values))}</span>
        </span>
      ))}
    </div>
  );
}

function TrendSVG({ series }: { series: TrendSeries[] }) {
  const gridID = useId().replace(/:/g, "");
  return (
    <svg aria-label="metric trend chart" className="h-full w-full overflow-visible" preserveAspectRatio="none" viewBox="0 0 100 100">
      <defs>
        <pattern id={gridID} width="20" height="25" patternUnits="userSpaceOnUse">
          <path d="M 20 0 L 0 0 0 25" fill="none" stroke="#e2e8f0" strokeWidth="0.45" />
        </pattern>
      </defs>
      <rect width="100" height="100" fill="#f8fafc" />
      <rect width="100" height="100" fill={`url(#${gridID})`} />
      {series.map((line) => {
        const end = lineEnd(line.values);
        return (
          <g key={line.label}>
            <polyline fill="none" points={linePoints(line.values)} stroke={line.color} strokeLinecap="round" strokeLinejoin="round" strokeWidth="2.4" vectorEffect="non-scaling-stroke" />
            {end ? <circle cx={end.x} cy={end.y} fill={line.color} r="1.5" /> : null}
          </g>
        );
      })}
    </svg>
  );
}

function linePoints(values: number[]): string {
  if (values.length === 0) return "";
  const maxIndex = Math.max(1, values.length - 1);
  return values
    .map((value, index) => {
      const x = (index / maxIndex) * 100;
      const y = 100 - clampPct(value);
      return `${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(" ");
}

function lineEnd(values: number[]): { x: number; y: number } | null {
  if (values.length === 0) return null;
  const index = values.length - 1;
  const maxIndex = Math.max(1, index);
  return {
    x: (index / maxIndex) * 100,
    y: 100 - clampPct(values[index])
  };
}

function latestValue(values: number[]): number {
  for (let index = values.length - 1; index >= 0; index -= 1) {
    if (Number.isFinite(values[index])) return values[index];
  }
  return 0;
}

function clampPct(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.max(0, Math.min(100, value));
}

function nodeLocationParts(metadata?: NodeMetadata): [string, string, string] {
  const provider = cleanText(metadata?.provider);
  const region = cleanText(metadata?.region);
  const place = [cleanText(metadata?.city), cleanText(metadata?.country)].filter(Boolean).join(", ");
  return [provider, region, place];
}

function nodeLocationDisplay(node: OpsNode) {
  const [provider, region, place] = nodeLocationParts(node.metadata);
  const configured = Boolean(provider || region || place);
  const source = cleanText(node.observed_ip) || cleanText(node.host);
  if (!configured) {
    return {
      provider: "位置待配置",
      region: "",
      place: "",
      source: source ? `识别源 ${source}` : "",
      configured: false
    };
  }
  return {
    provider: provider || "供应商待配置",
    region,
    place,
    source: "",
    configured: true
  };
}

function nodeLocationDetail(node: OpsNode): { value: string; detail: string } {
  const location = nodeLocationDisplay(node);
  if (!location.configured) {
    return {
      value: "位置待配置",
      detail: location.source || "暂无供应商 / 地区元数据"
    };
  }
  return {
    value: [location.provider, location.region].filter(Boolean).join(" · "),
    detail: location.place || "城市 / 国家未配置"
  };
}

function nodeLocationSearchText(node: OpsNode): string {
  const location = nodeLocationDisplay(node);
  return [location.provider, location.region, location.place, location.source].filter(Boolean).join(" ");
}

function monthUsageLabel(usage?: MonthUsage): string {
  if (!usage) return "暂无自然月累计";
  const parts = [];
  if (usage.month_key) parts.push(usage.month_key);
  if (usage.timezone_name) parts.push(usage.timezone_name);
  if (usage.counter_source) parts.push(usage.counter_source);
  return parts.length > 0 ? parts.join(" · ") : "自然月累计";
}

function heartbeatLabel(value?: string): string {
  if (!value) return "-";
  const at = new Date(value).getTime();
  if (Number.isNaN(at)) return "-";
  const sec = Math.max(0, Math.floor((Date.now() - at) / 1000));
  if (sec < 60) return "1 分钟内";
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`;
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`;
  return `${Math.floor(sec / 86400)} 天前`;
}

function stableAgo(value?: string): string {
  if (!value) return "-";
  const at = new Date(value).getTime();
  if (Number.isNaN(at)) return "-";
  const sec = Math.max(0, Math.floor((Date.now() - at) / 1000));
  if (sec < 60) return "刚刚";
  return `${relativeTime(value)} 前`;
}

function fleetTotals(nodes: OpsNode[]) {
  return nodes.reduce(
    (acc, node) => {
      const usage = node.month_usage;
      if (!usage) return acc;
      acc.nodesWithMonth += 1;
      acc.monthRX += usage.rx_bytes || 0;
      acc.monthTX += usage.tx_bytes || 0;
      acc.monthTotal += usage.total_bytes || usage.rx_bytes + usage.tx_bytes;
      return acc;
    },
    { nodesWithMonth: 0, monthRX: 0, monthTX: 0, monthTotal: 0 }
  );
}

function average(values: number[]): number {
  const finite = values.filter(Number.isFinite);
  if (finite.length === 0) return 0;
  return finite.reduce((sum, value) => sum + value, 0) / finite.length;
}

function cleanText(value?: string): string {
  return (value || "").trim();
}

export default App;
