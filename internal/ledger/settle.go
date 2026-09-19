package ledger

import (
	"fmt"
	"sort"
)

// 分录腿：销售腿与退货冲正腿严格分离，绝不轧差——
// "后续退货形成独立冲正"在账本结构上即可证明。
const (
	LegSale     = "sale"
	LegReversal = "reversal"
)

// Basis 是分账指令的公开依据：参与方可以看到自己金额的合同
// 版本、适用比例与来源事件序号，但看不到他方金额。
type Basis struct {
	Region                  string  `json:"region"`
	Period                  Date    `json:"period"`
	ContractNo              string  `json:"contract_no"`
	ContractVersion         int     `json:"contract_version"`
	BPS                     int64   `json:"bps,omitempty"`
	FeeBPS                  int64   `json:"fee_bps,omitempty"`
	EventSeqs               []int64 `json:"event_seqs"`
	OriginalPeriod          Date    `json:"original_period,omitempty"`
	OriginalContractVersion int     `json:"original_contract_version,omitempty"`
	Note                    string  `json:"note,omitempty"`
}

// Posting 是一条复式记账分录：借、贷恰其一为正（数据库 CHECK 同构）。
type Posting struct {
	Leg           string
	Account       string
	ParticipantID string
	DebitCents    int64
	CreditCents   int64
	Basis         Basis
}

// SettlementInput 是一次周期结算的全部输入（调用方已按审计序号水位过滤）。
type SettlementInput struct {
	Region     string
	Period     Date
	PlatformID string
	Orders     []OrderLine
	Returns    []ReturnLine
	Contracts  []ContractVersion
	Rates      []RateVersion
}

// Settlement 是计算结果：冻结的合同解释 + 全部分录。
type Settlement struct {
	Region        string
	Period        Date
	Contract      ContractVersion
	GrossCents    int64
	ReturnedCents int64
	Postings      []Posting
}

// Balanced 校验借贷平衡（借方合计 == 贷方合计）。
func (s Settlement) Balanced() bool {
	var d, c int64
	for _, p := range s.Postings {
		d += p.DebitCents
		c += p.CreditCents
	}
	return d == c
}

// shares 是一笔金额的四路切分。
type shares struct {
	channel, creator, coop, platform int64
}

// split 先扣渠道费再按合同 bps 分净额；floor 尾差归平台，
// 因此四份之和恒等于 amount，借贷天然平衡。
func split(amount, feeBPS int64, t Terms) shares {
	fee := amount * feeBPS / 10000
	net := amount - fee
	creator := net * t.CreatorBPS / 10000
	coop := net * t.CoopBPS / 10000
	platform := net - creator - coop
	return shares{channel: fee, creator: creator, coop: coop, platform: platform}
}

// bucket 是按（参与方, 腿, 费率/合同上下文）聚合的中间态。
type bucket struct {
	participantID string
	role          string // creator | coop | channel | platform
	leg           string
	bps           int64
	feeBPS        int64
	origPeriod    Date
	origVersion   int
	note          string
	amount        int64
	seqs          []int64
}

func (b *bucket) add(amount int64, seq int64) {
	b.amount += amount
	b.seqs = append(b.seqs, seq)
}

func accountFor(participantID, role string) string {
	suffix := map[string]string{
		"creator": "receivable", "coop": "receivable",
		"channel": "fee", "platform": "revenue",
	}[role]
	return participantID + ":" + suffix
}

