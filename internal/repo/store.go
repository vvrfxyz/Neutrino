package repo

import (
	"database/sql"
	"errors"
	"time"

	"neutrino/internal/config"
)

var (
	ErrUserNotFound               = errors.New("user not found")
	ErrUserInactive               = errors.New("user is not active")
	ErrUserNotAllowedOnNode       = errors.New("user not allowed on node")
	ErrUnsupportedTrafficPeriod   = errors.New("unsupported period")
	ErrNodeNotFound               = errors.New("node not found")
	ErrNodeDeleteWouldWidenAccess = errors.New("node delete would widen restricted user access")
	ErrNodeJobNotRunning          = errors.New("node job is not running for this attempt")
	ErrNodeJobInvalidStatus       = errors.New("node job finish status must be succeeded or failed")
	// ErrEnrollCodeInvalid covers every enroll-code rejection (missing, wrong,
	// already used, expired); the wrapping message carries the specific cause.
	ErrEnrollCodeInvalid    = errors.New("enroll code rejected")
	ErrUsageTimestampSkew   = errors.New("usage event timestamp outside allowed skew")
	ErrUsageTimestampTooOld = errors.New("usage event timestamp too old")
)

type Store struct {
	db  *sql.DB
	cfg config.Config
}

type User struct {
	ID                int64
	Username          string
	MonthlyLimitBytes int64
	CountingMode      string
	QuotaCycle        string
	QuotaTZ           string
	DeviceLimit       int
	PlanDays          int
	Status            string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	RemovedAt         *time.Time

	InboundBytes  int64
	OutboundBytes int64
	LastSeenAt    *time.Time

	LastTargetHost string
	LastTargetIP   string
	LastTargetPort *int64

	WindowStart         *time.Time
	WindowEnd           *time.Time
	WindowInboundBytes  int64
	WindowOutboundBytes int64
	WindowEffective     int64
	WindowCreditBytes   int64
	WindowCycleType     string
	WindowCycleKey      string
	WindowOverLimitAt   *time.Time
	WindowAlert80At     *time.Time
	WindowAlert90At     *time.Time

	ActiveLink         *ProxyLink
	OnlineSessionCount int
	OnlineDeviceCount  int
}

type ProxyLink struct {
	ID         int64
	UserID     int64
	Protocol   string
	UUID       string
	InboundTag string
	Link       string
	CreatedAt  time.Time
}

type UserEvent struct {
	ID            int64
	UserID        int64
	NodeID        *int64
	Direction     string
	Bytes         int64
	EventAt       time.Time
	Source        string
	SourceEventID string
	TargetHost    string
	TargetIP      string
	TargetPort    *int64
	SNI           string
	Destination   string
	ClientIP      string
	InboundTag    string
}

type TrafficBucket struct {
	BucketStart   time.Time `json:"bucket_start"`
	InboundBytes  int64     `json:"inbound_bytes"`
	OutboundBytes int64     `json:"outbound_bytes"`
}

type CreateUserInput struct {
	Username       string  `json:"username"`
	MonthlyLimitGB int64   `json:"monthly_limit_gb"`
	CountingMode   string  `json:"counting_mode"`
	QuotaCycle     string  `json:"quota_cycle"`
	QuotaTZ        string  `json:"quota_tz"`
	DeviceLimit    int     `json:"device_limit"`
	PlanDays       int     `json:"plan_days"`
	NodeIDs        []int64 `json:"node_ids,omitempty"`
}

