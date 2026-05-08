import type { HostSnapshot, NodeMetadata, OnlineUser, OpsDataset, OpsNode, ProbeResult } from "../types";
import { mockDataset } from "./mockAdapter";

type OpsSnapshotWire = {
  host?: HostSnapshot;
  online?: OnlineUser[];
  nodes?: Array<Partial<OpsNode> & { id: number; name: string }>;
};

export type LiveConfig = {
  poll_interval_ms: number;
  enrich_interval_ms: number;
};

type FetchLiveOptions = {
  enrich?: boolean;
};

type MergeOptions = {
  preserveEnrichment?: boolean;
  preserveAlerts?: boolean;
};

function normalizeNode(raw: Partial<OpsNode> & { id: number; name: string }): OpsNode {
  return {
    id: Number(raw.id),
    name: String(raw.name || `node-${raw.id}`),
    host: raw.host,
    observed_ip: raw.observed_ip,
    enabled: raw.enabled ?? true,
    managed: raw.managed ?? false,
    health: raw.health || "unknown",
    last_seen_at: raw.last_seen_at,
    last_error: raw.last_error,
    pending_jobs: Number(raw.pending_jobs || 0),
    running_kind: raw.running_kind,
    running_desired: raw.running_desired,
    desired_users_version: raw.desired_users_version,
    applied_users_version: raw.applied_users_version,
    desired_xray_version: raw.desired_xray_version,
    applied_xray_version: raw.applied_xray_version,
    agent_metrics: raw.agent_metrics,
    agent_metrics_at: raw.agent_metrics_at,
    month_usage: raw.month_usage,
    metadata: normalizeMetadata(raw.metadata),
    static_facts: raw.static_facts,
    samples: raw.samples || [],
    probes: raw.probes || []
  };
}

export async function fetchLiveConfig(): Promise<LiveConfig> {
  try {
    const data = await fetchJSON("/api/v1/ops/config");
    return {
      poll_interval_ms: clampInterval(Number(data.poll_interval_ms || 2000), 1000, 60000),
      enrich_interval_ms: clampInterval(Number(data.enrich_interval_ms || 30000), 10000, 300000)
    };
  } catch {
    return { poll_interval_ms: 2000, enrich_interval_ms: 30000 };
  }
}

export async function fetchLiveDataset(options: FetchLiveOptions = {}): Promise<OpsDataset> {
  const fallback = mockDataset("empty");
  const [hostResp, onlineResp, nodesResp, alertsResp] = await Promise.all([
    fetch("/api/v1/metrics/host?range=1h"),
    fetch("/api/v1/online-users"),
    fetch("/api/v1/ops/nodes"),
    fetch("/api/v1/ops/alerts?status=active")
  ]);
  if (!hostResp.ok || !onlineResp.ok || !nodesResp.ok || !alertsResp.ok) {
    throw new Error("live polling failed");
  }
  const hostJSON = await hostResp.json();
  const onlineJSON = await onlineResp.json();
  const nodesJSON = await nodesResp.json();
  const alertsJSON = await alertsResp.json();
  const hostItems = Array.isArray(hostJSON.items) ? hostJSON.items : [];
  const latest = hostItems.length > 0 ? hostItems[hostItems.length - 1] : null;
  const host: HostSnapshot = latest
    ? {
        cpu_percent: Number(latest.cpu_percent || 0),
        memory_bytes: Number(latest.memory_bytes || 0),
        inbound_bps: Number(latest.inbound_bps || 0),
        outbound_bps: Number(latest.outbound_bps || 0),
        month: hostJSON.month
      }
    : fallback.host;
  const baseNodes: OpsNode[] = (Array.isArray(nodesJSON.items) ? nodesJSON.items : []).map(normalizeNode);
  const nodes = options.enrich === false ? baseNodes : await Promise.all(baseNodes.map(enrichNode));
  const probes: ProbeResult[] = nodes.flatMap((node) => node.probes);
  return {
    host,
    nodes,
    online: Array.isArray(onlineJSON.items) ? onlineJSON.items : [],
    alerts: Array.isArray(alertsJSON.items) ? alertsJSON.items : [],
    probes,
    updated_at: new Date().toISOString()
  };
}

export function mergeLiveDataset(previous: OpsDataset, incoming: OpsDataset, options: MergeOptions = {}): OpsDataset {
  const previousByID = new Map(previous.nodes.map((node) => [node.id, node]));
  const nodes = incoming.nodes.map((node) => {
    const prior = previousByID.get(node.id);
    if (!prior || !options.preserveEnrichment) return node;
    return {
      ...prior,
      ...node,
      metadata: node.metadata || prior.metadata,
      static_facts: node.static_facts || prior.static_facts,
      samples: node.samples.length > 0 ? node.samples : prior.samples,
      probes: node.probes.length > 0 ? node.probes : prior.probes
    };
  });
  const probes = options.preserveEnrichment && incoming.probes.length === 0 ? nodes.flatMap((node) => node.probes) : incoming.probes;
  return {
    ...incoming,
    nodes,
    alerts: options.preserveAlerts && incoming.alerts.length === 0 ? previous.alerts : incoming.alerts,
    probes
  };
}

