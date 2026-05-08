import type { OpsDataset, OpsNode, ScenarioName } from "../types";

const GB = 1024 * 1024 * 1024;
const MB = 1024 * 1024;

function isoAgo(seconds: number): string {
  return new Date(Date.now() - seconds * 1000).toISOString();
}

function makeSeries(seed: number, count = 42) {
  const now = Date.now();
  return Array.from({ length: count }, (_, index) => {
    const x = index / Math.max(1, count - 1);
    const wave = Math.sin((x * Math.PI * 2 + seed) * 1.4);
    const pulse = Math.cos((x * Math.PI * 2 + seed) * 2.6);
    return {
      sampled_at: new Date(now - (count - index) * 120_000).toISOString(),
      cpu_percent: Math.max(1, Math.min(98, 28 + seed * 7 + wave * 16 + pulse * 5)),
      memory_used_percent: Math.max(8, Math.min(96, 42 + seed * 5 + pulse * 9)),
      disk_used_percent: Math.max(20, Math.min(96, 48 + seed * 6 + x * 9)),
      inbound_bps: Math.max(0, (22 + seed * 14 + wave * 10) * MB),
      outbound_bps: Math.max(0, (18 + seed * 13 + pulse * 8) * MB),
      queue_batches: Math.max(0, Math.round(seed % 2 === 0 ? pulse + 1 : pulse * 2 + 2))
    };
  });
}

const metadata = [
  {
    provider: "Akamai",
    region: "ap-east-hkg",
    country: "HK",
    city: "Hong Kong",
    tags: ["edge", "premium", "vision"],
    monthly_cost_cents: 2400,
    currency: "USD",
    renew_cycle: "month",
    renew_at: isoAgo(-9 * 86400),
    note: "Main Asia edge node"
  },
  {
    provider: "Hetzner",
    region: "eu-central",
    country: "DE",
    city: "Falkenstein",
    tags: ["bulk", "eu"],
    monthly_cost_cents: 880,
    currency: "EUR",
    renew_cycle: "month",
    renew_at: isoAgo(-4 * 86400)
  },
  {
    provider: "Oracle",
    region: "us-west",
    country: "US",
    city: "San Jose",
    tags: ["trial", "watch"],
    monthly_cost_cents: 0,
    currency: "USD",
    renew_cycle: "free-tier"
  }
];

const facts = [
  {
    os_name: "Ubuntu",
    os_version: "24.04.2 LTS",
    kernel: "Linux",
    kernel_version: "6.8.0-57-generic",
    arch: "amd64",
    hostname: "hkg-edge-01",
    virtualization: "kvm",
    cpu_model: "AMD EPYC 7713",
    cpu_physical_cores: 2,
    cpu_logical_cores: 4,
    agent_version: "0.9.8",
    xray_version: "26.2.6",
    reported_at: isoAgo(122)
  },
  {
    os_name: "Debian",
    os_version: "12.7",
    kernel: "Linux",
    kernel_version: "6.1.0-25-amd64",
    arch: "amd64",
    hostname: "fra-bulk-02",
    virtualization: "kvm",
    cpu_model: "Intel Xeon Gold",
    cpu_physical_cores: 4,
    cpu_logical_cores: 4,
    agent_version: "0.9.7",
    xray_version: "26.2.6",
    reported_at: isoAgo(620)
  },
  {
    os_name: "Oracle Linux",
    os_version: "9.4",
    kernel: "Linux",
    kernel_version: "5.15.0-204",
    arch: "aarch64",
    hostname: "sjc-watch-03",
    virtualization: "oci",
    cpu_model: "Ampere Altra",
    cpu_physical_cores: 2,
    cpu_logical_cores: 2,
    agent_version: "0.9.3",
    xray_version: "unknown",
    reported_at: isoAgo(2600)
  }
];