type SubscriptionToken struct {
	ID         int64
	UserID     int64
	Token      string
	Enabled    bool
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

type Node struct {
	ID                  int64      `json:"id"`
	Name                string     `json:"name"`
	CoreType            string     `json:"core_type"`
	Protocol            string     `json:"protocol"`
	Host                string     `json:"host"`
	Port                int        `json:"port"`
	Transport           string     `json:"transport"`
	Security            string     `json:"security"`
	Flow                string     `json:"flow"`
	SNI                 string     `json:"sni"`
	FP                  string     `json:"fp"`
	PublicKey           string     `json:"public_key"`
	ShortID             string     `json:"short_id"`
	ExtraJSON           string     `json:"extra_json"`
	Enabled             bool       `json:"enabled"`
	Managed             bool       `json:"managed"`
	AgentURL            string     `json:"agent_url,omitempty"`
	LastSeenAt          *time.Time `json:"last_seen_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ObservedIP          string     `json:"observed_ip,omitempty"`
	DesiredUsersVersion string     `json:"desired_users_version,omitempty"`
	AppliedUsersVersion string     `json:"applied_users_version,omitempty"`
	DesiredXrayVersion  string     `json:"desired_xray_version,omitempty"`
	AppliedXrayVersion  string     `json:"applied_xray_version,omitempty"`
	LastJobErrorKind    string     `json:"last_job_error_kind,omitempty"`
	LastJobErrorAt      *time.Time `json:"last_job_error_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type CreateNodeInput struct {
	Name      string `json:"name"`
	CoreType  string `json:"core_type"`
	Protocol  string `json:"protocol"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Transport string `json:"transport"`
	Security  string `json:"security"`
	Flow      string `json:"flow"`
	SNI       string `json:"sni"`
	FP        string `json:"fp"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id"`
	ExtraJSON string `json:"extra_json"`
	Enabled   bool   `json:"enabled"`

	Managed  bool   `json:"managed"`
	AgentURL string `json:"agent_url"`
}

type APIKey struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scopes     string     `json:"scopes"`
	NodeID     *int64     `json:"node_id,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

type OnlineUser struct {
	UserID    int64     `json:"user_id"`
	Username  string    `json:"username"`
	ClientIP  string    `json:"client_ip"`
	NodeID    *int64    `json:"node_id,omitempty"`
	FirstSeen time.Time `json:"first_seen_at"`
	LastSeen  time.Time `json:"last_seen_at"`
}

type BackupRecord struct {
	ID        int64     `json:"id"`
	FilePath  string    `json:"file_path"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	Storage   string    `json:"storage"`
	ObjectKey string    `json:"object_key,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Alert struct {
	ID        int64      `json:"id"`
	UserID    *int64     `json:"user_id,omitempty"`
	Type      string     `json:"type"`
	Threshold string     `json:"threshold,omitempty"`
	Channel   string     `json:"channel"`
	Message   string     `json:"message"`
	DedupeKey string     `json:"dedupe_key"`
	SentAt    *time.Time `json:"sent_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type AlertListItem struct {
	ID        int64
	UserID    *int64
	Username  string
	Type      string
	Threshold string
	Channel   string
	Message   string
	DedupeKey string
	SentAt    *time.Time
	CreatedAt time.Time
}

type EnforcementLog struct {
	ID        int64
	UserID    int64
	Username  string
	Action    string
	Reason    string
	Detail    string
	CreatedAt time.Time
}

type UsageInput struct {
	UserID        int64     `json:"user_id"`
	NodeID        *int64    `json:"node_id,omitempty"`
	Direction     string    `json:"direction"`
	Bytes         int64     `json:"bytes"`
	At            time.Time `json:"at"`
	Source        string    `json:"source"`
	SourceEventID string    `json:"source_event_id"`
	TargetHost    string    `json:"target_host"`
	TargetIP      string    `json:"target_ip"`
	TargetPort    int64     `json:"target_port"`
	SNI           string    `json:"sni"`
	Destination   string    `json:"destination"`
	ClientIP      string    `json:"client_ip"`
	InboundTag    string    `json:"inbound_tag"`
}

var ErrDuplicateEvent = errors.New("duplicate event")

// nowRFC3339 returns the current UTC time in the canonical storage format.
// All timestamps in the DB are UTC RFC3339 TEXT (see CLAUDE.md).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *Store) RawDB() *sql.DB {
	return s.db
}

func New(db *sql.DB, cfg config.Config) *Store {
	return &Store{db: db, cfg: cfg}
}

const usersSelectQuery = `
SELECT
	u.id,
	u.username,
	u.monthly_limit_bytes,
	u.counting_mode,
	u.quota_cycle,
	u.quota_tz,
	u.device_limit,
	u.plan_days,
	u.status,
	u.created_at,
	u.expires_at,
	u.removed_at,
	COALESCE(ts.inbound_bytes, 0),
	COALESCE(ts.outbound_bytes, 0),
	ts.last_seen_at,
	ts.last_target_host,
	ts.last_target_ip,
	ts.last_target_port,
	qw.window_start,
	qw.window_end,
	qw.cycle_type,
	qw.cycle_key,
	COALESCE(qw.inbound_bytes, 0),
	COALESCE(qw.outbound_bytes, 0),
	COALESCE(qw.effective_bytes, 0),
	COALESCE(qw.credit_bytes, 0),
	qw.over_limit_at,
	qw.alert_80_sent_at,
	qw.alert_90_sent_at,
	pl.id,
	pl.protocol,
	pl.uuid,
	pl.inbound_tag,
	pl.link,
	pl.created_at
FROM users u
LEFT JOIN traffic_stats ts ON ts.user_id = u.id
LEFT JOIN quota_windows qw ON qw.id = (
	SELECT id
	FROM quota_windows
	WHERE user_id = u.id
	ORDER BY window_start DESC
	LIMIT 1
)
LEFT JOIN proxy_links pl ON pl.id = (
	SELECT id
	FROM proxy_links
	WHERE user_id = u.id AND active = 1
	ORDER BY id DESC
	LIMIT 1
)
`

