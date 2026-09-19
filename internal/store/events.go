package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"example.com/culture-sharing/internal/ledger"
)

// EventInput 是一条待写入时间线的事件。
type EventInput struct {
	EventID    string // UUID，客户端幂等键
	Type       string
	RegionCode string
	OrderID    string
	OccurredAt time.Time
	Payload    json.RawMessage
	// Period 为空时按区域时区由 occurred_at 折算（订单/退货/费率/合同）；
	// 结算确认事件显式传入被确认周期。
	Period *ledger.Date
}

// InsertResult 是写入结果；Duplicate=true 表示幂等命中，返回既有行。
type InsertResult struct {
	Event     ledger.Event
	Duplicate bool
}

// InsertEvent 在独立事务中把事件追加到不可变时间线。
func (s *Store) InsertEvent(ctx context.Context, in EventInput) (InsertResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return InsertResult{}, err
	}
	defer tx.Rollback(ctx)
	res, err := s.InsertEventTx(ctx, tx, in)
	if err != nil {
		return InsertResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return InsertResult{}, err
	}
	return res, nil
}

// InsertEventTx 在给定事务内追加事件，步骤：
//  1. 幂等检查（event_id 唯一）——重复提交直接返回既有行；
//  2. chain_state 行锁——串行化全部写入，seq 无空洞且与哈希链一致；
//  3. 周期归属——按区域时区折算（跨午夜归属的唯一依据）；
//  4. 迟到判定——周期已关闭的旧事件标记 late，可审计但不回退水位；
//  5. 写哈希链；合同版本事件同步投影到 contracts 表。
func (s *Store) InsertEventTx(ctx context.Context, tx pgx.Tx, in EventInput) (InsertResult, error) {
	if in.EventID == "" {
		return InsertResult{}, fmt.Errorf("event_id is required")
	}
	if existing, ok, err := eventByIDTx(ctx, tx, in.EventID); err != nil {
		return InsertResult{}, err
	} else if ok {
		return InsertResult{Event: existing, Duplicate: true}, nil
	}

	var lastSeq int64
	var lastHash []byte
	if err := tx.QueryRow(ctx,
		`SELECT last_seq, last_hash FROM chain_state WHERE id = 1 FOR UPDATE`).Scan(&lastSeq, &lastHash); err != nil {
		return InsertResult{}, fmt.Errorf("lock chain_state: %w", err)
	}
	// 链锁已串行化全部写入：复查 event_id，关闭与并发写入的竞态窗口
	if existing, ok, err := eventByIDTx(ctx, tx, in.EventID); err != nil {
		return InsertResult{}, err
	} else if ok {
		return InsertResult{Event: existing, Duplicate: true}, nil
	}

	// occurred_at 经数据库归一化（timestamptz 为微秒精度），
	// 保证写入值与回读值逐位一致，哈希链在任何精度下都可重算。
	var occurredAt time.Time
	if err := tx.QueryRow(ctx, `SELECT $1::timestamptz`, in.OccurredAt).Scan(&occurredAt); err != nil {
		return InsertResult{}, fmt.Errorf("normalize occurred_at: %w", err)
	}

	var period ledger.Date
	if in.Period != nil {
		period = *in.Period
	} else {
		var tz string
		err := tx.QueryRow(ctx, `SELECT timezone FROM regions WHERE code = $1`, in.RegionCode).Scan(&tz)
		if errors.Is(err, pgx.ErrNoRows) {
			return InsertResult{}, fmt.Errorf("unknown region %q", in.RegionCode)
		}
		if err != nil {
			return InsertResult{}, err
		}
		var p string
		if err := tx.QueryRow(ctx,
			`SELECT to_char(($1::timestamptz AT TIME ZONE $2), 'YYYY-MM-DD')`,
			occurredAt, tz).Scan(&p); err != nil {
			return InsertResult{}, fmt.Errorf("derive period: %w", err)
		}
		period = ledger.Date(p)
	}

	late := false
	var closed string
	err := tx.QueryRow(ctx,
		`SELECT to_char(closed_period, 'YYYY-MM-DD') FROM watermarks WHERE region_code = $1`,
		in.RegionCode).Scan(&closed)
	switch {
	case err == nil:
		late = period <= ledger.Date(closed)
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return InsertResult{}, err
	}

	if err := validatePayload(in.Type, in.Payload); err != nil {
		return InsertResult{}, err
	}
	canonical, err := ledger.CanonicalPayload(in.Payload)
	if err != nil {
		return InsertResult{}, err
	}

	seq := lastSeq + 1
	hash := ledger.EventHash(lastHash, ledger.HashRecord{
		Seq: seq, EventID: in.EventID, Type: in.Type, RegionCode: in.RegionCode,
		OrderID: in.OrderID, OccurredAt: occurredAt, Period: period, Late: late,
		Payload: canonical,
	})
	var orderID any
	if in.OrderID != "" {
		orderID = in.OrderID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events
		(seq, event_id, type, region_code, order_id, occurred_at, period, late, payload, prev_hash, hash)
		VALUES ($1, $2::uuid, $3, $4, $5, $6, $7::date, $8, $9::jsonb, $10, $11)`,
		seq, in.EventID, in.Type, in.RegionCode, orderID, occurredAt,
		string(period), late, string(canonical), lastHash, hash); err != nil {
		return InsertResult{}, fmt.Errorf("insert event: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE chain_state SET last_seq = $1, last_hash = $2 WHERE id = 1`, seq, hash); err != nil {
		return InsertResult{}, err
	}

	if in.Type == ledger.EventContract {
		cv, err := ledger.ContractFromEvent(ledger.Event{Seq: seq, Type: in.Type, Payload: canonical})
		if err != nil {
			return InsertResult{}, err
		}
		termsJSON, _ := json.Marshal(cv.Terms)
		if _, err := tx.Exec(ctx, `INSERT INTO contracts
			(contract_no, version, effective_from, terms, registered_seq)
			VALUES ($1, $2, $3::date, $4::jsonb, $5)
			ON CONFLICT (contract_no, version) DO NOTHING`,
			cv.ContractNo, cv.Version, string(cv.EffectiveFrom), string(termsJSON), seq); err != nil {
			return InsertResult{}, err
		}
	}

	return InsertResult{Event: ledger.Event{
		Seq: seq, EventID: in.EventID, Type: in.Type, RegionCode: in.RegionCode,
		OrderID: in.OrderID, OccurredAt: occurredAt, Period: period, Late: late,
		Payload: canonical, PrevHash: lastHash, Hash: hash,
	}}, nil
}

