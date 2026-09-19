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

// SettlementRow 是一笔已确认结算（合同解释已冻结在行内）。
type SettlementRow struct {
	ID              int64
	ConfirmationID  string
	RegionCode      string
	Period          ledger.Date
	ContractNo      string
	ContractVersion int
	Terms           json.RawMessage
	ConfirmationSeq int64
	GrossCents      int64
	ReturnedCents   int64
}

// PostingRow 是一条分账指令分录。
type PostingRow struct {
	EntryNo       int
	Leg           string
	Account       string
	ParticipantID string
	DebitCents    int64
	CreditCents   int64
	Basis         json.RawMessage
}

// ConfirmOutcome 是确认结果分类。
type ConfirmOutcome string

const (
	ConfirmCreated   ConfirmOutcome = "created"   // 本次确认生成了分账指令
	ConfirmDuplicate ConfirmOutcome = "duplicate" // 同一 confirmation_id 已确认，原样返回
	ConfirmConflict  ConfirmOutcome = "conflict"  // 周期已被其他确认占用或已关闭
)

// ConfirmInput 是一次结算确认请求。
type ConfirmInput struct {
	ConfirmationID string
	RegionCode     string
	Period         string
	PlatformID     string
	// BeforeCommit 仅供故障注入：在提交前调用（如 os.Exit），事务随之回滚。
	BeforeCommit func()
}

// ConfirmResult 是确认结果；Conflict 时 Settlement 为既有结算。
type ConfirmResult struct {
	Outcome    ConfirmOutcome
	Settlement SettlementRow
	Postings   []PostingRow
}

