package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type HostNetMonthlyUsage struct {
	MonthKey  string    `json:"month_key"`
	RXBytes   int64     `json:"rx_bytes"`
	TXBytes   int64     `json:"tx_bytes"`
	UpdatedAt time.Time `json:"updated_at"`
}

func monthKeyLocal(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01")
}

func (s *Store) GetHostNetMonthlyUsage(ctx context.Context, at time.Time) (HostNetMonthlyUsage, bool, error) {
	key := monthKeyLocal(at, s.cfg.PanelLocation())
	var rx, tx int64
	var updatedRaw string
	err := s.db.QueryRowContext(ctx, `
SELECT rx_bytes, tx_bytes, updated_at
FROM host_net_monthly_usage
WHERE month_key = ?;
`, key).Scan(&rx, &tx, &updatedRaw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HostNetMonthlyUsage{}, false, nil
		}
		return HostNetMonthlyUsage{}, false, err
	}
	updatedAt, _ := time.Parse(time.RFC3339, updatedRaw)
	return HostNetMonthlyUsage{
		MonthKey:  key,
		RXBytes:   rx,
		TXBytes:   tx,
		UpdatedAt: updatedAt,
	}, true, nil
}

// RecordHostNetTotals updates natural-month RX/TX usage based on system-level monotonic counters.
// It is resilient to process restarts; reboot/counter reset is detected via negative deltas.
func (s *Store) RecordHostNetTotals(ctx context.Context, rxTotal, txTotal int64, source string, at time.Time) error {
	if rxTotal < 0 || txTotal < 0 {
		return nil
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	now := at.UTC()
	nowStr := now.Format(time.RFC3339)
	curMonth := monthKeyLocal(at, s.cfg.PanelLocation())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	const (
		keyLastRX  = "host_net_last_rx_total"
		keyLastTX  = "host_net_last_tx_total"
		keyLastMon = "host_net_last_month_key"
		keySource  = "host_net_counter_source"
	)

	lastMonth, hasMonth, err := s.getXrayStateTx(ctx, tx, keyLastMon)
	if err != nil {
		return err
	}
	lastRX, hasRX, err := s.getXrayStateInt64Tx(ctx, tx, keyLastRX)
	if err != nil {
		return err
	}
	lastTX, hasTX, err := s.getXrayStateInt64Tx(ctx, tx, keyLastTX)
	if err != nil {
		return err
	}
	lastSource, hasSource, err := s.getXrayStateTx(ctx, tx, keySource)
	if err != nil {
		return err
	}

	// First sample or month rollover: set baseline, ensure month row exists.
	if !hasMonth || !hasRX || !hasTX || !hasSource || lastMonth != curMonth || lastSource != source {
		if err := s.setXrayStateTx(ctx, tx, keyLastMon, curMonth, now); err != nil {
			return err
		}
		if err := s.setXrayStateInt64Tx(ctx, tx, keyLastRX, rxTotal, now); err != nil {
			return err
		}
		if err := s.setXrayStateInt64Tx(ctx, tx, keyLastTX, txTotal, now); err != nil {
			return err
		}
		if err := s.setXrayStateTx(ctx, tx, keySource, source, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO host_net_monthly_usage(month_key, rx_bytes, tx_bytes, updated_at)
VALUES (?, 0, 0, ?)
ON CONFLICT(month_key) DO NOTHING;
`, curMonth, nowStr)
		if err != nil {
			return err
		}
		return tx.Commit()
	}

	deltaRX := rxTotal - lastRX
	deltaTX := txTotal - lastTX
	if deltaRX < 0 || deltaTX < 0 {
		// System reboot or counter reset: reset baseline, don't accumulate.
		_ = s.setXrayStateInt64Tx(ctx, tx, keyLastRX, rxTotal, now)
		_ = s.setXrayStateInt64Tx(ctx, tx, keyLastTX, txTotal, now)
		_ = s.setXrayStateTx(ctx, tx, keySource, source, now)
		return tx.Commit()
	}
	if deltaRX == 0 && deltaTX == 0 {
		return tx.Commit()
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO host_net_monthly_usage(month_key, rx_bytes, tx_bytes, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(month_key) DO UPDATE SET
	rx_bytes = rx_bytes + excluded.rx_bytes,
	tx_bytes = tx_bytes + excluded.tx_bytes,
	updated_at = excluded.updated_at;
`, curMonth, deltaRX, deltaTX, nowStr)
	if err != nil {
		return err
	}
	if err := s.setXrayStateInt64Tx(ctx, tx, keyLastRX, rxTotal, now); err != nil {
		return err
	}
	if err := s.setXrayStateInt64Tx(ctx, tx, keyLastTX, txTotal, now); err != nil {
		return err
	}
	if err := s.setXrayStateTx(ctx, tx, keySource, source, now); err != nil {
		return err
	}
	return tx.Commit()
}
