// Package seed 把 contracts/ 下的规则与合同版本载入数据库：
// 区域、参与方直接插入（幂等）；合同版本与渠道费率作为事件写入
// 不可变时间线（确定性 event_id，重复执行是空操作），
// 因此合同解释从第一条审计序号起就有链上证据。
package seed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"example.com/culture-sharing/internal/ledger"
	"example.com/culture-sharing/internal/store"
)

type rulesFile struct {
	Timezone      string   `json:"timezone"`
	FeeScale      int      `json:"fee_scale"`
	ProductGroups []string `json:"product_groups"`
}

type versionsFile struct {
	Regions []struct {
		Code     string `json:"code"`
		Timezone string `json:"timezone"`
	} `json:"regions"`
	Participants []struct {
		ID          string `json:"id"`
		Kind        string `json:"kind"`
		DisplayName string `json:"display_name"`
		Token       string `json:"token"`
	} `json:"participants"`
	Contracts []struct {
		ContractNo    string       `json:"contract_no"`
		Version       int          `json:"version"`
		EffectiveFrom string       `json:"effective_from"`
		Terms         ledger.Terms `json:"terms"`
		RegisteredAt  string       `json:"registered_at"`
	} `json:"contracts"`
	ChannelRates []struct {
		ChannelID       string `json:"channel_id"`
		ProductGroup    string `json:"product_group"`
		FeeRate         string `json:"fee_rate"`
		EffectivePeriod string `json:"effective_period"`
		RegisteredAt    string `json:"registered_at"`
	} `json:"channel_rates"`
}

// Run 执行种子加载；可重复运行。
func Run(ctx context.Context, st *store.Store, rulesPath, versionsPath string) error {
	var rules rulesFile
	if err := readJSON(rulesPath, &rules); err != nil {
		return fmt.Errorf("read rules: %w", err)
	}
	if rules.FeeScale != 4 {
		return fmt.Errorf("rules fee_scale must be 4, got %d", rules.FeeScale)
	}
	var vf versionsFile
	if err := readJSON(versionsPath, &vf); err != nil {
		return fmt.Errorf("read versions: %w", err)
	}
	if len(vf.Regions) == 0 {
		return fmt.Errorf("versions file must define at least one region")
	}

	groups := map[string]bool{}
	for _, g := range rules.ProductGroups {
		groups[g] = true
	}

	for _, r := range vf.Regions {
		if r.Timezone != rules.Timezone {
			return fmt.Errorf("region %s timezone %q does not match rules timezone %q",
				r.Code, r.Timezone, rules.Timezone)
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO regions (code, timezone)
			VALUES ($1, $2) ON CONFLICT (code) DO NOTHING`, r.Code, r.Timezone); err != nil {
			return err
		}
	}
	for _, p := range vf.Participants {
		sum := sha256.Sum256([]byte(p.Token))
		if _, err := st.Pool.Exec(ctx, `INSERT INTO participants (id, kind, display_name, token_hash)
			VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`,
			p.ID, p.Kind, p.DisplayName, sum[:]); err != nil {
			return err
		}
	}

	region := vf.Regions[0].Code
	for _, c := range vf.Contracts {
		if err := c.Terms.Validate(); err != nil {
			return fmt.Errorf("contract %s v%d: %w", c.ContractNo, c.Version, err)
		}
		payload, _ := json.Marshal(map[string]any{
			"contract_no": c.ContractNo, "version": c.Version,
			"effective_from": c.EffectiveFrom, "terms": c.Terms,
		})
		if err := insertSeedEvent(ctx, st, region,
			fmt.Sprintf("contract/%s/v%d", c.ContractNo, c.Version),
			ledger.EventContract, c.RegisteredAt, payload); err != nil {
			return err
		}
	}
	for _, rt := range vf.ChannelRates {
		if !groups[rt.ProductGroup] {
			return fmt.Errorf("rate product_group %q not in rules product_groups", rt.ProductGroup)
		}
		if _, err := ledger.ParseRateBPS(rt.FeeRate); err != nil {
			return fmt.Errorf("rate %s/%s: %w", rt.ChannelID, rt.ProductGroup, err)
		}
		payload, _ := json.Marshal(map[string]string{
			"channel_id": rt.ChannelID, "product_group": rt.ProductGroup,
			"fee_rate": rt.FeeRate, "effective_period": rt.EffectivePeriod,
		})
		if err := insertSeedEvent(ctx, st, region,
			fmt.Sprintf("rate/%s/%s/%s", rt.ChannelID, rt.ProductGroup, rt.EffectivePeriod),
			ledger.EventChannelRate, rt.RegisteredAt, payload); err != nil {
			return err
		}
	}
	return nil
}

// insertSeedEvent 用确定性 event_id 写入种子事件；重复执行为幂等空操作。
func insertSeedEvent(ctx context.Context, st *store.Store, region, name, eventType, occurredAt string, payload []byte) error {
	ts, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		return fmt.Errorf("seed %s occurred_at: %w", name, err)
	}
	res, err := st.InsertEvent(ctx, store.EventInput{
		EventID: seedUUID(name), Type: eventType, RegionCode: region,
		OccurredAt: ts, Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("seed event %s: %w", name, err)
	}
	_ = res
	return nil
}

// seedUUID 由种子内容名生成确定性 UUID（v5 风格），保证重复种子不产生新事件。
func seedUUID(name string) string {
	sum := sha256.Sum256([]byte("culture-sharing/seed/v1/" + name))
	b := make([]byte, 16)
	copy(b, sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