// ConfirmSettlement 确认一个区域一个周期的结算。整个流程在单事务内：
// 幂等命中 -> 周期咨询锁（并发串行化）-> 冲突检查 -> 写确认事件（审计序号）
// -> 纯函数计算 -> 写结算与分录 -> 同事务校验借贷平衡 -> 水位单调前进 -> 提交。
// 任何一步失败整体回滚，时间线与水位保持原状。
func (s *Store) ConfirmSettlement(ctx context.Context, in ConfirmInput) (ConfirmResult, error) {
	period, err := ledger.ParseDate(in.Period)
	if err != nil {
		return ConfirmResult{}, err
	}
	if in.ConfirmationID == "" {
		return ConfirmResult{}, fmt.Errorf("confirmation_id is required")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ConfirmResult{}, err
	}
	defer tx.Rollback(ctx)

	// 1) 同一 confirmation_id：幂等返回，分账指令只产生一次
	if row, ok, err := settlementByConfirmationTx(ctx, tx, in.ConfirmationID); err != nil {
		return ConfirmResult{}, err
	} else if ok {
		postings, err := postingsForTx(ctx, tx, row.ID)
		if err != nil {
			return ConfirmResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ConfirmResult{}, err
		}
		return ConfirmResult{Outcome: ConfirmDuplicate, Settlement: row, Postings: postings}, nil
	}

	// 2) 周期级咨询锁：并发确认在同一事务内串行
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`,
		in.RegionCode+"|"+string(period)); err != nil {
		return ConfirmResult{}, err
	}

	// 3) 锁内复查 confirmation_id：并发同键确认后到者按幂等返回
	if row, ok, err := settlementByConfirmationTx(ctx, tx, in.ConfirmationID); err != nil {
		return ConfirmResult{}, err
	} else if ok {
		postings, err := postingsForTx(ctx, tx, row.ID)
		if err != nil {
			return ConfirmResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ConfirmResult{}, err
		}
		return ConfirmResult{Outcome: ConfirmDuplicate, Settlement: row, Postings: postings}, nil
	}

	// 4) 周期已被其他确认占用
	if row, ok, err := settlementByPeriodTx(ctx, tx, in.RegionCode, period); err != nil {
		return ConfirmResult{}, err
	} else if ok {
		if err := tx.Commit(ctx); err != nil {
			return ConfirmResult{}, err
		}
		return ConfirmResult{Outcome: ConfirmConflict, Settlement: row}, nil
	}

	// 5) 周期已关闭（水位）——迟到确认不得回退水位
	var closed string
	err = tx.QueryRow(ctx,
		`SELECT to_char(closed_period, 'YYYY-MM-DD') FROM watermarks WHERE region_code = $1`,
		in.RegionCode).Scan(&closed)
	switch {
	case err == nil:
		if period <= ledger.Date(closed) {
			if err := tx.Commit(ctx); err != nil {
				return ConfirmResult{}, err
			}
			return ConfirmResult{Outcome: ConfirmConflict}, nil
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return ConfirmResult{}, err
	}

	// 6) 写确认事件（占用审计序号，进入哈希链）
	confPayload, _ := json.Marshal(map[string]string{
		"region_code": in.RegionCode, "period": string(period),
	})
	evRes, err := s.InsertEventTx(ctx, tx, EventInput{
		EventID: in.ConfirmationID, Type: ledger.EventConfirmation,
		RegionCode: in.RegionCode, OccurredAt: time.Now().UTC(),
		Payload: confPayload, Period: &period,
	})
	if err != nil {
		return ConfirmResult{}, err
	}
	if evRes.Duplicate {
		// 事件已存在却没有结算行：只可能是历史异常，按冲突处理，绝不重复记账
		if err := tx.Commit(ctx); err != nil {
			return ConfirmResult{}, err
		}
		return ConfirmResult{Outcome: ConfirmConflict}, nil
	}
	confSeq := evRes.Event.Seq

	// 7) 取数：本周期的订单/退货（排除迟到）+ 截至确认序号的合同与费率
	lines, err := loadLinesTx(ctx, tx, in.RegionCode, period, confSeq)
	if err != nil {
		return ConfirmResult{}, err
	}
	st, err := ledger.ComputeSettlement(ledger.SettlementInput{
		Region: in.RegionCode, Period: period, PlatformID: in.PlatformID,
		Orders: lines.orders, Returns: lines.returns,
		Contracts: lines.contracts, Rates: lines.rates,
	})
	if err != nil {
		return ConfirmResult{}, err
	}

	// 8) 写结算行（冻结合同解释）
	termsJSON, _ := json.Marshal(st.Contract.Terms)
	var settlementID int64
	if err := tx.QueryRow(ctx, `INSERT INTO settlements
		(confirmation_id, region_code, period, contract_no, contract_version, terms,
		 confirmation_seq, gross_cents, returned_cents)
		VALUES ($1::uuid, $2, $3::date, $4, $5, $6::jsonb, $7, $8, $9)
		RETURNING id`,
		in.ConfirmationID, in.RegionCode, string(period),
		st.Contract.ContractNo, st.Contract.Version, string(termsJSON),
		confSeq, st.GrossCents, st.ReturnedCents).Scan(&settlementID); err != nil {
		return ConfirmResult{}, fmt.Errorf("insert settlement: %w", err)
	}

	// 9) 写分录（销售腿 + 独立冲正腿）
	for i, p := range st.Postings {
		basisJSON, _ := json.Marshal(p.Basis)
		if _, err := tx.Exec(ctx, `INSERT INTO postings
			(settlement_id, entry_no, leg, account, participant_id, debit_cents, credit_cents, basis)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)`,
			settlementID, i+1, p.Leg, p.Account, p.ParticipantID,
			p.DebitCents, p.CreditCents, string(basisJSON)); err != nil {
			return ConfirmResult{}, fmt.Errorf("insert posting %d: %w", i+1, err)
		}
	}

	// 10) 同事务借贷平衡校验（防御纵深；纯函数层已构造性保证）
	var debits, credits int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(debit_cents), 0), COALESCE(SUM(credit_cents), 0)
		FROM postings WHERE settlement_id = $1`, settlementID).Scan(&debits, &credits); err != nil {
		return ConfirmResult{}, err
	}
	if debits != credits {
		return ConfirmResult{}, fmt.Errorf("unbalanced settlement: debits %d != credits %d", debits, credits)
	}

	// 11) 水位单调前进（GREATEST 保证只进不退）
	if _, err := tx.Exec(ctx, `INSERT INTO watermarks (region_code, closed_period, high_seq)
		VALUES ($1, $2::date, $3)
		ON CONFLICT (region_code) DO UPDATE SET
			closed_period = GREATEST(watermarks.closed_period, EXCLUDED.closed_period),
			high_seq      = GREATEST(watermarks.high_seq, EXCLUDED.high_seq)`,
		in.RegionCode, string(period), confSeq); err != nil {
		return ConfirmResult{}, err
	}

	// 12) 故障注入钩子（仅测试）：此处退出则事务回滚，什么都不留下
	if in.BeforeCommit != nil {
		in.BeforeCommit()
	}

	// 13) 提交
	if err := tx.Commit(ctx); err != nil {
		return ConfirmResult{}, err
	}

	row := SettlementRow{
		ID: settlementID, ConfirmationID: in.ConfirmationID,
		RegionCode: in.RegionCode, Period: period,
		ContractNo: st.Contract.ContractNo, ContractVersion: st.Contract.Version,
		Terms: termsJSON, ConfirmationSeq: confSeq,
		GrossCents: st.GrossCents, ReturnedCents: st.ReturnedCents,
	}
	postings := make([]PostingRow, 0, len(st.Postings))
	for i, p := range st.Postings {
		basisJSON, _ := json.Marshal(p.Basis)
		postings = append(postings, PostingRow{
			EntryNo: i + 1, Leg: p.Leg, Account: p.Account,
			ParticipantID: p.ParticipantID,
			DebitCents:    p.DebitCents, CreditCents: p.CreditCents,
			Basis: basisJSON,
		})
	}
	return ConfirmResult{Outcome: ConfirmCreated, Settlement: row, Postings: postings}, nil
}