const nodes: OpsNode[] = [
  {
    id: 11,
    name: "hkg-edge-01-long-name-that-stays-readable",
    enabled: true,
    managed: true,
    health: "online",
    last_seen_at: isoAgo(4),
    pending_jobs: 0,
    running_kind: "",
    desired_users_version: "8d1a9c1fd6b2481e",
    applied_users_version: "8d1a9c1fd6b2481e",
    desired_xray_version: "d92e5a0c77a841ab",
    applied_xray_version: "d92e5a0c77a841ab",
    agent_metrics_at: isoAgo(3),
    agent_metrics: {
      cpu_percent: 31.4,
      load1: 0.28,
      load5: 0.34,
      load15: 0.33,
      memory_bytes: 2.8 * GB,
      memory_total_bytes: 7.8 * GB,
      memory_available_bytes: 4.6 * GB,
      swap_used_bytes: 0,
      swap_total_bytes: 1 * GB,
      inbound_bps: 68 * MB,
      outbound_bps: 54 * MB,
      disk_total_bytes: 80 * GB,
      disk_used_bytes: 37 * GB,
      disk_free_bytes: 43 * GB,
      disk_used_percent: 46.2,
      disk_read_bps: 11 * MB,
      disk_write_bps: 6 * MB,
      tcp_connections: 183,
      udp_connections: 17,
      process_count: 138,
      system_uptime_sec: 18 * 86400 + 3600,
      agent_uptime_sec: 5 * 86400 + 420,
      uptime_sec: 5 * 86400 + 420,
      queue_bytes: 0,
      queue_batches: 0,
      goroutines: 36
    },
    month_usage: {
      month_key: "2026-05",
      timezone_name: "Asia/Shanghai",
      rx_bytes: 4.8 * 1024 * GB,
      tx_bytes: 3.9 * 1024 * GB,
      total_bytes: 8.7 * 1024 * GB,
      last_reported_at: isoAgo(4)
    },
    metadata: metadata[0],
    static_facts: facts[0],
    samples: makeSeries(1),
    probes: [
      { id: 1, node_id: 11, kind: "probe_tcp", target: "gateway:443", success: true, latency_ms: 41, checked_at: isoAgo(90) },
      { id: 2, node_id: 11, kind: "probe_http", target: "https://www.microsoft.com", success: true, latency_ms: 168, status_code: 200, checked_at: isoAgo(320) }
    ]
  },
  {
    id: 12,
    name: "fra-bulk-02",
    enabled: true,
    managed: true,
    health: "drift",
    last_seen_at: isoAgo(18),
    pending_jobs: 2,
    running_kind: "xray_apply",
    running_desired: "2ec1b3b5ac9844e0",
    desired_users_version: "d13bbd9b715d4a20",
    applied_users_version: "9af03c01a45e40b6",
    desired_xray_version: "2ec1b3b5ac9844e0",
    applied_xray_version: "f8c17a0924e84fbb",
    agent_metrics_at: isoAgo(19),
    agent_metrics: {
      cpu_percent: 72.2,
      load1: 1.92,
      load5: 1.38,
      load15: 1.08,
      memory_bytes: 5.9 * GB,
      memory_total_bytes: 7.8 * GB,
      memory_available_bytes: 1.4 * GB,
      swap_used_bytes: 220 * MB,
      swap_total_bytes: 1 * GB,
      inbound_bps: 122 * MB,
      outbound_bps: 108 * MB,
      disk_total_bytes: 160 * GB,
      disk_used_bytes: 128 * GB,
      disk_free_bytes: 32 * GB,
      disk_used_percent: 80.1,
      disk_read_bps: 38 * MB,
      disk_write_bps: 24 * MB,
      tcp_connections: 391,
      udp_connections: 42,
      process_count: 169,
      system_uptime_sec: 94 * 86400,
      agent_uptime_sec: 7200,
      uptime_sec: 7200,
      queue_bytes: 22 * MB,
      queue_batches: 4,
      goroutines: 48
    },
    month_usage: {
      month_key: "2026-05",
      timezone_name: "UTC+02",
      rx_bytes: 12.2 * 1024 * GB,
      tx_bytes: 10.7 * 1024 * GB,
      total_bytes: 22.9 * 1024 * GB,
      last_reported_at: isoAgo(21)
    },
    metadata: metadata[1],
    static_facts: facts[1],
    samples: makeSeries(4),
    probes: [
      { id: 3, node_id: 12, kind: "probe_tcp", target: "gateway:443", success: true, latency_ms: 88, checked_at: isoAgo(88) },
      { id: 4, node_id: 12, kind: "probe_http", target: "https://example.com/healthz", success: false, latency_ms: 0, status_code: 502, error: "unexpected status", checked_at: isoAgo(210) }
    ]
  },
  {
    id: 13,
    name: "sjc-watch-03",
    enabled: true,
    managed: true,
    health: "stale",
    last_seen_at: isoAgo(338),
    pending_jobs: 1,
    running_kind: "",
    desired_users_version: "acd2f7e12f10495b",
    applied_users_version: "acd2f7e12f10495b",
    desired_xray_version: "d92e5a0c77a841ab",
    applied_xray_version: "d92e5a0c77a841ab",
    agent_metrics_at: isoAgo(340),
    last_error: "runtime report stale: last successful node report exceeded 2m threshold",
    agent_metrics: {
      cpu_percent: 4.1,
      memory_bytes: 780 * MB,
      memory_total_bytes: 3.6 * GB,
      memory_available_bytes: 2.6 * GB,
      inbound_bps: 0,
      outbound_bps: 0,
      disk_total_bytes: 50 * GB,
      disk_used_bytes: 18 * GB,
      disk_free_bytes: 32 * GB,
      disk_used_percent: 36.2,
      tcp_connections: 12,
      udp_connections: 2,
      process_count: 93,
      system_uptime_sec: 12 * 86400,
      agent_uptime_sec: 3600,
      uptime_sec: 3600,
      queue_bytes: 4 * MB,
      queue_batches: 1,
      goroutines: 23
    },
    month_usage: {
      month_key: "2026-05",
      timezone_name: "UTC-07",
      rx_bytes: 700 * GB,
      tx_bytes: 560 * GB,
      total_bytes: 1260 * GB,
      last_reported_at: isoAgo(340)
    },
    metadata: metadata[2],
    static_facts: facts[2],
    samples: makeSeries(2),
    probes: [{ id: 5, node_id: 13, kind: "probe_dns", target: "1.1.1.1", success: false, latency_ms: 0, error: "timeout", checked_at: isoAgo(130) }]
  },
  {
    id: 14,
    name: "disabled-archive-node",
    enabled: false,
    managed: false,
    health: "disabled",
    last_seen_at: isoAgo(14 * 86400),
    pending_jobs: 0,
    desired_users_version: "",
    applied_users_version: "",
    desired_xray_version: "",
    applied_xray_version: "",
    samples: [],
    probes: []
  },
  {
    id: 15,
    name: "tyo-hotfix-node-with-a-very-long-provider-label",
    enabled: true,
    managed: true,
    health: "error",
    last_seen_at: isoAgo(7),
    pending_jobs: 0,
    running_kind: "",
    last_error: "xray_apply failed: rendered config is not valid json near inbound[0].settings.clients[31].flow",
    desired_users_version: "6bd8e1db91294493",
    applied_users_version: "6bd8e1db91294493",
    desired_xray_version: "c17ab8e12d4447aa",
    applied_xray_version: "5f9d212a701f459d",
    agent_metrics_at: isoAgo(8),
    agent_metrics: {
      cpu_percent: 92.4,
      load1: 4.8,
      load5: 3.6,
      load15: 2.9,
      memory_bytes: 7.1 * GB,
      memory_total_bytes: 7.8 * GB,
      memory_available_bytes: 360 * MB,
      swap_used_bytes: 720 * MB,
      swap_total_bytes: 1 * GB,
      inbound_bps: 18 * MB,
      outbound_bps: 21 * MB,
      disk_total_bytes: 80 * GB,
      disk_used_bytes: 73 * GB,
      disk_free_bytes: 7 * GB,
      disk_used_percent: 91.3,
      disk_read_bps: 4 * MB,
      disk_write_bps: 19 * MB,
      tcp_connections: 221,
      udp_connections: 13,
      process_count: 177,
      system_uptime_sec: 31 * 86400,
      agent_uptime_sec: 640,
      uptime_sec: 640,
      queue_bytes: 0,
      queue_batches: 0,
      goroutines: 41
    },
    month_usage: {
      month_key: "2026-05",
      timezone_name: "Asia/Tokyo",
      rx_bytes: 2.1 * 1024 * GB,
      tx_bytes: 1.8 * 1024 * GB,
      total_bytes: 3.9 * 1024 * GB,
      last_reported_at: isoAgo(8)
    },
    metadata: {
      provider: "Vultr",
      region: "ap-northeast",
      country: "JP",
      city: "Tokyo",
      tags: ["hotfix", "at-risk"],
      monthly_cost_cents: 1200,
      currency: "USD",
      renew_cycle: "month",
      renew_at: isoAgo(-2 * 86400)
    },
    static_facts: {
      ...facts[0],
      hostname: "tyo-hotfix-05",
      reported_at: isoAgo(10)
    },
    samples: makeSeries(7),
    probes: [{ id: 6, node_id: 15, kind: "probe_tcp", target: "gateway:443", success: true, latency_ms: 58, checked_at: isoAgo(52) }]
  }
];

