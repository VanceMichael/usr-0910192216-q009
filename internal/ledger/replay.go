package ledger

import (
	"fmt"
	"sort"
)

// ReplayedSettlement 是重放得到的一笔结算摘要，用于审计差异说明。
type ReplayedSettlement struct {
	Region          string `json:"region"`
	Period          Date   `json:"period"`
	ContractNo      string `json:"contract_no"`
	ContractVersion int    `json:"contract_version"`
	GrossCents      int64  `json:"gross_cents"`
	ReturnedCents   int64  `json:"returned_cents"`
	ConfirmationSeq int64  `json:"confirmation_seq"`
}

// Snapshot 是指定审计序号水位上的重算结果。
type Snapshot struct {
	AsOfSeq     int64
	Balances    map[string]int64 // participant_id -> 净额（贷-借，分）
	Settlements []ReplayedSettlement
}

// Replay 把事件流重放到 asOf（含）。只有被 settlement_confirmation 事件
// 确认过的 (region, period) 才计入余额；迟到事件（late=true）永远不计入，
// 但保留在事件流中供审计。与在线确认共用 ComputeSettlement。
func Replay(events []Event, asOf int64, platformID string) (Snapshot, error) {
	snap := Snapshot{AsOfSeq: asOf, Balances: map[string]int64{}}
	var allContracts []ContractVersion
	var allRates []RateVersion
	type confKey struct {
		region string
		period Date
	}
	confs := map[confKey]int64{} // -> 首个确认事件 seq（重复确认只生效一次）
	ordersByID := map[string]OrderLine{}
	for _, ev := range events {
		if ev.Seq > asOf {
			continue
		}
		switch ev.Type {
		case EventContract:
			c, err := ContractFromEvent(ev)
			if err != nil {
				return snap, err
			}
			allContracts = append(allContracts, c)
		case EventChannelRate:
			r, err := RateFromEvent(ev)
			if err != nil {
				return snap, err
			}
			allRates = append(allRates, r)
		case EventConfirmation:
			region, period, err := ConfirmationFromEvent(ev)
			if err != nil {
				return snap, err
			}
			k := confKey{region, period}
			if _, dup := confs[k]; !dup {
				confs[k] = ev.Seq
			}
		case EventOrder:
			ol, err := OrderLineFromEvent(ev)
			if err != nil {
				return snap, err
			}
			if _, exists := ordersByID[ev.OrderID]; !exists {
				ordersByID[ev.OrderID] = ol
			}
		}
	}

	keys := make([]confKey, 0, len(confs))
	for k := range confs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return confs[keys[i]] < confs[keys[j]] })

	for _, k := range keys {
		// 每个周期按其确认序号截取合同与费率版本——与在线确认路径
		// （seq <= confirmation_seq）严格同构。确认之后才注册的追溯性
		// 版本不会改变历史结算的重放结果，保证快照与账本对账一致。
		confSeq := confs[k]
		contracts := filterContractsBySeq(allContracts, confSeq)
		rates := filterRatesBySeq(allRates, confSeq)

		var orders []OrderLine
		var returns []ReturnLine
		for _, ev := range events {
			if ev.Seq > asOf || ev.Late {
				continue
			}
			if ev.RegionCode != k.region || ev.Period != k.period {
				continue
			}
			switch ev.Type {
			case EventOrder:
				ol, err := OrderLineFromEvent(ev)
				if err != nil {
					return snap, err
				}
				orders = append(orders, ol)
			case EventReturn:
				var orig *OrderLine
				if o, found := ordersByID[ev.OrderID]; found {
					cp := o
					orig = &cp
				}
				rl, err := ReturnLineFromEvent(ev, orig)
				if err != nil {
					return snap, err
				}
				returns = append(returns, rl)
			}
		}
		st, err := ComputeSettlement(SettlementInput{
			Region: k.region, Period: k.period, PlatformID: platformID,
			Orders: orders, Returns: returns, Contracts: contracts, Rates: rates,
		})
		if err != nil {
			return snap, fmt.Errorf("replay %s/%s: %w", k.region, k.period, err)
		}
		for _, p := range st.Postings {
			snap.Balances[p.ParticipantID] += p.CreditCents - p.DebitCents
		}
		snap.Settlements = append(snap.Settlements, ReplayedSettlement{
			Region: k.region, Period: k.period,
			ContractNo: st.Contract.ContractNo, ContractVersion: st.Contract.Version,
			GrossCents: st.GrossCents, ReturnedCents: st.ReturnedCents,
			ConfirmationSeq: confs[k],
		})
	}
	return snap, nil
}

// filterContractsBySeq 截取 seq <= cutoff 已注册的合同版本。
func filterContractsBySeq(vs []ContractVersion, cutoff int64) []ContractVersion {
	out := make([]ContractVersion, 0, len(vs))
	for _, v := range vs {
		if v.Seq <= cutoff {
			out = append(out, v)
		}
	}
	return out
}

// filterRatesBySeq 截取 seq <= cutoff 已注册的费率版本。
func filterRatesBySeq(rs []RateVersion, cutoff int64) []RateVersion {
	out := make([]RateVersion, 0, len(rs))
	for _, r := range rs {
		if r.Seq <= cutoff {
			out = append(out, r)
		}
	}
	return out
}

// SnapshotDiff 是两次重算（两个水位）的差异。
type SnapshotDiff struct {
	FromSeq        int64
	ToSeq          int64
	Deltas         map[string]int64 // participant_id -> 净额变化（分）
	NewSettlements []ReplayedSettlement
}

// DiffSnapshots 计算 b 相对 a 的差异；a 必须是较早水位。
func DiffSnapshots(a, b Snapshot) SnapshotDiff {
	d := SnapshotDiff{
		FromSeq: a.AsOfSeq, ToSeq: b.AsOfSeq,
		Deltas: map[string]int64{},
	}
	for id, bal := range b.Balances {
		d.Deltas[id] = bal - a.Balances[id]
	}
	for id, bal := range a.Balances {
		if _, ok := b.Balances[id]; !ok {
			d.Deltas[id] = -bal
		}
	}
	seen := map[string]bool{}
	for _, s := range a.Settlements {
		seen[s.Region+"|"+string(s.Period)] = true
	}
	for _, s := range b.Settlements {
		if !seen[s.Region+"|"+string(s.Period)] {
			d.NewSettlements = append(d.NewSettlements, s)
		}
	}
	return d
}
