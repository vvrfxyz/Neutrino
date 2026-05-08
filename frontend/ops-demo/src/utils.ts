import type { AgentMetrics, NodeHealth, OpsNode } from "./types";

export function formatBytes(value: number): string {
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let n = Number.isFinite(value) ? Math.max(0, value) : 0;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i += 1;
  }
  return `${n.toFixed(i === 0 ? 0 : 2)} ${units[i]}`;
}

export function formatBps(value: number): string {
  return `${formatBytes(value)}/s`;
}

export function formatPct(value: number): string {
  const n = Number.isFinite(value) ? value : 0;
  return `${n.toFixed(1)}%`;
}

export function formatDuration(seconds: number): string {
  let s = Math.max(0, Math.floor(Number.isFinite(seconds) ? seconds : 0));
  const days = Math.floor(s / 86400);
  s %= 86400;
  const hours = Math.floor(s / 3600);
  s %= 3600;
  const minutes = Math.floor(s / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m`;
  return `${s}s`;
}

export function relativeTime(value?: string): string {
  if (!value) return "-";
  const at = new Date(value).getTime();
  if (Number.isNaN(at)) return "-";
  const sec = Math.max(0, Math.floor((Date.now() - at) / 1000));
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hour = Math.floor(min / 60);
  if (hour < 24) return `${hour}h`;
  return `${Math.floor(hour / 24)}d`;
}

export function absoluteTime(value?: string): string {
  if (!value) return "-";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "-";
  return new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false
  }).format(d);
}

export function healthTone(health: NodeHealth): "ok" | "warn" | "bad" | "muted" {
  if (health === "online") return "ok";
  if (health === "stale" || health === "drift") return "warn";
  if (health === "error") return "bad";
  return "muted";
}

export function nodeHasDrift(node: OpsNode): boolean {
  const usersDesired = node.desired_users_version || "";
  const usersApplied = node.applied_users_version || "";
  const xrayDesired = node.desired_xray_version || "";
  const xrayApplied = node.applied_xray_version || "";
  return (usersDesired !== "" && usersDesired !== usersApplied) || (xrayDesired !== "" && xrayDesired !== xrayApplied);
}

export function metricMemoryPercent(metrics?: AgentMetrics): number {
  if (!metrics || !metrics.memory_total_bytes || metrics.memory_total_bytes <= 0) return 0;
  return Math.min(100, Math.max(0, (metrics.memory_bytes / metrics.memory_total_bytes) * 100));
}

export function compactVersion(value?: string): string {
  const s = (value || "").trim();
  if (!s) return "-";
  return s.length > 14 ? `${s.slice(0, 6)}...${s.slice(-4)}` : s;
}

export function fleetCounts(nodes: OpsNode[]) {
  return nodes.reduce(
    (acc, node) => {
      acc.total += 1;
      if (node.health === "online") acc.online += 1;
      if (node.health === "stale") acc.stale += 1;
      if (node.health === "disabled") acc.disabled += 1;
      if (node.health === "error" || node.last_error) acc.error += 1;
      if (node.health === "drift" || nodeHasDrift(node)) acc.drift += 1;
      return acc;
    },
    { total: 0, online: 0, stale: 0, disabled: 0, error: 0, drift: 0 }
  );
}