const fleetDataset: OpsDataset = {
  host: {
    cpu_percent: 18.8,
    memory_bytes: 6.2 * GB,
    inbound_bps: 210 * MB,
    outbound_bps: 186 * MB,
    month: {
      rx_bytes: 3.1 * 1024 * GB,
      tx_bytes: 2.7 * 1024 * GB,
      total_bytes: 5.8 * 1024 * GB
    }
  },
  nodes,
  online: [
    { user_id: 101, username: "kepler", client_ip: "203.0.113.28", node_id: 11, first_seen_at: isoAgo(820), last_seen_at: isoAgo(8) },
    { user_id: 102, username: "linh", client_ip: "2001:db8:7f34:4::91", node_id: 12, first_seen_at: isoAgo(2400), last_seen_at: isoAgo(14) },
    { user_id: 103, username: "sato", client_ip: "198.51.100.44", node_id: 15, first_seen_at: isoAgo(124), last_seen_at: isoAgo(3) },
    { user_id: 104, username: "ops-reviewer-with-long-name", client_ip: "192.0.2.111", node_id: 11, first_seen_at: isoAgo(4400), last_seen_at: isoAgo(47) }
  ],
  probes: nodes.flatMap((node) => node.probes),
  alerts: [
    { id: 1, node_id: 15, kind: "disk_high", severity: "critical", status: "active", message: "Disk usage crossed 90%", last_seen_at: isoAgo(8) },
    { id: 2, node_id: 13, kind: "node_stale", severity: "warning", status: "active", message: "Runtime report stale", last_seen_at: isoAgo(338) },
    { id: 3, node_id: 12, kind: "version_drift", severity: "warning", status: "active", message: "Desired Xray config not applied", last_seen_at: isoAgo(18) },
    { id: 4, node_id: 12, kind: "probe_failed", severity: "warning", status: "active", message: "HTTP health probe returned 502", last_seen_at: isoAgo(210) }
  ],
  updated_at: isoAgo(2)
};

