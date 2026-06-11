package service

import (
	"context"
	"crypto/x509"
	"time"

	"neutrino/internal/repo"
)

// Control-plane methods used by the agent-facing HTTP handlers (report,
// job claim/finish, metadata, probes, metrics history) and the admin cert
// lifecycle endpoints. They are intentionally thin: orchestration stays in
// the service layer so handlers never touch repo.Store directly.

func (s *NodeService) ApplyReport(ctx context.Context, nodeID int64, in repo.NodeReportInput) (time.Time, error) {
	return s.store.ApplyNodeReportLatest(ctx, nodeID, in)
}

func (s *NodeService) UpdateObservedIP(ctx context.Context, nodeID int64, ip string) error {
	return s.store.UpdateNodeObservedIP(ctx, nodeID, ip)
}

func (s *NodeService) UpdateAgentStatus(ctx context.Context, nodeID int64, lastSeenAt *time.Time, errMsg string) error {
	return s.store.UpdateNodeAgentStatus(ctx, nodeID, lastSeenAt, errMsg)
}

func (s *NodeService) ClaimNextJob(ctx context.Context, nodeID int64, allowedKinds []string) (repo.NodeJob, bool, error) {
	return s.store.ClaimNextNodeJobForNodeKinds(ctx, nodeID, allowedKinds)
}

func (s *NodeService) FinishJob(ctx context.Context, nodeID, jobID int64, in repo.FinishNodeJobInput) (string, error) {
	return s.store.FinishNodeJobForNode(ctx, nodeID, jobID, in)
}

func (s *NodeService) GetJob(ctx context.Context, jobID int64) (repo.NodeJob, bool, error) {
	return s.store.GetNodeJob(ctx, jobID)
}

func (s *NodeService) SetAppliedUsersVersion(ctx context.Context, nodeID int64, version string) error {
	return s.store.SetNodeAppliedUsersVersion(ctx, nodeID, version)
}

func (s *NodeService) SetAppliedXrayVersion(ctx context.Context, nodeID int64, version string) error {
	return s.store.SetNodeAppliedXrayVersion(ctx, nodeID, version)
}

func (s *NodeService) MarkXrayRolledBack(ctx context.Context, nodeID int64, appliedVersion string) error {
	return s.store.MarkNodeXrayRolledBack(ctx, nodeID, appliedVersion)
}

func (s *NodeService) SetJobError(ctx context.Context, nodeID int64, jobKind, errKind, errMsg string) error {
	return s.store.SetNodeJobError(ctx, nodeID, jobKind, errKind, errMsg)
}

func (s *NodeService) InsertProbeResult(ctx context.Context, nodeID int64, in repo.InsertNodeProbeResultInput) (int64, error) {
	return s.store.InsertNodeProbeResult(ctx, nodeID, in)
}

func (s *NodeService) EnqueueProbeJob(ctx context.Context, nodeID int64, kind, payloadJSON string, timeoutSec int, correlationID string) (int64, bool, error) {
	return s.store.EnqueueNodeJob(ctx, nodeID, kind, "", payloadJSON, timeoutSec, correlationID)
}

func (s *NodeService) GetMetadata(ctx context.Context, nodeID int64) (repo.NodeMetadata, bool, error) {
	return s.store.GetNodeMetadata(ctx, nodeID)
}

func (s *NodeService) UpsertMetadata(ctx context.Context, nodeID int64, in repo.UpsertNodeMetadataInput) error {
	return s.store.UpsertNodeMetadata(ctx, nodeID, in)
}

func (s *NodeService) ListProbeResults(ctx context.Context, nodeID int64, limit int) ([]repo.NodeProbeResult, error) {
	return s.store.ListNodeProbeResults(ctx, nodeID, limit)
}

func (s *NodeService) ListMetricSeries(ctx context.Context, nodeID int64, rangeName, step string, now time.Time) ([]repo.NodeMetricSeriesPoint, error) {
	return s.store.ListNodeMetricSeries(ctx, nodeID, rangeName, step, now)
}

func (s *NodeService) GetLatestMetricDetails(ctx context.Context, nodeID int64) (repo.NodeMetricDetails, bool, error) {
	return s.store.GetLatestNodeMetricDetails(ctx, nodeID)
}

func (s *NodeService) GetLatestStaticFacts(ctx context.Context, nodeID int64) (repo.NodeStaticFacts, bool, error) {
	return s.store.GetLatestNodeStaticFacts(ctx, nodeID)
}

func (s *NodeService) ListStaticFacts(ctx context.Context, nodeID int64, limit int) ([]repo.NodeStaticFacts, error) {
	return s.store.ListNodeStaticFacts(ctx, nodeID, limit)
}

// Enrollment / cert pin lifecycle.

func (s *NodeService) CreateEnrollCode(ctx context.Context, nodeID int64, ttl time.Duration) (repo.NodeEnrollCode, error) {
	return s.store.CreateNodeEnrollCode(ctx, nodeID, ttl)
}

func (s *NodeService) GetEnrollCode(ctx context.Context, nodeID int64) (repo.NodeEnrollCode, bool, error) {
	return s.store.GetNodeEnrollCode(ctx, nodeID)
}

func (s *NodeService) ValidateEnrollCode(ctx context.Context, nodeID int64, code string) error {
	return s.store.ValidateNodeEnrollCode(ctx, nodeID, code)
}

func (s *NodeService) CompleteEnroll(ctx context.Context, nodeID int64, code string, cert *x509.Certificate, now time.Time) (int64, error) {
	return s.store.CompleteNodeEnroll(ctx, nodeID, code, cert, now)
}

func (s *NodeService) RenewCertPin(ctx context.Context, nodeID int64, purpose string, cert *x509.Certificate, now time.Time) (int64, error) {
	return s.store.RenewNodeCertPin(ctx, nodeID, purpose, cert, now)
}

func (s *NodeService) RevokeCertPin(ctx context.Context, nodeID int64, certSHA256, reason string) (int64, error) {
	return s.store.RevokeNodeCertPin(ctx, nodeID, certSHA256, reason)
}

func (s *NodeService) ListCertPins(ctx context.Context, nodeID int64, limit int) ([]repo.NodeCertPin, error) {
	return s.store.ListNodeCertPins(ctx, nodeID, limit)
}