// settlementLines 是一次结算的事件取数结果。
type settlementLines struct {
	orders    []ledger.OrderLine
	returns   []ledger.ReturnLine
	contracts []ledger.ContractVersion
	rates     []ledger.RateVersion
}

// loadLinesTx 读取计算所需的全部事件并解码为领域行。
func loadLinesTx(ctx context.Context, tx pgx.Tx, region string, period ledger.Date, asOfSeq int64) (settlementLines, error) {
	var out settlementLines

	rows, err := tx.Query(ctx, `SELECT `+eventColumns+` FROM events
		WHERE region_code = $1 AND period = $2::date AND type IN ('order','return') AND NOT late
		ORDER BY seq`, region, string(period))
	if err != nil {
		return out, err
	}
	periodEvents, err := collectEvents(rows)
	if err != nil {
		return out, err
	}

	// 原订单查找：退货按原订单周期的合同与费率冲回
	returnOrderIDs := map[string]bool{}
	for _, ev := range periodEvents {
		if ev.Type == ledger.EventReturn {
			returnOrderIDs[ev.OrderID] = true
		}
	}
	originals := map[string]ledger.OrderLine{}
	if len(returnOrderIDs) > 0 {
		ids := make([]string, 0, len(returnOrderIDs))
		for id := range returnOrderIDs {
			ids = append(ids, id)
		}
		rows, err := tx.Query(ctx, `SELECT `+eventColumns+` FROM events
			WHERE type = 'order' AND order_id = ANY($1) AND seq <= $2 ORDER BY seq`, ids, asOfSeq)
		if err != nil {
			return out, err
		}
		orderEvents, err := collectEvents(rows)
		if err != nil {
			return out, err
		}
		for _, ev := range orderEvents {
			if _, exists := originals[ev.OrderID]; !exists {
				ol, err := ledger.OrderLineFromEvent(ev)
				if err != nil {
					return out, err
				}
				originals[ev.OrderID] = ol
			}
		}
	}

	for _, ev := range periodEvents {
		switch ev.Type {
		case ledger.EventOrder:
			ol, err := ledger.OrderLineFromEvent(ev)
			if err != nil {
				return out, err
			}
			out.orders = append(out.orders, ol)
		case ledger.EventReturn:
			var orig *ledger.OrderLine
			if o, found := originals[ev.OrderID]; found {
				cp := o
				orig = &cp
			}
			rl, err := ledger.ReturnLineFromEvent(ev, orig)
			if err != nil {
				return out, err
			}
			out.returns = append(out.returns, rl)
		}
	}

	// 合同与费率版本：全时间线、截至确认序号
	rows, err = tx.Query(ctx, `SELECT `+eventColumns+` FROM events
		WHERE type IN ('contract_version','channel_rate') AND seq <= $1 ORDER BY seq`, asOfSeq)
	if err != nil {
		return out, err
	}
	versionEvents, err := collectEvents(rows)
	if err != nil {
		return out, err
	}
	for _, ev := range versionEvents {
		switch ev.Type {
		case ledger.EventContract:
			cv, err := ledger.ContractFromEvent(ev)
			if err != nil {
				return out, err
			}
			out.contracts = append(out.contracts, cv)
		case ledger.EventChannelRate:
			rv, err := ledger.RateFromEvent(ev)
			if err != nil {
				return out, err
			}
			out.rates = append(out.rates, rv)
		}
	}
	return out, nil
}