export function mockDataset(scenario: ScenarioName): OpsDataset {
  if (scenario === "empty") {
    return {
      ...fleetDataset,
      host: { cpu_percent: 0, memory_bytes: 0, inbound_bps: 0, outbound_bps: 0 },
      nodes: [],
      online: [],
      probes: [],
      alerts: [],
      updated_at: isoAgo(1)
    };
  }
  if (scenario === "error") {
    return {
      ...fleetDataset,
      alerts: [
        { id: 99, kind: "agent_metrics_missing", severity: "critical", status: "active", message: "Snapshot stream disconnected and polling failed", last_seen_at: isoAgo(1) }
      ],
      updated_at: isoAgo(1)
    };
  }
  return fleetDataset;
}

export function jitterDataset(input: OpsDataset): OpsDataset {
  const now = new Date().toISOString();
  const nextNodes = input.nodes.map((node, index) => {
    if (!node.agent_metrics) return node;
    const wave = Math.sin(Date.now() / 1800 + index);
    const cpu = Math.max(0, Math.min(99, node.agent_metrics.cpu_percent + wave * 2.2));
    const inbound = Math.max(0, node.agent_metrics.inbound_bps * (1 + wave * 0.025));
    const outbound = Math.max(0, node.agent_metrics.outbound_bps * (1 - wave * 0.02));
    return {
      ...node,
      last_seen_at: node.health === "stale" || node.health === "disabled" ? node.last_seen_at : now,
      agent_metrics_at: node.health === "stale" || node.health === "disabled" ? node.agent_metrics_at : now,
      agent_metrics: {
        ...node.agent_metrics,
        cpu_percent: cpu,
        inbound_bps: inbound,
        outbound_bps: outbound
      }
    };
  });
  return {
    ...input,
    nodes: nextNodes,
    host: {
      ...input.host,
      cpu_percent: Math.max(0, input.host.cpu_percent + Math.sin(Date.now() / 2200) * 1.4),
      inbound_bps: input.host.inbound_bps * (1 + Math.sin(Date.now() / 1900) * 0.02),
      outbound_bps: input.host.outbound_bps * (1 + Math.cos(Date.now() / 2100) * 0.02)
    },
    updated_at: now
  };
}