async function enrichNode(node: OpsNode): Promise<OpsNode> {
  const [metrics, facts, metadata, probes] = await Promise.allSettled([
    fetchJSON(`/api/v1/nodes/${node.id}/metrics?range=1h&step=1m`),
    fetchJSON(`/api/v1/nodes/${node.id}/static-facts/latest`),
    fetchJSON(`/api/v1/nodes/${node.id}/metadata`),
    fetchJSON(`/api/v1/nodes/${node.id}/probe-results`)
  ]);
  const out: OpsNode = { ...node };
  if (metrics.status === "fulfilled" && Array.isArray(metrics.value.items)) {
    out.samples = metrics.value.items.map((item: Record<string, unknown>) => ({
      sampled_at: String(item.bucket_start || item.sampled_at || ""),
      cpu_percent: Number(item.cpu_avg || item.cpu_percent || 0),
      memory_used_percent: Number(item.memory_used_percent_avg || item.memory_used_percent || 0),
      disk_used_percent: Number(item.disk_used_percent_avg || item.disk_used_percent || 0),
      inbound_bps: Number(item.inbound_bps_avg || item.inbound_bps || 0),
      outbound_bps: Number(item.outbound_bps_avg || item.outbound_bps || 0),
      queue_batches: Number(item.queue_batches_max || item.queue_batches || 0)
    }));
  }
  if (facts.status === "fulfilled" && facts.value.item) {
    out.static_facts = facts.value.item;
  }
  if (metadata.status === "fulfilled" && metadata.value.item) {
    out.metadata = normalizeMetadata(metadata.value.item);
  }
  if (probes.status === "fulfilled" && Array.isArray(probes.value.items)) {
    out.probes = probes.value.items;
  }
  return out;
}

function normalizeMetadata(raw: unknown): NodeMetadata | undefined {
  if (!raw || typeof raw !== "object") return undefined;
  const item = raw as Record<string, unknown>;
  return {
    provider: text(item.provider),
    region: text(item.region),
    country: text(item.country),
    city: text(item.city),
    tags: Array.isArray(item.tags) ? item.tags.map((tag) => text(tag)).filter(Boolean) : [],
    monthly_cost_cents: numberValue(item.monthly_cost_cents ?? item.monthlyCostCents),
    currency: text(item.currency) || "USD",
    renew_cycle: text(item.renew_cycle ?? item.renewCycle),
    renew_at: optionalText(item.renew_at ?? item.renewAt),
    note: optionalText(item.note)
  };
}

function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function optionalText(value: unknown): string | undefined {
  const out = text(value);
  return out || undefined;
}

function numberValue(value: unknown): number {
  const n = Number(value || 0);
  return Number.isFinite(n) ? n : 0;
}

async function fetchJSON(path: string) {
  const resp = await fetch(path);
  if (!resp.ok) {
    throw new Error(`${path} returned ${resp.status}`);
  }
  return resp.json();
}

function clampInterval(value: number, min: number, max: number): number {
  if (!Number.isFinite(value)) return min;
  if (value < min) return min;
  if (value > max) return max;
  return Math.round(value);
}

export function connectLiveStream(onDataset: (dataset: OpsDataset) => void, onError: (message: string) => void): () => void {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${window.location.host}/api/v1/stream`);
  let closed = false;

  ws.addEventListener("message", (event) => {
    try {
      const envelope = JSON.parse(event.data);
      if (!envelope || envelope.kind !== "ops_snapshot" || !envelope.data) return;
      const data = envelope.data as OpsSnapshotWire;
      const nodes: OpsNode[] = Array.isArray(data.nodes) ? data.nodes.map(normalizeNode) : [];
      onDataset({
        host: data.host || mockDataset("empty").host,
        nodes,
        online: Array.isArray(data.online) ? data.online : [],
        alerts: [],
        probes: nodes.flatMap((node) => node.probes),
        updated_at: envelope.at || new Date().toISOString()
      });
    } catch (err) {
      onError(err instanceof Error ? err.message : "stream decode failed");
    }
  });

  ws.addEventListener("error", () => onError("stream error"));
  ws.addEventListener("close", () => {
    if (!closed) onError("stream closed");
  });

  return () => {
    closed = true;
    ws.close();
  };
}
