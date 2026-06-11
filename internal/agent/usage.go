package agent

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Usage pipeline: durable queue + flush-first.
func (a *Agent) startUsageLoop(ctx context.Context) {
	statsInterval := time.Duration(a.cfg.StatsPollSec) * time.Second
	if statsInterval < time.Second {
		statsInterval = 5 * time.Second
	}
	accessInterval := time.Duration(a.cfg.AccessPollSec) * time.Second
	if accessInterval < time.Second {
		accessInterval = 2 * time.Second
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	lastStatsAt := time.Time{}
	lastAccessAt := time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 1) Flush queued batches first (try multiple items per tick to catch up faster).
			flushed := 0
			for flushed < 8 {
				advanced, ok := a.flushQueuedBatchOnce(ctx)
				if !ok {
					break
				}
				if advanced {
					flushed++
				}
			}
			if flushed > 0 {
				continue
			}
			if !shouldGenerateUsageBatches(flushed, a.queue) {
				continue
			}

			// 2) Generate due batches only when queue is empty.
			// We may enqueue both access and stats in the same tick; this prevents
			// access batches from starving stats on busy nodes while still keeping
			// the stronger invariant that we never sample fresh usage while any
			// older batch remains pending in the queue.
			now := time.Now()
			var enqueued int
			lastAccessAt, lastStatsAt, enqueued = enqueueDueUsageBatches(
				now,
				lastAccessAt,
				lastStatsAt,
				accessInterval,
				statsInterval,
				func() (UsageBatch, bool) { return a.buildAccessBatch(ctx) },
				func() (UsageBatch, bool) { return a.buildStatsBatch(ctx) },
				a.queue.Enqueue,
			)
			if enqueued > 0 {
				continue
			}
		}
	}
}

// flushQueuedBatchOnce attempts to push the oldest queued batch to the panel.
// Returns (advanced, keepGoing): advanced is true when a batch was flushed or
// quarantined (the queue head moved), keepGoing is false when the flush loop
// should stop for this tick (queue empty, transport error, or panel missing).
//
// Poison-batch isolation: a corrupt batch file is quarantined immediately; a
// batch the panel keeps rejecting (deterministic per content, see
// UsageRejectionError) is quarantined after cfg.UsageQuarantineAfterRejects
// consecutive rejections. Without this, a single poison batch at the queue
// head halts the node's entire usage pipeline forever, because flush-first
// also stops fresh sampling while anything is queued.
func (a *Agent) flushQueuedBatchOnce(ctx context.Context) (bool, bool) {
	path, batch, ok, err := a.queue.PeekOldest()
	if err != nil {
		if errors.Is(err, ErrBatchCorrupt) && path != "" {
			a.quarantineBatch(path, err.Error())
			return true, true
		}
		log.Printf("queue peek failed: %v", err)
		return false, false
	}
	if !ok {
		return false, false
	}
	pc := a.panelClient()
	if pc == nil {
		return false, false
	}
	if err := pc.PushUsage(ctx, batch.Events); err != nil {
		log.Printf("push queued usage failed: %v", err)
		var rejection *UsageRejectionError
		if errors.As(err, &rejection) {
			if a.noteUsageRejection(path) {
				// The panel durably commits every acceptable event of a
				// rejected batch before reporting per-event errors, so the
				// acks must advance exactly as on success — otherwise the
				// next sample re-emits the same byte ranges under fresh
				// event ids (ids embed the absolute counters) and the panel
				// double-counts them.
				a.commitBatchAcks(batch)
				a.quarantineBatch(path, rejection.Error())
				return true, true
			}
		}
		return false, false
	}
	a.clearUsageRejections(path)
	a.commitBatchAcks(batch)
	if err := a.queue.Dequeue(path); err != nil {
		log.Printf("queue dequeue failed: %v", err)
		return false, false
	}
	return true, true
}

// commitBatchAcks advances the persisted ack state (stats counters/epoch or
// access offset) for a batch that has left the queue.
func (a *Agent) commitBatchAcks(batch UsageBatch) {
	if a.state == nil {
		return
	}
	_ = a.state.Update(func(s *State) {
		switch batch.Kind {
		case "stats":
			if batch.Stats != nil {
				if s.AckedStats == nil {
					s.AckedStats = make(map[string]StatEntry)
				}
				for k, v := range batch.Stats.NextAck {
					s.AckedStats[k] = v
				}
				s.StatsEpoch = batch.Stats.Epoch
			}
		case "access":
			if batch.Access != nil {
				s.Access.Path = batch.Access.Path
				s.Access.Inode = batch.Access.Inode
				s.Access.Offset = batch.Access.Offset
			}
		}
	})
}