func scanUserRow(rows *sql.Rows) (User, error) {
	var u User
	var (
		createdAt, expiresAt string
		removedAt            sql.NullString
		lastSeen             sql.NullString
		lastTargetHost       sql.NullString
		lastTargetIP         sql.NullString
		lastTargetPort       sql.NullInt64
		windowStart          sql.NullString
		windowEnd            sql.NullString
		windowCycleType      sql.NullString
		windowCycleKey       sql.NullString
		windowOverLimitAt    sql.NullString
		windowAlert80At      sql.NullString
		windowAlert90At      sql.NullString
		linkID               sql.NullInt64
		linkProtocol         sql.NullString
		linkUUID             sql.NullString
		inboundTag           sql.NullString
		linkURL              sql.NullString
		linkCreatedAt        sql.NullString
	)

	if err := rows.Scan(
		&u.ID,
		&u.Username,
		&u.MonthlyLimitBytes,
		&u.CountingMode,
		&u.QuotaCycle,
		&u.QuotaTZ,
		&u.DeviceLimit,
		&u.PlanDays,
		&u.Status,
		&createdAt,
		&expiresAt,
		&removedAt,
		&u.InboundBytes,
		&u.OutboundBytes,
		&lastSeen,
		&lastTargetHost,
		&lastTargetIP,
		&lastTargetPort,
		&windowStart,
		&windowEnd,
		&windowCycleType,
		&windowCycleKey,
		&u.WindowInboundBytes,
		&u.WindowOutboundBytes,
		&u.WindowEffective,
		&u.WindowCreditBytes,
		&windowOverLimitAt,
		&windowAlert80At,
		&windowAlert90At,
		&linkID,
		&linkProtocol,
		&linkUUID,
		&inboundTag,
		&linkURL,
		&linkCreatedAt,
	); err != nil {
		return User{}, err
	}

	var err error
	u.CreatedAt, err = time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return User{}, err
	}
	u.ExpiresAt, err = time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return User{}, err
	}
	if removedAt.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, removedAt.String)
		if parseErr == nil {
			u.RemovedAt = &parsed
		}
	}
	if lastSeen.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, lastSeen.String)
		if parseErr == nil {
			u.LastSeenAt = &parsed
		}
	}
	u.LastTargetHost = lastTargetHost.String
	u.LastTargetIP = lastTargetIP.String
	if lastTargetPort.Valid {
		v := lastTargetPort.Int64
		u.LastTargetPort = &v
	}

	if windowStart.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, windowStart.String)
		if parseErr == nil {
			u.WindowStart = &parsed
		}
	}
	if windowEnd.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, windowEnd.String)
		if parseErr == nil {
			u.WindowEnd = &parsed
		}
	}
	if windowOverLimitAt.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, windowOverLimitAt.String)
		if parseErr == nil {
			u.WindowOverLimitAt = &parsed
		}
	}
	u.WindowCycleType = windowCycleType.String
	u.WindowCycleKey = windowCycleKey.String
	if windowAlert80At.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, windowAlert80At.String)
		if parseErr == nil {
			u.WindowAlert80At = &parsed
		}
	}
	if windowAlert90At.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, windowAlert90At.String)
		if parseErr == nil {
			u.WindowAlert90At = &parsed
		}
	}

	if linkID.Valid {
		created, parseErr := time.Parse(time.RFC3339, linkCreatedAt.String)
		if parseErr != nil {
			return User{}, parseErr
		}
		u.ActiveLink = &ProxyLink{
			ID:         linkID.Int64,
			UserID:     u.ID,
			Protocol:   linkProtocol.String,
			UUID:       linkUUID.String,
			InboundTag: inboundTag.String,
			Link:       linkURL.String,
			CreatedAt:  created,
		}
	}

	return u, nil
}
