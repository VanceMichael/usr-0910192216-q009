package ledger

import (
	"encoding/json"
	"fmt"
)

// 事件 payload 的线上格式。金额与费率以字符串承载，全程不经过浮点。

type orderPayload struct {
	ProductGroup string `json:"product_group"`
	Amount       string `json:"amount"`
	CreatorID    string `json:"creator_id"`
	CoopID       string `json:"coop_id"`
	ChannelID    string `json:"channel_id"`
}

// OrderLineFromEvent 把 order 事件解码为结算输入行。
func OrderLineFromEvent(ev Event) (OrderLine, error) {
	if ev.Type != EventOrder {
		return OrderLine{}, fmt.Errorf("event %d is %q, not order", ev.Seq, ev.Type)
	}
	var p orderPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return OrderLine{}, fmt.Errorf("event %d payload: %w", ev.Seq, err)
	}
	amount, err := ParseAmount(p.Amount)
	if err != nil {
		return OrderLine{}, fmt.Errorf("event %d amount: %w", ev.Seq, err)
	}
	if amount <= 0 {
		return OrderLine{}, fmt.Errorf("event %d: order amount must be positive", ev.Seq)
	}
	if p.ProductGroup == "" || p.CreatorID == "" || p.CoopID == "" || p.ChannelID == "" {
		return OrderLine{}, fmt.Errorf("event %d: order payload missing required fields", ev.Seq)
	}
	return OrderLine{
		Seq: ev.Seq, OrderID: ev.OrderID, Period: ev.Period,
		ProductGroup: p.ProductGroup, AmountCents: amount,
		CreatorID: p.CreatorID, CoopID: p.CoopID, ChannelID: p.ChannelID,
	}, nil
}

// ReturnLineFromEvent 把 return 事件解码为冲正输入行；original 为原订单（可空）。
func ReturnLineFromEvent(ev Event, original *OrderLine) (ReturnLine, error) {
	if ev.Type != EventReturn {
		return ReturnLine{}, fmt.Errorf("event %d is %q, not return", ev.Seq, ev.Type)
	}
	var p orderPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return ReturnLine{}, fmt.Errorf("event %d payload: %w", ev.Seq, err)
	}
	amount, err := ParseAmount(p.Amount)
	if err != nil {
		return ReturnLine{}, fmt.Errorf("event %d amount: %w", ev.Seq, err)
	}
	if amount <= 0 {
		return ReturnLine{}, fmt.Errorf("event %d: return amount must be positive", ev.Seq)
	}
	if ev.OrderID == "" {
		return ReturnLine{}, fmt.Errorf("event %d: return must reference an order_id", ev.Seq)
	}
	if p.ProductGroup == "" || p.CreatorID == "" || p.CoopID == "" || p.ChannelID == "" {
		return ReturnLine{}, fmt.Errorf("event %d: return payload missing required fields", ev.Seq)
	}
	return ReturnLine{
		Seq: ev.Seq, OrderID: ev.OrderID,
		ProductGroup: p.ProductGroup, AmountCents: amount,
		CreatorID: p.CreatorID, CoopID: p.CoopID, ChannelID: p.ChannelID,
		Original: original,
	}, nil
}

type contractPayload struct {
	ContractNo    string `json:"contract_no"`
	Version       int    `json:"version"`
	EffectiveFrom string `json:"effective_from"`
	Terms         Terms  `json:"terms"`
}

// ContractFromEvent 把 contract_version 事件解码为合同版本。
func ContractFromEvent(ev Event) (ContractVersion, error) {
	if ev.Type != EventContract {
		return ContractVersion{}, fmt.Errorf("event %d is %q, not contract_version", ev.Seq, ev.Type)
	}
	var p contractPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return ContractVersion{}, fmt.Errorf("event %d payload: %w", ev.Seq, err)
	}
	eff, err := ParseDate(p.EffectiveFrom)
	if err != nil {
		return ContractVersion{}, fmt.Errorf("event %d effective_from: %w", ev.Seq, err)
	}
	if p.ContractNo == "" || p.Version < 1 {
		return ContractVersion{}, fmt.Errorf("event %d: invalid contract_no/version", ev.Seq)
	}
	if err := p.Terms.Validate(); err != nil {
		return ContractVersion{}, fmt.Errorf("event %d terms: %w", ev.Seq, err)
	}
	return ContractVersion{
		ContractNo: p.ContractNo, Version: p.Version,
		EffectiveFrom: eff, Terms: p.Terms, Seq: ev.Seq,
	}, nil
}

type ratePayload struct {
	ChannelID       string `json:"channel_id"`
	ProductGroup    string `json:"product_group"`
	FeeRate         string `json:"fee_rate"`
	EffectivePeriod string `json:"effective_period"`
}

// RateFromEvent 把 channel_rate 事件解码为费率版本。
func RateFromEvent(ev Event) (RateVersion, error) {
	if ev.Type != EventChannelRate {
		return RateVersion{}, fmt.Errorf("event %d is %q, not channel_rate", ev.Seq, ev.Type)
	}
	var p ratePayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return RateVersion{}, fmt.Errorf("event %d payload: %w", ev.Seq, err)
	}
	bps, err := ParseRateBPS(p.FeeRate)
	if err != nil {
		return RateVersion{}, fmt.Errorf("event %d fee_rate: %w", ev.Seq, err)
	}
	eff, err := ParseDate(p.EffectivePeriod)
	if err != nil {
		return RateVersion{}, fmt.Errorf("event %d effective_period: %w", ev.Seq, err)
	}
	if p.ChannelID == "" || p.ProductGroup == "" {
		return RateVersion{}, fmt.Errorf("event %d: rate payload missing channel_id/product_group", ev.Seq)
	}
	return RateVersion{
		ChannelID: p.ChannelID, ProductGroup: p.ProductGroup,
		FeeBPS: bps, EffectivePeriod: eff, Seq: ev.Seq,
	}, nil
}

// ConfirmationPayload 是结算确认事件的载荷。
type ConfirmationPayload struct {
	RegionCode string `json:"region_code"`
	Period     string `json:"period"`
}

// ConfirmationFromEvent 解码结算确认事件，返回区域与周期。
func ConfirmationFromEvent(ev Event) (string, Date, error) {
	if ev.Type != EventConfirmation {
		return "", "", fmt.Errorf("event %d is %q, not settlement_confirmation", ev.Seq, ev.Type)
	}
	var p ConfirmationPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return "", "", fmt.Errorf("event %d payload: %w", ev.Seq, err)
	}
	period, err := ParseDate(p.Period)
	if err != nil {
		return "", "", fmt.Errorf("event %d period: %w", ev.Seq, err)
	}
	if p.RegionCode == "" {
		return "", "", fmt.Errorf("event %d: confirmation missing region_code", ev.Seq)
	}
	return p.RegionCode, period, nil
}