type usageRejectionState struct {
	count   int
	firstAt time.Time
}

// noteUsageRejection bumps the consecutive-rejection counter for a batch path
// and reports whether the quarantine threshold has been reached. Quarantine
// requires both N rejections and a minimum wall-clock age since the first
// rejection (USAGE_QUARANTINE_MIN_AGE_SEC) so conditions that self-heal —
// like an unknown panel error string for a transient state — do not cost a
// batch of real usage data within seconds.
func (a *Agent) noteUsageRejection(path string) bool {
	threshold := a.cfg.UsageQuarantineAfterRejects
	if threshold <= 0 {
		threshold = 5
	}
	minAge := time.Duration(a.cfg.UsageQuarantineMinAgeSec) * time.Second
	if minAge < 0 {
		minAge = 0
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usageRejections == nil {
		a.usageRejections = make(map[string]usageRejectionState)
	}
	st := a.usageRejections[path]
	if st.count == 0 {
		st.firstAt = now
	}
	st.count++
	a.usageRejections[path] = st
	return st.count >= threshold && now.Sub(st.firstAt) >= minAge
}

func (a *Agent) clearUsageRejections(path string) {
	a.mu.Lock()
	if a.usageRejections != nil {
		delete(a.usageRejections, path)
	}
	a.mu.Unlock()
}

// quarantineBatch moves a poison batch out of the active queue. Events the
// panel rejected are permanently lost to accounting (the file is kept on disk
// for inspection); events it had already accepted were counted and their acks
// committed by the caller. This is loud: the quarantine count also reaches
// the panel via heartbeat metrics and raises a critical ops alert there.
func (a *Agent) quarantineBatch(path, reason string) {
	a.clearUsageRejections(path)
	dest, err := a.queue.Quarantine(path)
	if err != nil {
		log.Printf("quarantine usage batch failed path=%s: %v", path, err)
		return
	}
	log.Printf("quarantined poison usage batch path=%s dest=%s reason=%s", path, dest, reason)
}

func shouldGenerateUsageBatches(flushed int, q interface{ ApproxBatches() int64 }) bool {
	if flushed > 0 {
		return false
	}
	return !queueHasPending(q)
}

func queueHasPending(q interface{ ApproxBatches() int64 }) bool {
	if q == nil {
		return false
	}
	return q.ApproxBatches() > 0
}

type usageBatchBuilder func() (UsageBatch, bool)
type usageBatchEnqueuer func(UsageBatch) error

func enqueueDueUsageBatches(
	now time.Time,
	lastAccessAt time.Time,
	lastStatsAt time.Time,
	accessInterval time.Duration,
	statsInterval time.Duration,
	buildAccess usageBatchBuilder,
	buildStats usageBatchBuilder,
	enqueue usageBatchEnqueuer,
) (time.Time, time.Time, int) {
	enqueued := 0

	if usagePollDue(lastAccessAt, now, accessInterval) {
		if batch, ok := buildAccess(); ok {
			if err := enqueue(batch); err != nil {
				log.Printf("enqueue access batch failed: %v", err)
			} else {
				lastAccessAt = now
				enqueued++
			}
		} else {
			lastAccessAt = now
		}
	}

	if usagePollDue(lastStatsAt, now, statsInterval) {
		if batch, ok := buildStats(); ok {
			if err := enqueue(batch); err != nil {
				log.Printf("enqueue stats batch failed: %v", err)
			} else {
				lastStatsAt = now
				enqueued++
			}
		} else {
			lastStatsAt = now
		}
	}

	return lastAccessAt, lastStatsAt, enqueued
}

func usagePollDue(last time.Time, now time.Time, interval time.Duration) bool {
	return last.IsZero() || now.Sub(last) >= interval
}

type statsCounterSample struct {
	UserID  int64
	Email   string
	Acked   StatEntry
	Current StatEntry
}

func planStatsBatch(nodeID int64, samples []statsCounterSample, baseEpoch int64, maxEvents int, now time.Time) UsageBatch {
	epochTarget := baseEpoch
	for _, sample := range samples {
		if sample.Current.Uplink < sample.Acked.Uplink || sample.Current.Downlink < sample.Acked.Downlink {
			epochTarget++
			break
		}
	}

	events := make([]UsageEvent, 0, len(samples)*2)
	nextAck := make(map[string]StatEntry, len(samples))

	for _, sample := range samples {
		if sample.UserID <= 0 || strings.TrimSpace(sample.Email) == "" {
			continue
		}

		ackedToCommit := sample.Acked
		upDelta, upRebase := deltaValue(sample.Acked.Uplink, sample.Current.Uplink)
		downDelta, downRebase := deltaValue(sample.Acked.Downlink, sample.Current.Downlink)

		if upDelta == 0 {
			ackedToCommit.Uplink = upRebase
		}
		if downDelta == 0 {
			ackedToCommit.Downlink = downRebase
		}

		if upDelta > 0 && statsBatchHasCapacity(len(events), maxEvents) {
			nodeIDCopy := nodeID
			events = append(events, UsageEvent{
				UserID:        sample.UserID,
				NodeID:        &nodeIDCopy,
				Direction:     "inbound",
				Bytes:         upDelta,
				Source:        "xray-stats",
				SourceEventID: fmt.Sprintf("xray-stats:%d:%s:e%d:uplink:%d", nodeID, sample.Email, epochTarget, sample.Current.Uplink),
				At:            now.Format(time.RFC3339),
			})
			ackedToCommit.Uplink = sample.Current.Uplink
		}
		if downDelta > 0 && statsBatchHasCapacity(len(events), maxEvents) {
			nodeIDCopy := nodeID
			events = append(events, UsageEvent{
				UserID:        sample.UserID,
				NodeID:        &nodeIDCopy,
				Direction:     "outbound",
				Bytes:         downDelta,
				Source:        "xray-stats",
				SourceEventID: fmt.Sprintf("xray-stats:%d:%s:e%d:downlink:%d", nodeID, sample.Email, epochTarget, sample.Current.Downlink),
				At:            now.Format(time.RFC3339),
			})
			ackedToCommit.Downlink = sample.Current.Downlink
		}

		if ackedToCommit != sample.Acked {
			nextAck[sample.Email] = ackedToCommit
		}
	}

	return UsageBatch{
		Kind:      "stats",
		CreatedAt: now.Format(time.RFC3339),
		Events:    events,
		Stats:     &StatsMeta{NextAck: nextAck, Epoch: epochTarget},
	}
}

func statsBatchHasCapacity(currentEvents int, maxEvents int) bool {
	return maxEvents <= 0 || currentEvents < maxEvents
}

func (a *Agent) buildStatsBatch(ctx context.Context) (UsageBatch, bool) {
	snap := a.state.Snapshot()
	users := snap.SyncedUsers
	if len(users) == 0 {
		return UsageBatch{}, false
	}

	samples := make([]statsCounterSample, 0, len(users))

	for _, u := range users {
		if strings.TrimSpace(u.Email) == "" || u.Status != "active" {
			continue
		}
		if u.UserID <= 0 {
			continue
		}
		uplinkCurr, downlinkCurr, err := a.xray.PullUserTraffic(ctx, u.Email)
		if err != nil {
			continue
		}
		samples = append(samples, statsCounterSample{
			UserID:  u.UserID,
			Email:   u.Email,
			Acked:   snap.AckedStats[u.Email],
			Current: StatEntry{Uplink: uplinkCurr, Downlink: downlinkCurr},
		})
	}

	batch := planStatsBatch(a.cfg.NodeID, samples, snap.StatsEpoch, a.cfg.PushBatchMaxEvents, time.Now().UTC())
	if len(batch.Events) == 0 {
		if batch.Stats != nil && (len(batch.Stats.NextAck) > 0 || batch.Stats.Epoch != snap.StatsEpoch) {
			// Persist epoch/counter-only acknowledgements (for example, xray counter
			// resets with zero delta) even when there is nothing to send to panel.
			_ = a.state.Update(func(s *State) {
				if s.AckedStats == nil {
					s.AckedStats = make(map[string]StatEntry)
				}
				for k, v := range batch.Stats.NextAck {
					s.AckedStats[k] = v
				}
				s.StatsEpoch = batch.Stats.Epoch
			})
		}
		return UsageBatch{}, false
	}

	return batch, true
}

func deltaValue(last, current int64) (delta, rebaseTo int64) {
	if current < 0 {
		return 0, 0
	}
	if current >= last {
		return current - last, current
	}
	return current, current
}

func (a *Agent) buildAccessBatch(ctx context.Context) (UsageBatch, bool) {
	path := strings.TrimSpace(a.cfg.XrayAccessLogPath)
	if path == "" {
		return UsageBatch{}, false
	}

	snap := a.state.Snapshot()
	users := snap.SyncedUsers
	if len(users) == 0 {
		return UsageBatch{}, false
	}
	userIDByEmail := make(map[string]int64, len(users))
	for _, u := range users {
		if u.UserID > 0 && strings.TrimSpace(u.Email) != "" {
			userIDByEmail[u.Email] = u.UserID
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return UsageBatch{}, false
	}
	inode := fileInode(info)

	access := snap.Access
	if strings.TrimSpace(access.Path) == "" {
		access.Path = path
	}
	if access.Path != path {
		access.Path = path
		access.Inode = 0
		access.Offset = 0
	}
	if access.Inode != 0 && inode != 0 && inode != access.Inode {
		access.Offset = 0
	}
	if info.Size() < access.Offset {
		access.Offset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return UsageBatch{}, false
	}
	defer f.Close()
	if _, err := f.Seek(access.Offset, 0); err != nil {
		return UsageBatch{}, false
	}

	maxLines := 300
	if a.cfg.PushBatchMaxEvents > 0 && a.cfg.PushBatchMaxEvents < maxLines {
		maxLines = a.cfg.PushBatchMaxEvents
	}

	reader := bufio.NewReader(f)
	cur := access.Offset
	events := make([]UsageEvent, 0, 32)
	onlineEvents := make(map[string]UsageEvent)
	newOffset := cur

	for len(events) < maxLines {
		line, readErr := reader.ReadString('\n')
		if len(line) == 0 {
			if readErr != nil {
				break
			}
			continue
		}
		start := cur
		cur += int64(len(line))
		newOffset = cur

		evt, ok := parseAccessLine(path, inode, start, strings.TrimSpace(line), userIDByEmail, a.cfg.NodeID, a.cfg.AccessLogLocation())
		if ok {
			// Zero-byte sampling/limit.
			if a.sampler.Allow(evt.UserID, evt.Destination, time.Now()) {
				events = append(events, evt)
			} else if onlineEvt, onlineKey, ok := compactAccessOnlineEvent(evt, a.cfg.NodeID); ok {
				onlineEvents[onlineKey] = onlineEvt
			}
		}
		if readErr != nil {
			break
		}
	}
	if len(onlineEvents) > 0 {
		keys := make([]string, 0, len(onlineEvents))
		for k := range onlineEvents {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			events = append(events, onlineEvents[k])
		}
	}

	if len(events) == 0 {
		// Advance the cursor even when this read window only contained
		// unmatched, inactive-user, or sampled-out lines.
		if newOffset != access.Offset || access.Inode == 0 || access.Path != path {
			_ = a.state.Update(func(s *State) {
				s.Access.Path = path
				s.Access.Inode = inode
				s.Access.Offset = newOffset
			})
		}
		return UsageBatch{}, false
	}

	return UsageBatch{
		Kind:      "access",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Events:    events,
		Access:    &AccessMeta{Path: path, Inode: inode, Offset: newOffset},
	}, true
}

func fileInode(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		return uint64(st.Ino)
	}
	return 0
}

var (
	reAccessEmail = regexp.MustCompile(`\bemail:\s*([^\s]+)`)
	reAccessDest  = regexp.MustCompile(`\b(tcp|udp):(\[[^\]]+\]|[^\s:]+):([0-9]{1,5})`)
	reAccessRoute = regexp.MustCompile(`\[([^\s\]]+)\s*>>\s*([^\s\]]+)\]`)
	reAccessSNI   = regexp.MustCompile(`\bsni:\s*([^\s]+)`)
	reAccessFrom  = regexp.MustCompile(`\bfrom\s+(?:\[([^\]]+)\]|([^\s:]+)):[0-9]+`)
)

func extractEventTime(line string, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	if len(line) >= 19 {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], loc); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

func parseAccessLine(path string, inode uint64, startOffset int64, line string, userIDByEmail map[string]int64, nodeID int64, loc *time.Location) (UsageEvent, bool) {
	if strings.TrimSpace(line) == "" {
		return UsageEvent{}, false
	}
	emailMatch := reAccessEmail.FindStringSubmatch(line)
	if len(emailMatch) < 2 {
		return UsageEvent{}, false
	}
	email := strings.TrimSpace(emailMatch[1])
	userID := userIDByEmail[email]
	if userID <= 0 {
		return UsageEvent{}, false
	}
	destMatch := reAccessDest.FindStringSubmatch(line)
	if len(destMatch) < 4 {
		return UsageEvent{}, false
	}

	proto := destMatch[1]
	host := strings.Trim(destMatch[2], "[]")
	port, _ := strconv.ParseInt(destMatch[3], 10, 64)

	targetHost := ""
	targetIP := ""
	if ip := net.ParseIP(host); ip == nil {
		targetHost = host
	} else {
		targetIP = host
	}

	inboundTag := ""
	if routeMatch := reAccessRoute.FindStringSubmatch(line); len(routeMatch) >= 2 {
		inboundTag = strings.TrimSpace(routeMatch[1])
	}

	clientIP := ""
	if fromMatch := reAccessFrom.FindStringSubmatch(line); len(fromMatch) >= 2 {
		switch {
		case strings.TrimSpace(fromMatch[1]) != "":
			clientIP = strings.TrimSpace(fromMatch[1])
		case len(fromMatch) >= 3:
			clientIP = strings.TrimSpace(fromMatch[2])
		}
	}

	sni := ""
	if sniMatch := reAccessSNI.FindStringSubmatch(line); len(sniMatch) >= 2 {
		sni = strings.TrimSpace(sniMatch[1])
	}
	if sni == "" && proto == "tcp" && targetHost != "" {
		sni = targetHost
	}

	eventAt := extractEventTime(line, loc)

	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d|%d|%s", path, inode, startOffset, len(line), line)))
	eventID := hex.EncodeToString(sum[:])
	nid := nodeID

	return UsageEvent{
		UserID:    userID,
		NodeID:    &nid,
		Direction: "outbound",
		Bytes:     0,
		Source:    "xray-access",
		// Include node_id to avoid cross-node id collisions in panel idempotency keying.
		SourceEventID: fmt.Sprintf("xray-access:%d:%s", nodeID, eventID),
		TargetHost:    targetHost,
		TargetIP:      targetIP,
		TargetPort:    port,
		SNI:           sni,
		Destination:   proto + ":" + host + ":" + destMatch[3],
		ClientIP:      clientIP,
		InboundTag:    inboundTag,
		At:            eventAt.UTC().Format(time.RFC3339),
	}, true
}

func compactAccessOnlineEvent(evt UsageEvent, nodeID int64) (UsageEvent, string, bool) {
	clientIP := strings.TrimSpace(evt.ClientIP)
	if evt.UserID <= 0 || clientIP == "" {
		return UsageEvent{}, "", false
	}
	eventAt, err := time.Parse(time.RFC3339, evt.At)
	if err != nil {
		eventAt = time.Now().UTC()
	}
	minute := eventAt.UTC().Truncate(time.Minute)
	key := fmt.Sprintf("%d|%s|%s", evt.UserID, clientIP, minute.Format("200601021504"))
	sum := sha1.Sum([]byte(fmt.Sprintf("%d|%d|%s|%s", nodeID, evt.UserID, clientIP, minute.Format(time.RFC3339))))
	nid := nodeID
	return UsageEvent{
		UserID:        evt.UserID,
		NodeID:        &nid,
		Direction:     "outbound",
		Bytes:         0,
		Source:        "xray-access",
		SourceEventID: fmt.Sprintf("xray-access-online:%d:%s", nodeID, hex.EncodeToString(sum[:])),
		TargetHost:    evt.TargetHost,
		TargetIP:      evt.TargetIP,
		TargetPort:    evt.TargetPort,
		SNI:           evt.SNI,
		Destination:   evt.Destination,
		ClientIP:      clientIP,
		InboundTag:    evt.InboundTag,
		At:            minute.Format(time.RFC3339),
	}, key, true
}
