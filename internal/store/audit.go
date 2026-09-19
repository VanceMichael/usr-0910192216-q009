package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"example.com/culture-sharing/internal/ledger"
)

// Participant 是一个 API 主体（参与方、审计员或管理员）。
type Participant struct {
	ID          string
	Kind        string
	DisplayName string
}

// ParticipantByTokenHash 按 token 的 sha256 查找主体。
func (s *Store) ParticipantByTokenHash(ctx context.Context, hash []byte) (Participant, error) {
	var p Participant
	err := s.Pool.QueryRow(ctx,
		`SELECT id, kind, display_name FROM participants WHERE token_hash = $1`, hash).
		Scan(&p.ID, &p.Kind, &p.DisplayName)
	return p, err
}

// AllEvents 按审计序号返回全部事件（重放与链校验用）。
func (s *Store) AllEvents(ctx context.Context) ([]ledger.Event, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+eventColumns+` FROM events ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	return collectEvents(rows)
}

// ListEvents 返回 [fromSeq, toSeq] 区间的事件。
func (s *Store) ListEvents(ctx context.Context, fromSeq, toSeq int64) ([]ledger.Event, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+eventColumns+` FROM events
		WHERE seq >= $1 AND seq <= $2 ORDER BY seq`, fromSeq, toSeq)
	if err != nil {
		return nil, err
	}
	return collectEvents(rows)
}

// ChainHead 返回链状态（最大审计序号与链头哈希）。
func (s *Store) ChainHead(ctx context.Context) (int64, []byte, error) {
	var seq int64
	var hash []byte
	err := s.Pool.QueryRow(ctx,
		`SELECT last_seq, last_hash FROM chain_state WHERE id = 1`).Scan(&seq, &hash)
	return seq, hash, err
}

// StoredBalances 返回截至 asOfSeq 已确认结算产生的各参与方净额（贷-借，分）。
func (s *Store) StoredBalances(ctx context.Context, asOfSeq int64) (map[string]int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT p.participant_id,
		COALESCE(SUM(p.credit_cents), 0) - COALESCE(SUM(p.debit_cents), 0)
		FROM postings p JOIN settlements st ON st.id = p.settlement_id
		WHERE st.confirmation_seq <= $1
		GROUP BY p.participant_id`, asOfSeq)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	defer rows.Close()
	for rows.Next() {
		var id string
		var net int64
		if err := rows.Scan(&id, &net); err != nil {
			return nil, err
		}
		out[id] = net
	}
	return out, rows.Err()
}

// WatermarkRow 是一个区域的水位。
type WatermarkRow struct {
	RegionCode   string      `json:"region_code"`
	ClosedPeriod ledger.Date `json:"closed_period"`
	HighSeq      int64       `json:"high_seq"`
}

// Watermarks 返回全部区域水位。
func (s *Store) Watermarks(ctx context.Context) ([]WatermarkRow, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT region_code, to_char(closed_period, 'YYYY-MM-DD'), high_seq FROM watermarks ORDER BY region_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WatermarkRow
	for rows.Next() {
		var w WatermarkRow
		var closed string
		if err := rows.Scan(&w.RegionCode, &closed, &w.HighSeq); err != nil {
			return nil, err
		}
		w.ClosedPeriod = ledger.Date(closed)
		out = append(out, w)
	}
	return out, rows.Err()
}

// StatementRow 是参与方对账单的一行（只含本人金额与公开依据）。
type StatementRow struct {
	SettlementID int64
	RegionCode   string
	Period       ledger.Date
	Leg          string
	Account      string
	DebitCents   int64
	CreditCents  int64
	Basis        json.RawMessage
}

// Statements 返回指定参与方的分录（可选按周期过滤）。
func (s *Store) Statements(ctx context.Context, participantID string, period *ledger.Date) ([]StatementRow, error) {
	query := `SELECT st.id, st.region_code, to_char(st.period, 'YYYY-MM-DD'),
		p.leg, p.account, p.debit_cents, p.credit_cents, p.basis::text
		FROM postings p JOIN settlements st ON st.id = p.settlement_id
		WHERE p.participant_id = $1`
	args := []any{participantID}
	if period != nil {
		query += ` AND st.period = $2::date`
		args = append(args, string(*period))
	}
	query += ` ORDER BY st.id, p.entry_no`
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementRow
	for rows.Next() {
		var r StatementRow
		var per, basis string
		if err := rows.Scan(&r.SettlementID, &r.RegionCode, &per, &r.Leg, &r.Account,
			&r.DebitCents, &r.CreditCents, &basis); err != nil {
			return nil, err
		}
		r.Period = ledger.Date(per)
		r.Basis = json.RawMessage(basis)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SettlementDetail 返回单笔结算与全部分录（管理员/审计员）。
func (s *Store) SettlementDetail(ctx context.Context, id int64) (SettlementRow, []PostingRow, error) {
	row, err := scanSettlement(s.Pool.QueryRow(ctx,
		`SELECT `+settlementColumns+` FROM settlements WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SettlementRow{}, nil, nil
	}
	if err != nil {
		return SettlementRow{}, nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT entry_no, leg, account, participant_id,
		debit_cents, credit_cents, basis::text
		FROM postings WHERE settlement_id = $1 ORDER BY entry_no`, id)
	if err != nil {
		return SettlementRow{}, nil, err
	}
	postings, err := collectPostings(rows)
	return row, postings, err
}

// ImbalancedSettlements 返回借贷不平衡的结算 id（正常应为空，供 verify 使用）。
func (s *Store) ImbalancedSettlements(ctx context.Context) ([]int64, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT settlement_id FROM v_settlement_balance WHERE imbalance <> 0 ORDER BY settlement_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
