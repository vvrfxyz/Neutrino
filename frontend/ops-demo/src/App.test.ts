import { describe, expect, it } from "vitest";
import { mergeLiveDataset } from "./adapters/liveAdapter";
import { mockDataset } from "./adapters/mockAdapter";
import { fleetCounts, formatBytes, nodeHasDrift } from "./utils";

describe("ops demo data model", () => {
  it("includes the review states required by the plan", () => {
    const data = mockDataset("fleet");
    const states = new Set(data.nodes.map((node) => node.health));
    expect(states.has("online")).toBe(true);
    expect(states.has("stale")).toBe(true);
    expect(states.has("disabled")).toBe(true);
    expect(states.has("error")).toBe(true);
    expect(data.nodes.some(nodeHasDrift)).toBe(true);
  });

  it("returns a true empty state", () => {
    const data = mockDataset("empty");
    expect(data.nodes).toHaveLength(0);
    expect(data.online).toHaveLength(0);
    expect(data.alerts).toHaveLength(0);
  });

  it("summarizes fleet state without dropping error or drift nodes", () => {
    const counts = fleetCounts(mockDataset("fleet").nodes);
    expect(counts.total).toBeGreaterThan(4);
    expect(counts.error).toBeGreaterThan(0);
    expect(counts.drift).toBeGreaterThan(0);
  });

  it("formats bytes with bounded precision", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1024)).toBe("1.00 KB");
  });

  it("preserves enriched node details when a websocket snapshot is summary-only", () => {
    const previous = mockDataset("fleet");
    const node = previous.nodes.find((item) => item.samples.length > 0 && item.probes.length > 0);
    expect(node).toBeTruthy();
    const incoming = {
      ...previous,
      alerts: [],
      probes: [],
      nodes: [{ ...node!, samples: [], probes: [], metadata: undefined, static_facts: undefined }]
    };
    const merged = mergeLiveDataset(previous, incoming, { preserveAlerts: true, preserveEnrichment: true });
    expect(merged.nodes[0].samples.length).toBeGreaterThan(0);
    expect(merged.nodes[0].probes.length).toBeGreaterThan(0);
    expect(merged.alerts.length).toBe(previous.alerts.length);
  });
});
