package ledger

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

var shanghai = func() *time.Location {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return l
}()

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// ---- 金额与费率解析 ----

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"800.00", 80000}, {"0.01", 1}, {"7", 700}, {"-0.01", -1},
		{"1234567.89", 123456789}, {"+3.5", 350}, {"0.1", 10},
	}
	for _, c := range cases {
		got, err := ParseAmount(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseAmount(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "1.234", "1.2.3", "-", "1,00"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount(%q) should fail", bad)
		}
	}
}

func TestParseRateBPS(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0.0250", 250}, {"0.1", 1000}, {"1", 10000}, {"0.0001", 1}, {"0", 0},
	}
	for _, c := range cases {
		got, err := ParseRateBPS(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseRateBPS(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"0.00001", "-0.1", "x", ""} {
		if _, err := ParseRateBPS(bad); err == nil {
			t.Errorf("ParseRateBPS(%q) should fail", bad)
		}
	}
}

func TestFormatCents(t *testing.T) {
	if got := FormatCents(80000); got != "800.00" {
		t.Errorf("got %s", got)
	}
	if got := FormatCents(-1); got != "-0.01" {
		t.Errorf("got %s", got)
	}
}

// ---- 跨午夜周期归属 ----

func TestDateOfCrossMidnight(t *testing.T) {
	// UTC 16:30 = 上海次日 00:30，必须归入次日周期
	if got := DateOf(mustParseTime(t, "2026-09-09T16:30:00Z"), shanghai); got != "2026-09-10" {
		t.Errorf("cross-midnight attribution wrong: %s", got)
	}
	// UTC 15:59:59 = 上海 23:59:59，仍属当日
	if got := DateOf(mustParseTime(t, "2026-09-09T15:59:59Z"), shanghai); got != "2026-09-09" {
		t.Errorf("same-day attribution wrong: %s", got)
	}
	// 带原始偏移的本地时间
	if got := DateOf(mustParseTime(t, "2026-09-10T00:10:00+08:00"), shanghai); got != "2026-09-10" {
		t.Errorf("local offset attribution wrong: %s", got)
	}
}

// ---- 合同切换日 ----

func testContracts() []ContractVersion {
	return []ContractVersion{
		{ContractNo: "C-2026", Version: 1, EffectiveFrom: "2026-09-01",
			Terms: Terms{CreatorBPS: 4500, CoopBPS: 3500, PlatformBPS: 2000}, Seq: 1},
		{ContractNo: "C-2026", Version: 2, EffectiveFrom: "2026-09-10",
			Terms: Terms{CreatorBPS: 5000, CoopBPS: 3000, PlatformBPS: 2000}, Seq: 2},
	}
}

func TestSelectContractSwitchDay(t *testing.T) {
	cs := testContracts()
	v, ok := SelectContract(cs, "2026-09-09")
	if !ok || v.Version != 1 {
		t.Errorf("day before switch should use v1, got %+v", v)
	}
	v, ok = SelectContract(cs, "2026-09-10")
	if !ok || v.Version != 2 {
		t.Errorf("switch day should use v2, got %+v", v)
	}
	if _, ok = SelectContract(cs, "2026-08-31"); ok {
		t.Errorf("before any effective date should find nothing")
	}
}

func TestSelectRate(t *testing.T) {
	rs := []RateVersion{
		{ChannelID: "channel:mall", ProductGroup: "tea", FeeBPS: 250, EffectivePeriod: "2026-09-01", Seq: 3},
		{ChannelID: "channel:mall", ProductGroup: "tea", FeeBPS: 300, EffectivePeriod: "2026-09-15", Seq: 9},
		{ChannelID: "channel:mall", ProductGroup: "silk", FeeBPS: 500, EffectivePeriod: "2026-09-01", Seq: 4},
	}
	r, ok := SelectRate(rs, "channel:mall", "tea", "2026-09-10")
	if !ok || r.FeeBPS != 250 {
		t.Errorf("want 250, got %+v", r)
	}
	r, ok = SelectRate(rs, "channel:mall", "tea", "2026-09-15")
	if !ok || r.FeeBPS != 300 {
		t.Errorf("newer rate should win from its effective period, got %+v", r)
	}
	if _, ok = SelectRate(rs, "channel:mall", "ceramic", "2026-09-10"); ok {
		t.Errorf("unknown group should find no rate")
	}
}

// ---- 结算计算 ----

func testRates() []RateVersion {
	return []RateVersion{
		{ChannelID: "channel:mall", ProductGroup: "tea", FeeBPS: 250, EffectivePeriod: "2026-09-01", Seq: 3},
	}
}

func order(seq int64, id string, period Date, amount int64) OrderLine {
	return OrderLine{Seq: seq, OrderID: id, Period: period, ProductGroup: "tea",
		AmountCents: amount, CreatorID: "creator:li", CoopID: "coop:tea", ChannelID: "channel:mall"}
}

func TestComputeSettlementSaleLeg(t *testing.T) {
	st, err := ComputeSettlement(SettlementInput{
		Region: "CN-SH", Period: "2026-09-09", PlatformID: "platform:ops",
		Orders:    []OrderLine{order(10, "O-01", "2026-09-09", 80000)},
		Contracts: testContracts(), Rates: testRates(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Contract.Version != 1 {
		t.Errorf("period 09-09 must use contract v1, got v%d", st.Contract.Version)
	}
	if st.GrossCents != 80000 || !st.Balanced() {
		t.Fatalf("gross=%d balanced=%v", st.GrossCents, st.Balanced())
	}
	// 800.00：渠道费 2.5% = 20.00；净额 780.00 按 45/35/20 分
	want := map[string]int64{
		"creator:li":   35100,
		"coop:tea":     27300,
		"channel:mall": 2000,
		"platform:ops": 15600,
	}
	got := map[string]int64{}
	for _, p := range st.Postings {
		if p.Leg == LegSale && p.CreditCents > 0 {
			got[p.ParticipantID] += p.CreditCents
		}
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %d want %d", id, got[id], w)
		}
	}
	// 清算户借记全额
	var clearing int64
	for _, p := range st.Postings {
		if p.Account == "platform:ops:clearing" {
			clearing += p.DebitCents
		}
	}
	if clearing != 80000 {
		t.Errorf("clearing debit = %d, want 80000", clearing)
	}
}

func TestComputeSettlementSwitchDayAndIndependentReversal(t *testing.T) {
	orig := order(10, "O-01", "2026-09-09", 80000)
	st, err := ComputeSettlement(SettlementInput{
		Region: "CN-SH", Period: "2026-09-10", PlatformID: "platform:ops",
		Orders: []OrderLine{order(20, "O-02", "2026-09-10", 120000)},
		Returns: []ReturnLine{{Seq: 21, OrderID: "O-01", ProductGroup: "tea",
			AmountCents: 80000, CreatorID: "creator:li", CoopID: "coop:tea",
			ChannelID: "channel:mall", Original: &orig}},
		Contracts: testContracts(), Rates: testRates(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 切换日新销售必须用 v2
	if st.Contract.Version != 2 {
		t.Fatalf("switch-day sale must use v2, got v%d", st.Contract.Version)
	}
	if !st.Balanced() {
		t.Fatal("settlement must balance")
	}
	var saleCreator, revCreator, revChannel int64
	var revBasis *Basis
	for i, p := range st.Postings {
		switch {
		case p.Leg == LegSale && p.ParticipantID == "creator:li":
			saleCreator = p.CreditCents
		case p.Leg == LegReversal && p.ParticipantID == "creator:li":
			revCreator = p.DebitCents
			revBasis = &st.Postings[i].Basis
		case p.Leg == LegReversal && p.ParticipantID == "channel:mall":
			revChannel = p.DebitCents
		}
	}
	// 销售腿：1200.00，费 30.00，净 1170.00 按 50/30/20
	if saleCreator != 58500 {
		t.Errorf("sale creator = %d, want 58500 (v2 terms)", saleCreator)
	}
	// 冲正腿：必须按原订单周期（09-09，v1 45/35/20）冲回，与当日 v2 无关
	if revCreator != 35100 {
		t.Errorf("reversal creator = %d, want 35100 (original v1 terms)", revCreator)
	}
	if revChannel != 2000 {
		t.Errorf("reversal channel = %d, want 2000", revChannel)
	}
	if revBasis == nil || revBasis.OriginalContractVersion != 1 || revBasis.OriginalPeriod != "2026-09-09" {
		t.Errorf("reversal basis must cite original contract v1 / 2026-09-09, got %+v", revBasis)
	}
	// 冲正独立成腿：不得与销售轧差
	var saleLegs, revLegs int
	for _, p := range st.Postings {
		if p.Leg == LegSale {
			saleLegs++
		} else {
			revLegs++
		}
	}
	if saleLegs == 0 || revLegs == 0 {
		t.Errorf("reversal must be independent legs, sale=%d rev=%d", saleLegs, revLegs)
	}
}

func TestComputeSettlementResidualToPlatform(t *testing.T) {
	// 1 分钱在 45/35/20 下全部 floor 为 0，尾差必须归平台，总额守恒
	st, err := ComputeSettlement(SettlementInput{
		Region: "CN-SH", Period: "2026-09-09", PlatformID: "platform:ops",
		Orders:    []OrderLine{order(10, "O-01", "2026-09-09", 1)},
		Contracts: testContracts(), Rates: testRates(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var platform, total int64
	for _, p := range st.Postings {
		if p.Leg == LegSale && p.CreditCents > 0 {
			total += p.CreditCents
			if p.ParticipantID == "platform:ops" {
				platform = p.CreditCents
			}
		}
	}
	if total != 1 || platform != 1 {
		t.Errorf("residual must go to platform: total=%d platform=%d", total, platform)
	}
	if !st.Balanced() {
		t.Error("must balance")
	}
}

func TestComputeSettlementMissingContractOrRate(t *testing.T) {
	if _, err := ComputeSettlement(SettlementInput{
		Region: "CN-SH", Period: "2020-01-01", PlatformID: "platform:ops",
		Orders:    []OrderLine{order(1, "O", "2020-01-01", 100)},
		Contracts: testContracts(), Rates: testRates(),
	}); err == nil {
		t.Error("no effective contract should error")
	}
	if _, err := ComputeSettlement(SettlementInput{
		Region: "CN-SH", Period: "2026-09-09", PlatformID: "platform:ops",
		Orders: []OrderLine{{Seq: 1, OrderID: "O", Period: "2026-09-09", ProductGroup: "ceramic",
			AmountCents: 100, CreatorID: "c", CoopID: "k", ChannelID: "channel:mall"}},
		Contracts: testContracts(), Rates: testRates(),
	}); err == nil {
		t.Error("no channel rate should error")
	}
}

// ---- 哈希链 ----

var seedHash = []byte{0}

func mkEvent(seq int64, typ, orderID string, occurred time.Time, period Date, late bool, payload string, prev []byte) Event {
	e := Event{
		Seq: seq, EventID: fmt.Sprintf("00000000-0000-0000-0000-%012d", seq),
		Type: typ, RegionCode: "CN-SH", OrderID: orderID,
		OccurredAt: occurred, Period: period, Late: late, Payload: []byte(payload),
	}
	cp, err := CanonicalPayload(e.Payload)
	if err != nil {
		panic(err)
	}
	e.PrevHash = prev
	e.Hash = EventHash(prev, HashRecord{
		Seq: e.Seq, EventID: e.EventID, Type: e.Type, RegionCode: e.RegionCode,
		OrderID: e.OrderID, OccurredAt: e.OccurredAt, Period: e.Period, Late: e.Late, Payload: cp,
	})
	return e
}

func TestHashChainVerifyAndTamper(t *testing.T) {
	e1 := mkEvent(1, EventOrder, "O-01", mustParseTime(t, "2026-09-09T23:50:00+08:00"), "2026-09-09", false,
		`{"product_group":"tea","amount":"800.00","creator_id":"creator:li","coop_id":"coop:tea","channel_id":"channel:mall"}`, seedHash)
	e2 := mkEvent(2, EventReturn, "O-01", mustParseTime(t, "2026-09-10T00:05:00+08:00"), "2026-09-10", false,
		`{"product_group":"tea","amount":"800.00","creator_id":"creator:li","coop_id":"coop:tea","channel_id":"channel:mall"}`, e1.Hash)
	head, err := VerifyChain([]Event{e1, e2}, seedHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(head, e2.Hash) {
		t.Error("head must equal last event hash")
	}
	// 篡改金额 -> 断链
	e1.Payload = []byte(`{"product_group":"tea","amount":"900.00","creator_id":"creator:li","coop_id":"coop:tea","channel_id":"channel:mall"}`)
	if _, err := VerifyChain([]Event{e1, e2}, seedHash); err == nil {
		t.Error("tampered payload must break the chain")
	}
}

func TestCanonicalPayloadStable(t *testing.T) {
	a := []byte(`{"b":1,"a":"800.00"}`)
	b := []byte(`{"a":"800.00","b":1}`)
	ca, err := CanonicalPayload(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := CanonicalPayload(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca, cb) {
		t.Errorf("key order must not matter: %s vs %s", ca, cb)
	}
	// 数字字面量必须保留（800.00 不得变成 800）
	c, _ := CanonicalPayload([]byte(`{"amount":800.00}`))
	if !bytes.Contains(c, []byte("800.00")) {
		t.Errorf("numeric literal must be preserved, got %s", c)
	}
}

// ---- 重放 / 快照 / 差异 ----

func buildDemoTimeline(t *testing.T) []Event {
	t.Helper()
	var evs []Event
	prev := seedHash
	add := func(typ, orderID, occurred string, period Date, late bool, payload string) Event {
		e := mkEvent(int64(len(evs)+1), typ, orderID, mustParseTime(t, occurred), period, late, payload, prev)
		evs = append(evs, e)
		prev = e.Hash
		return e
	}
	add(EventContract, "", "2026-08-30T10:00:00+08:00", "2026-08-30", false,
		`{"contract_no":"C-2026","version":1,"effective_from":"2026-09-01","terms":{"creator_bps":4500,"coop_bps":3500,"platform_bps":2000}}`)
	add(EventContract, "", "2026-09-05T10:00:00+08:00", "2026-09-05", false,
		`{"contract_no":"C-2026","version":2,"effective_from":"2026-09-10","terms":{"creator_bps":5000,"coop_bps":3000,"platform_bps":2000}}`)
	add(EventChannelRate, "", "2026-08-30T10:05:00+08:00", "2026-08-30", false,
		`{"channel_id":"channel:mall","product_group":"tea","fee_rate":"0.0250","effective_period":"2026-09-01"}`)
	orderPayload := `{"product_group":"tea","amount":"800.00","creator_id":"creator:li","coop_id":"coop:tea","channel_id":"channel:mall"}`
	add(EventOrder, "O-01", "2026-09-09T23:50:00+08:00", "2026-09-09", false, orderPayload) // seq 4
	add(EventOrder, "O-02", "2026-09-10T00:10:00+08:00", "2026-09-10", false,
		`{"product_group":"tea","amount":"1200.00","creator_id":"creator:li","coop_id":"coop:tea","channel_id":"channel:mall"}`) // seq 5
	add(EventReturn, "O-01", "2026-09-10T00:05:00+08:00", "2026-09-10", false, orderPayload) // seq 6 跨午夜退货
	add(EventConfirmation, "", "2026-09-10T09:00:00+08:00", "2026-09-10", false,
		`{"region_code":"CN-SH","period":"2026-09-09"}`) // seq 7 确认 09-09
	add(EventOrder, "O-00", "2026-09-09T10:00:00+08:00", "2026-09-09", true, orderPayload) // seq 8 迟到旧事件
	add(EventConfirmation, "", "2026-09-11T09:00:00+08:00", "2026-09-11", false,
		`{"region_code":"CN-SH","period":"2026-09-10"}`) // seq 9 确认 09-10
	add(EventConfirmation, "", "2026-09-11T09:05:00+08:00", "2026-09-11", false,
		`{"region_code":"CN-SH","period":"2026-09-10"}`) // seq 10 重复确认（不同 event_id，重放只算一次）
	return evs
}

func TestReplaySnapshotAndDiff(t *testing.T) {
	evs := buildDemoTimeline(t)

	// 快照 1：只确认到 09-09（seq 7）
	s1, err := Replay(evs, 7, "platform:ops")
	if err != nil {
		t.Fatal(err)
	}
	if got := s1.Balances["creator:li"]; got != 35100 {
		t.Errorf("creator at seq7 = %d, want 35100", got)
	}
	if len(s1.Settlements) != 1 || s1.Settlements[0].ContractVersion != 1 {
		t.Fatalf("settlements at seq7 = %+v", s1.Settlements)
	}

	// 快照 2：确认到 09-10（seq 10，含重复确认与迟到事件）
	s2, err := Replay(evs, 10, "platform:ops")
	if err != nil {
		t.Fatal(err)
	}
	// 迟到事件 O-00（seq 8, late）绝不影响余额
	// 重复确认（seq 10）不重复记账
	if got := s2.Balances["creator:li"]; got != 58500 {
		t.Errorf("creator at seq10 = %d, want 58500 (35100 + 58500 - 35100)", got)
	}
	if got := s2.Balances["coop:tea"]; got != 35100 {
		t.Errorf("coop at seq10 = %d, want 35100", got)
	}
	if got := s2.Balances["channel:mall"]; got != 3000 {
		t.Errorf("channel at seq10 = %d, want 3000", got)
	}
	if len(s2.Settlements) != 2 {
		t.Fatalf("duplicate confirmation must settle once, got %+v", s2.Settlements)
	}
	// 全部参与方净额之和为 0（复式记账守恒，含平台清算户）
	var sum int64
	for _, v := range s2.Balances {
		sum += v
	}
	if sum != 0 {
		t.Errorf("double-entry invariant: balances must sum to 0, got %d", sum)
	}

	// 差异：seq7 -> seq10
	d := DiffSnapshots(s1, s2)
	if d.Deltas["creator:li"] != 23400 {
		t.Errorf("creator delta = %d, want 23400 (58500-35100)", d.Deltas["creator:li"])
	}
	if len(d.NewSettlements) != 1 || d.NewSettlements[0].Period != "2026-09-10" {
		t.Errorf("new settlements = %+v", d.NewSettlements)
	}
}

func TestReplayExcludesUnconfirmed(t *testing.T) {
	evs := buildDemoTimeline(t)
	// seq 6：尚未有任何确认 -> 全部余额为 0
	s, err := Replay(evs, 6, "platform:ops")
	if err != nil {
		t.Fatal(err)
	}
	for id, v := range s.Balances {
		if v != 0 {
			t.Errorf("no confirmation yet: %s balance %d must be 0", id, v)
		}
	}
}

// 确认之后才注册的追溯性合同版本，不得改变历史结算的重放结果——
// 每个周期按其确认序号截取版本，与在线确认路径严格同构。
func TestReplayRetroactiveContractDoesNotRewriteHistory(t *testing.T) {
	evs := buildDemoTimeline(t)
	prev := evs[len(evs)-1].Hash
	retro := mkEvent(11, EventContract, "", mustParseTime(t, "2026-09-12T10:00:00+08:00"), "2026-09-12", false,
		`{"contract_no":"C-2026","version":3,"effective_from":"2026-09-01","terms":{"creator_bps":9000,"coop_bps":500,"platform_bps":500}}`, prev)
	evs = append(evs, retro)

	s, err := Replay(evs, 11, "platform:ops")
	if err != nil {
		t.Fatal(err)
	}
	// 若重放误用全局水位选版本，v3（90% 创作者）会改写两个周期的结果
	if got := s.Balances["creator:li"]; got != 58500 {
		t.Errorf("retroactive contract rewrote history: creator = %d, want 58500", got)
	}
	if got := s.Balances["coop:tea"]; got != 35100 {
		t.Errorf("retroactive contract rewrote history: coop = %d, want 35100", got)
	}
}
