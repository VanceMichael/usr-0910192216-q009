package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"example.com/culture-sharing/internal/ledger"
	"example.com/culture-sharing/internal/store"
)

// genesisSeed 与 chain_state 初始 last_hash ('\x00') 对应。
var genesisSeed = []byte{0}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Pool.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "database not ready")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- 事件写入 ----

type ingestRequest struct {
	EventID    string          `json:"event_id"`
	Type       string          `json:"type"`
	RegionCode string          `json:"region_code"`
	OrderID    string          `json:"order_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

func (s *Server) handleIngestEvent(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.EventID == "" || req.Type == "" || req.RegionCode == "" || req.OccurredAt.IsZero() || len(req.Payload) == 0 {
		writeErr(w, http.StatusBadRequest, "event_id, type, region_code, occurred_at, payload are required")
		return
	}
	res, err := s.st.InsertEvent(r.Context(), store.EventInput{
		EventID: req.EventID, Type: req.Type, RegionCode: req.RegionCode,
		OrderID: req.OrderID, OccurredAt: req.OccurredAt, Payload: req.Payload,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, eventJSON(res.Event, res.Duplicate))
}

func eventJSON(ev ledger.Event, duplicate bool) map[string]any {
	return map[string]any{
		"seq":         ev.Seq,
		"event_id":    ev.EventID,
		"type":        ev.Type,
		"region_code": ev.RegionCode,
		"order_id":    ev.OrderID,
		"occurred_at": ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		"period":      ev.Period,
		"late":        ev.Late,
		"hash":        hex.EncodeToString(ev.Hash),
		"duplicate":   duplicate,
	}
}

// ---- 结算确认 ----

type confirmRequest struct {
	ConfirmationID string `json:"confirmation_id"`
	RegionCode     string `json:"region_code"`
	Period         string `json:"period"`
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	var req confirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	in := store.ConfirmInput{
		ConfirmationID: req.ConfirmationID,
		RegionCode:     req.RegionCode,
		Period:         req.Period,
		PlatformID:     s.cfg.PlatformID,
	}
	// 故障注入钩子（仅 LEDGER_FAULT_HOOKS=1 时可用）：
	// crash_before_commit 在事务提交前退出 -> 整体回滚；
	// crash_after_commit 在提交后、响应前退出 -> 客户端重试走幂等路径。
	fault := r.URL.Query().Get("fault")
	if fault != "" && !s.cfg.EnableFaultHooks {
		writeErr(w, http.StatusForbidden, "fault hooks disabled")
		return
	}
	if fault == "crash_before_commit" {
		in.BeforeCommit = func() { os.Exit(1) }
	}
	res, err := s.st.ConfirmSettlement(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if fault == "crash_after_commit" {
		os.Exit(1)
	}
	body := map[string]any{
		"outcome":    res.Outcome,
		"settlement": settlementJSON(res.Settlement),
	}
	switch res.Outcome {
	case store.ConfirmCreated:
		body["postings"] = postingsJSON(res.Postings)
		writeJSON(w, http.StatusCreated, body)
	case store.ConfirmDuplicate:
		body["postings"] = postingsJSON(res.Postings)
		writeJSON(w, http.StatusOK, body)
	default: // conflict
		writeJSON(w, http.StatusConflict, body)
	}
}

func settlementJSON(row store.SettlementRow) map[string]any {
	return map[string]any{
		"id":               row.ID,
		"confirmation_id":  row.ConfirmationID,
		"region_code":      row.RegionCode,
		"period":           row.Period,
		"contract_no":      row.ContractNo,
		"contract_version": row.ContractVersion,
		"terms":            row.Terms,
		"confirmation_seq": row.ConfirmationSeq,
		"gross_cents":      row.GrossCents,
		"returned_cents":   row.ReturnedCents,
	}
}

func postingsJSON(rows []store.PostingRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, map[string]any{
			"entry_no":       p.EntryNo,
			"leg":            p.Leg,
			"account":        p.Account,
			"participant_id": p.ParticipantID,
			"debit_cents":    p.DebitCents,
			"credit_cents":   p.CreditCents,
			"basis":          p.Basis,
		})
	}
	return out
}

// ---- 参与方对账单（只能读自己的金额与公开依据） ----

func (s *Server) handleStatements(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	participantID := p.ID
	if p.Kind == "admin" || p.Kind == "auditor" {
		participantID = r.URL.Query().Get("participant_id")
		if participantID == "" {
			writeErr(w, http.StatusBadRequest, "participant_id is required for admin/auditor")
			return
		}
	}
	var period *ledger.Date
	if q := r.URL.Query().Get("period"); q != "" {
		d, err := ledger.ParseDate(q)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		period = &d
	}
	rows, err := s.st.Statements(r.Context(), participantID, period)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	entries := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, map[string]any{
			"settlement_id": row.SettlementID,
			"region_code":   row.RegionCode,
			"period":        row.Period,
			"leg":           row.Leg,
			"account":       row.Account,
			"debit_cents":   row.DebitCents,
			"credit_cents":  row.CreditCents,
			"basis":         row.Basis,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"participant_id": participantID,
		"entries":        entries,
	})
}

// ---- 结算明细（管理员/审计员） ----

func (s *Server) handleGetSettlement(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad settlement id")
		return
	}
	row, postings, err := s.st.SettlementDetail(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if row.ID == 0 {
		writeErr(w, http.StatusNotFound, "settlement not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settlement": settlementJSON(row),
		"postings":   postingsJSON(postings),
	})
}

// ---- 审计接口 ----

func (s *Server) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from := parseIntDefault(q.Get("from_seq"), 1)
	to := parseIntDefault(q.Get("to_seq"), 1<<62)
	if from < 1 || to < from {
		writeErr(w, http.StatusBadRequest, "invalid seq range")
		return
	}
	events, err := s.st.ListEvents(r.Context(), from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	const limit = 1000
	truncated := false
	if len(events) > limit {
		events = events[:limit]
		truncated = true
	}
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		out = append(out, eventJSON(ev, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "truncated": truncated})
}

// handleSnapshot 重放到指定审计序号，并与账本已确认结果对账。
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	events, headSeq, err := s.loadTimeline(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	asOf := parseIntDefault(r.URL.Query().Get("as_of_seq"), headSeq)
	if asOf < 0 || asOf > headSeq {
		writeErr(w, http.StatusBadRequest, "as_of_seq out of range")
		return
	}
	snap, err := ledger.Replay(events, asOf, s.cfg.PlatformID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	stored, err := s.st.StoredBalances(r.Context(), asOf)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of_seq":       asOf,
		"balances":        snap.Balances,
		"stored_balances": stored,
		"match":           balancesEqual(snap.Balances, stored),
		"settlements":     snap.Settlements,
	})
}

// handleDiff 输出两个水位上两次重算的差异。
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from := parseIntDefault(q.Get("from_seq"), -1)
	to := parseIntDefault(q.Get("to_seq"), -1)
	if from < 0 || to <= from {
		writeErr(w, http.StatusBadRequest, "require 0 <= from_seq < to_seq")
		return
	}
	events, headSeq, err := s.loadTimeline(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if to > headSeq {
		writeErr(w, http.StatusBadRequest, "to_seq beyond chain head")
		return
	}
	a, err := ledger.Replay(events, from, s.cfg.PlatformID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	b, err := ledger.Replay(events, to, s.cfg.PlatformID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	d := ledger.DiffSnapshots(a, b)
	writeJSON(w, http.StatusOK, map[string]any{
		"from_seq":        d.FromSeq,
		"to_seq":          d.ToSeq,
		"deltas":          d.Deltas,
		"new_settlements": d.NewSettlements,
	})
}

// handleVerifyChain 重算整条哈希链并与链状态对照——证明时间线未被改写。
func (s *Server) handleVerifyChain(w http.ResponseWriter, r *http.Request) {
	events, headSeq, headHash, err := s.loadTimelineWithHead(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	computed, err := ledger.VerifyChain(events, genesisSeed)
	ok := err == nil && string(computed) == string(headHash)
	resp := map[string]any{
		"ok":             ok,
		"events_checked": len(events),
		"head_seq":       headSeq,
		"head_hash":      hex.EncodeToString(headHash),
	}
	if err != nil {
		resp["error"] = err.Error()
	}
	if err == nil && !ok {
		resp["error"] = "computed head does not match chain_state"
		resp["computed_head"] = hex.EncodeToString(computed)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleWatermarks(w http.ResponseWriter, r *http.Request) {
	wms, err := s.st.Watermarks(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"watermarks": wms})
}

// ---- 故障注入（仅演示/测试） ----

func (s *Server) handleFault(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.EnableFaultHooks {
		writeErr(w, http.StatusForbidden, "fault hooks disabled")
		return
	}
	switch r.URL.Query().Get("mode") {
	case "crash":
		writeJSON(w, http.StatusAccepted, map[string]string{"fault": "crash scheduled"})
		go func() {
			time.Sleep(150 * time.Millisecond)
			os.Exit(1)
		}()
	default:
		writeErr(w, http.StatusBadRequest, "unknown fault mode")
	}
}

// ---- 辅助 ----

func (s *Server) loadTimeline(r *http.Request) ([]ledger.Event, int64, error) {
	events, headSeq, _, err := s.loadTimelineWithHead(r)
	return events, headSeq, err
}

func (s *Server) loadTimelineWithHead(r *http.Request) ([]ledger.Event, int64, []byte, error) {
	events, err := s.st.AllEvents(r.Context())
	if err != nil {
		return nil, 0, nil, err
	}
	headSeq, headHash, err := s.st.ChainHead(r.Context())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil, err
	}
	return events, headSeq, headHash, nil
}

func balancesEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func parseIntDefault(s string, def int64) int64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return n
}
