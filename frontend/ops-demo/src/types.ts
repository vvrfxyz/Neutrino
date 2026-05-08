export type NodeHealth = "online" | "stale" | "disabled" | "error" | "drift" | "unknown";
export type ScenarioName = "fleet" | "loading" | "empty" | "error";

export type AgentMetrics = {
  cpu_percent: number;
  load1?: number;
  load5?: number;
  load15?: number;
  memory_bytes: number;
  memory_total_bytes?: number;
  memory_available_bytes?: number;
  swap_used_bytes?: number;
  swap_total_bytes?: number;
  inbound_bps: number;
  outbound_bps: number;
  disk_total_bytes: number;
  disk_used_bytes: number;
  disk_free_bytes: number;
  disk_used_percent: number;
  disk_read_bps?: number;
  disk_write_bps?: number;
  tcp_connections?: number;
  udp_connections?: number;
  process_count?: number;
  system_uptime_sec?: number;
  agent_uptime_sec?: number;
  uptime_sec?: number;
  queue_bytes: number;
  queue_batches: number;
  goroutines: number;
};

export type MonthUsage = {
  month_key: string;
  timezone_name: string;
  rx_bytes: number;
  tx_bytes: number;
  total_bytes: number;
  counter_source?: string;
  last_reported_at?: string;
};

export type NodeMetadata = {
  provider: string;
  region: string;
  country: string;
  city: string;
  tags: string[];
  monthly_cost_cents: number;
  currency: string;
  renew_cycle: string;
  renew_at?: string;
  note?: string;
};

export type StaticFacts = {
  os_name: string;
  os_version: string;
  kernel: string;
  kernel_version: string;
  arch: string;
  hostname: string;
  virtualization: string;
  cpu_model: string;
  cpu_physical_cores: number;
  cpu_logical_cores: number;
  agent_version: string;
  xray_version: string;
  reported_at: string;
};

export type MetricPoint = {
  sampled_at: string;
  cpu_percent: number;
  memory_used_percent: number;
  disk_used_percent: number;
  inbound_bps: number;
  outbound_bps: number;
  queue_batches: number;
};

export type ProbeResult = {
  id: number;
  node_id: number;
  kind: "probe_ping" | "probe_tcp" | "probe_http";
  target: string;
  success: boolean;
  latency_ms: number;
  status_code?: number;
  error?: string;
  checked_at: string;
};

export type OpsAlert = {
  id: number;
  node_id?: number;
  kind: string;
  severity: "info" | "warning" | "critical";
  status: "active" | "resolved";
  message: string;
  last_seen_at: string;
};

export type OpsNode = {
  id: number;
  name: string;
  host?: string;
  observed_ip?: string;
  enabled: boolean;
  managed: boolean;
  health: NodeHealth;
  last_seen_at?: string;
  last_error?: string;
  pending_jobs: number;
  running_kind?: string;
  running_desired?: string;
  desired_users_version?: string;
  applied_users_version?: string;
  desired_xray_version?: string;
  applied_xray_version?: string;
  agent_metrics?: AgentMetrics;
  agent_metrics_at?: string;
  month_usage?: MonthUsage;
  metadata?: NodeMetadata;
  static_facts?: StaticFacts;
  samples: MetricPoint[];
  probes: ProbeResult[];
};

export type OnlineUser = {
  user_id: number;
  username: string;
  client_ip: string;
  node_id?: number;
  first_seen_at: string;
  last_seen_at: string;
};

export type HostSnapshot = {
  cpu_percent: number;
  memory_bytes: number;
  inbound_bps: number;
  outbound_bps: number;
  month?: {
    rx_bytes: number;
    tx_bytes: number;
    total_bytes: number;
  };
};

export type OpsDataset = {
  host: HostSnapshot;
  nodes: OpsNode[];
  online: OnlineUser[];
  alerts: OpsAlert[];
  probes: ProbeResult[];
  updated_at: string;
};

export type AdapterState = {
  dataset: OpsDataset;
  loading: boolean;
  error: string;
  source: "mock" | "live";
};