// ComputeSettlement 计算一个区域一个周期的分账指令。纯函数：
// 相同输入必得相同输出，审计重放与在线确认因此必然一致。
func ComputeSettlement(in SettlementInput) (Settlement, error) {
	if in.PlatformID == "" {
		return Settlement{}, fmt.Errorf("platform id is required")
	}
	contract, ok := SelectContract(in.Contracts, in.Period)
	if !ok {
		return Settlement{}, fmt.Errorf("no contract effective for period %s", in.Period)
	}
	out := Settlement{Region: in.Region, Period: in.Period, Contract: contract}
	buckets := map[string]*bucket{}
	get := func(key, participantID, role, leg string, bps, feeBPS int64, origPeriod Date, origVersion int, note string) *bucket {
		b, ok := buckets[key]
		if !ok {
			b = &bucket{participantID: participantID, role: role, leg: leg,
				bps: bps, feeBPS: feeBPS, origPeriod: origPeriod, origVersion: origVersion, note: note}
			buckets[key] = b
		}
		return b
	}

	// ---- 销售腿：按结算周期的合同与费率 ----
	for _, o := range in.Orders {
		rate, ok := SelectRate(in.Rates, o.ChannelID, o.ProductGroup, in.Period)
		if !ok {
			return Settlement{}, fmt.Errorf("no channel rate for %s/%s at %s", o.ChannelID, o.ProductGroup, in.Period)
		}
		sh := split(o.AmountCents, rate.FeeBPS, contract.Terms)
		out.GrossCents += o.AmountCents
		t := contract.Terms
		get("sale|creator|"+o.CreatorID, o.CreatorID, "creator", LegSale, t.CreatorBPS, 0, "", 0, "").add(sh.creator, o.Seq)
		get("sale|coop|"+o.CoopID, o.CoopID, "coop", LegSale, t.CoopBPS, 0, "", 0, "").add(sh.coop, o.Seq)
		get(fmt.Sprintf("sale|channel|%s|%d", o.ChannelID, rate.FeeBPS), o.ChannelID, "channel", LegSale, 0, rate.FeeBPS, "", 0, "").add(sh.channel, o.Seq)
		get("sale|platform", in.PlatformID, "platform", LegSale, t.PlatformBPS, 0, "", 0, "").add(sh.platform, o.Seq)
	}

	// ---- 冲正腿：按原订单周期的合同与费率冲回，保证切换日解释一致 ----
	for _, r := range in.Returns {
		ctxContract := contract
		ratePeriod := in.Period
		var origPeriod Date
		origVersion := 0
		note := ""
		if r.Original != nil {
			ratePeriod = r.Original.Period
			if oc, ok := SelectContract(in.Contracts, r.Original.Period); ok {
				ctxContract = oc
				origPeriod = r.Original.Period
				origVersion = oc.Version
			} else {
				note = "original_contract_missing"
			}
		} else {
			note = "original_order_missing"
		}
		rate, ok := SelectRate(in.Rates, r.ChannelID, r.ProductGroup, ratePeriod)
		if !ok {
			return Settlement{}, fmt.Errorf("no channel rate for %s/%s at %s", r.ChannelID, r.ProductGroup, ratePeriod)
		}
		sh := split(r.AmountCents, rate.FeeBPS, ctxContract.Terms)
		out.ReturnedCents += r.AmountCents
		t := ctxContract.Terms
		ctx := fmt.Sprintf("|%s|%d", origPeriod, origVersion)
		get("rev|creator|"+r.CreatorID+ctx, r.CreatorID, "creator", LegReversal, t.CreatorBPS, 0, origPeriod, origVersion, note).add(sh.creator, r.Seq)
		get("rev|coop|"+r.CoopID+ctx, r.CoopID, "coop", LegReversal, t.CoopBPS, 0, origPeriod, origVersion, note).add(sh.coop, r.Seq)
		get(fmt.Sprintf("rev|channel|%s|%d", r.ChannelID, rate.FeeBPS)+ctx, r.ChannelID, "channel", LegReversal, 0, rate.FeeBPS, origPeriod, origVersion, note).add(sh.channel, r.Seq)
		get("rev|platform"+ctx, in.PlatformID, "platform", LegReversal, t.PlatformBPS, 0, origPeriod, origVersion, note).add(sh.platform, r.Seq)
	}

	// ---- 装配分录：确定性顺序，借贷平衡 ----
	clearing := in.PlatformID + ":clearing"
	base := Basis{Region: in.Region, Period: in.Period, ContractNo: contract.ContractNo, ContractVersion: contract.Version}
	if out.GrossCents > 0 {
		out.Postings = append(out.Postings, Posting{
			Leg: LegSale, Account: clearing, ParticipantID: in.PlatformID,
			DebitCents: out.GrossCents,
			Basis:      withSeqs(base, allSeqs(buckets, LegSale)),
		})
	}
	appendBuckets := func(leg string, credit bool) {
		keys := make([]string, 0, len(buckets))
		for k, b := range buckets {
			if b.leg == leg && b.amount > 0 {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			b := buckets[k]
			basis := base
			basis.BPS = b.bps
			basis.FeeBPS = b.feeBPS
			basis.EventSeqs = sortedSeqs(b.seqs)
			basis.OriginalPeriod = b.origPeriod
			basis.OriginalContractVersion = b.origVersion
			basis.Note = b.note
			p := Posting{
				Leg: leg, Account: accountFor(b.participantID, b.role),
				ParticipantID: b.participantID, Basis: basis,
			}
			if credit {
				p.CreditCents = b.amount
			} else {
				p.DebitCents = b.amount
			}
			out.Postings = append(out.Postings, p)
		}
	}
	appendBuckets(LegSale, true)      // 销售腿贷记各参与方
	appendBuckets(LegReversal, false) // 冲正腿借记各参与方
	if out.ReturnedCents > 0 {
		out.Postings = append(out.Postings, Posting{
			Leg: LegReversal, Account: clearing, ParticipantID: in.PlatformID,
			CreditCents: out.ReturnedCents,
			Basis:       withSeqs(base, allSeqs(buckets, LegReversal)),
		})
	}
	if !out.Balanced() {
		return Settlement{}, fmt.Errorf("internal error: unbalanced settlement")
	}
	return out, nil
}

func withSeqs(b Basis, seqs []int64) Basis {
	b.EventSeqs = seqs
	return b
}

func allSeqs(buckets map[string]*bucket, leg string) []int64 {
	var seqs []int64
	for _, b := range buckets {
		if b.leg == leg {
			seqs = append(seqs, b.seqs...)
		}
	}
	return sortedSeqs(seqs)
}

func sortedSeqs(seqs []int64) []int64 {
	out := make([]int64, len(seqs))
	copy(out, seqs)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