const settlementColumns = `id, confirmation_id::text, region_code, to_char(period, 'YYYY-MM-DD'),
	contract_no, contract_version, terms::text, confirmation_seq, gross_cents, returned_cents`

func scanSettlement(row pgx.Row) (SettlementRow, error) {
	var r SettlementRow
	var period, terms string
	err := row.Scan(&r.ID, &r.ConfirmationID, &r.RegionCode, &period,
		&r.ContractNo, &r.ContractVersion, &terms, &r.ConfirmationSeq, &r.GrossCents, &r.ReturnedCents)
	r.Period = ledger.Date(period)
	r.Terms = json.RawMessage(terms)
	return r, err
}

func settlementByConfirmationTx(ctx context.Context, tx pgx.Tx, confirmationID string) (SettlementRow, bool, error) {
	row, err := scanSettlement(tx.QueryRow(ctx,
		`SELECT `+settlementColumns+` FROM settlements WHERE confirmation_id = $1::uuid`, confirmationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SettlementRow{}, false, nil
	}
	return row, err == nil, err
}

func settlementByPeriodTx(ctx context.Context, tx pgx.Tx, region string, period ledger.Date) (SettlementRow, bool, error) {
	row, err := scanSettlement(tx.QueryRow(ctx,
		`SELECT `+settlementColumns+` FROM settlements WHERE region_code = $1 AND period = $2::date`,
		region, string(period)))
	if errors.Is(err, pgx.ErrNoRows) {
		return SettlementRow{}, false, nil
	}
	return row, err == nil, err
}

func postingsForTx(ctx context.Context, tx pgx.Tx, settlementID int64) ([]PostingRow, error) {
	rows, err := tx.Query(ctx, `SELECT entry_no, leg, account, participant_id,
		debit_cents, credit_cents, basis::text
		FROM postings WHERE settlement_id = $1 ORDER BY entry_no`, settlementID)
	if err != nil {
		return nil, err
	}
	return collectPostings(rows)
}

// collectPostings 迭代分录查询结果。
func collectPostings(rows pgx.Rows) ([]PostingRow, error) {
	defer rows.Close()
	var out []PostingRow
	for rows.Next() {
		var p PostingRow
		var basis string
		if err := rows.Scan(&p.EntryNo, &p.Leg, &p.Account, &p.ParticipantID,
			&p.DebitCents, &p.CreditCents, &basis); err != nil {
			return nil, err
		}
		p.Basis = json.RawMessage(basis)
		out = append(out, p)
	}
	return out, rows.Err()
}
