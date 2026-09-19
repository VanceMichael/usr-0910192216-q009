// Package ledger 是分账账本的纯函数领域核心：不依赖数据库与网络，
// 给定相同输入必然产生相同结果，因此结算确认、审计重放、时点快照
// 都复用同一份实现，从根上保证"重算一致"。
package ledger

import (
	"fmt"
	"time"

	_ "time/tzdata" // 内嵌 IANA 时区库，保证 alpine/scratch 环境中周期归属一致
)

// Date 是 ISO 日历日（YYYY-MM-DD）。字典序与时间序一致，可直接用 < 比较。
type Date string

// ParseDate 校验并规范化 YYYY-MM-DD。
func ParseDate(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", fmt.Errorf("invalid date %q: %w", s, err)
	}
	return Date(t.Format("2006-01-02")), nil
}

// DateOf 把时刻折算到指定时区下的日历日——跨午夜归属的唯一依据。
func DateOf(t time.Time, loc *time.Location) Date {
	return Date(t.In(loc).Format("2006-01-02"))
}

// 事件类型，与 events.type 的 CHECK 约束一致。
const (
	EventOrder        = "order"
	EventReturn       = "return"
	EventChannelRate  = "channel_rate"
	EventContract     = "contract_version"
	EventConfirmation = "settlement_confirmation"
)

// Event 是时间线上的一条记录（含数据库投影字段）。
type Event struct {
	Seq        int64
	EventID    string
	Type       string
	RegionCode string
	OrderID    string
	OccurredAt time.Time
	Period     Date
	Late       bool
	Payload    []byte // 原始 jsonb
	PrevHash   []byte
	Hash       []byte
}

// Terms 是合同分成条款，单位万分之一（bps），三者合计必须等于 10000。
type Terms struct {
	CreatorBPS  int64 `json:"creator_bps"`
	CoopBPS     int64 `json:"coop_bps"`
	PlatformBPS int64 `json:"platform_bps"`
}

// Validate 校验条款自洽。
func (t Terms) Validate() error {
	if t.CreatorBPS < 0 || t.CoopBPS < 0 || t.PlatformBPS < 0 {
		return fmt.Errorf("terms bps must be non-negative: %+v", t)
	}
	if t.CreatorBPS+t.CoopBPS+t.PlatformBPS != 10000 {
		return fmt.Errorf("terms bps must sum to 10000, got %d", t.CreatorBPS+t.CoopBPS+t.PlatformBPS)
	}
	return nil
}

// ContractVersion 是合同的一个版本，EffectiveFrom 即切换日。
type ContractVersion struct {
	ContractNo    string
	Version       int
	EffectiveFrom Date
	Terms         Terms
	Seq           int64 // 注册该版本的事件 seq
}

// RateVersion 是渠道费率的一个版本（按渠道+商品组生效）。
type RateVersion struct {
	ChannelID       string
	ProductGroup    string
	FeeBPS          int64
	EffectivePeriod Date
	Seq             int64
}

// OrderLine 是参与结算的一笔订单（金额已解析为分）。
type OrderLine struct {
	Seq          int64
	OrderID      string
	Period       Date
	ProductGroup string
	AmountCents  int64
	CreatorID    string
	CoopID       string
	ChannelID    string
}

// ReturnLine 是一笔退货（独立冲正）。Original 指向原订单，
// 用于按原周期的合同与费率冲回，保证切换日前后解释一致。
type ReturnLine struct {
	Seq          int64
	OrderID      string
	ProductGroup string
	AmountCents  int64
	CreatorID    string
	CoopID       string
	ChannelID    string
	Original     *OrderLine // nil 表示原订单不在时间线内
}
