package ledger

// SelectContract 选出在 period 当日生效的合同版本：
// EffectiveFrom <= period 中切换日最新者；并列时取版本号更高者。
// 这就是"合同切换日使用正确版本"的判定函数。
func SelectContract(versions []ContractVersion, period Date) (ContractVersion, bool) {
	var best ContractVersion
	found := false
	for _, v := range versions {
		if v.EffectiveFrom > period {
			continue
		}
		if !found ||
			v.EffectiveFrom > best.EffectiveFrom ||
			(v.EffectiveFrom == best.EffectiveFrom && v.Version > best.Version) ||
			(v.EffectiveFrom == best.EffectiveFrom && v.Version == best.Version && v.Seq > best.Seq) {
			best = v
			found = true
		}
	}
	return best, found
}

// SelectRate 选出某渠道+商品组在 period 当日生效的费率版本：
// EffectivePeriod <= period 中最新者；并列时取注册序号（seq）更大者。
func SelectRate(rates []RateVersion, channelID, productGroup string, period Date) (RateVersion, bool) {
	var best RateVersion
	found := false
	for _, r := range rates {
		if r.ChannelID != channelID || r.ProductGroup != productGroup {
			continue
		}
		if r.EffectivePeriod > period {
			continue
		}
		if !found ||
			r.EffectivePeriod > best.EffectivePeriod ||
			(r.EffectivePeriod == best.EffectivePeriod && r.Seq > best.Seq) {
			best = r
			found = true
		}
	}
	return best, found
}