// validatePayload 用领域解析器校验载荷，拒绝垃圾进入时间线。
func validatePayload(eventType string, payload json.RawMessage) error {
	ev := ledger.Event{Type: eventType, Payload: payload}
	var err error
	switch eventType {
	case ledger.EventOrder:
		_, err = ledger.OrderLineFromEvent(ev)
	case ledger.EventReturn:
		_, err = ledger.ReturnLineFromEvent(ev, nil)
	case ledger.EventChannelRate:
		_, err = ledger.RateFromEvent(ev)
	case ledger.EventContract:
		_, err = ledger.ContractFromEvent(ev)
	case ledger.EventConfirmation:
		_, _, err = ledger.ConfirmationFromEvent(ev)
	default:
		err = fmt.Errorf("unknown event type %q", eventType)
	}
	return err
}

const eventColumns = `seq, event_id::text, type, region_code, COALESCE(order_id, ''),
	occurred_at, to_char(period, 'YYYY-MM-DD'), late, payload::text, prev_hash, hash`

// scanner 同时被 pgx.Row 与 pgx.Rows 满足。
type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(row scanner) (ledger.Event, error) {
	var ev ledger.Event
	var period string
	var payload string
	err := row.Scan(&ev.Seq, &ev.EventID, &ev.Type, &ev.RegionCode, &ev.OrderID,
		&ev.OccurredAt, &period, &ev.Late, &payload, &ev.PrevHash, &ev.Hash)
	ev.Period = ledger.Date(period)
	ev.Payload = []byte(payload)
	return ev, err
}

// collectEvents 迭代查询结果并逐行解码。
func collectEvents(rows pgx.Rows) ([]ledger.Event, error) {
	defer rows.Close()
	var out []ledger.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func eventByIDTx(ctx context.Context, tx pgx.Tx, eventID string) (ledger.Event, bool, error) {
	ev, err := scanEvent(tx.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM events WHERE event_id = $1::uuid`, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Event{}, false, nil
	}
	if err != nil {
		return ledger.Event{}, false, err
	}
	return ev, true, nil
}
