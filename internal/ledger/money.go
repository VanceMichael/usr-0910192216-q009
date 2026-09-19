package ledger

import (
	"fmt"
	"strings"
)

// ParseAmount 把十进制金额字符串精确解析为分（int64），拒绝浮点。
// 允许最多 2 位小数，例如 "800.00"、"-0.01"、"7"。
func ParseAmount(s string) (int64, error) {
	neg, intPart, fracPart, err := splitDecimal(s, 2)
	if err != nil {
		return 0, err
	}
	whole, err := atoiDigits(intPart)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	cents := whole * 100
	if len(fracPart) > 0 {
		frac, err := atoiDigits(fracPart)
		if err != nil {
			return 0, fmt.Errorf("invalid amount %q", s)
		}
		if len(fracPart) == 1 {
			frac *= 10
		}
		cents += frac
	}
	if neg {
		cents = -cents
	}
	return cents, nil
}

// ParseRateBPS 把费率字符串解析为万分之一（bps）。
// 遵循分账规则的 fee_scale=4：最多 4 位小数，"0.0250" -> 250。
func ParseRateBPS(s string) (int64, error) {
	neg, intPart, fracPart, err := splitDecimal(s, 4)
	if err != nil {
		return 0, err
	}
	whole, err := atoiDigits(intPart)
	if err != nil {
		return 0, fmt.Errorf("invalid rate %q", s)
	}
	bps := whole * 10000
	if len(fracPart) > 0 {
		frac, err := atoiDigits(fracPart)
		if err != nil {
			return 0, fmt.Errorf("invalid rate %q", s)
		}
		for i := len(fracPart); i < 4; i++ {
			frac *= 10
		}
		bps += frac
	}
	if neg {
		return 0, fmt.Errorf("rate must not be negative: %q", s)
	}
	return bps, nil
}

// FormatCents 把分格式化为定点字符串，供 API 输出。
func FormatCents(c int64) string {
	neg := c < 0
	if neg {
		c = -c
	}
	s := fmt.Sprintf("%d.%02d", c/100, c%100)
	if neg {
		return "-" + s
	}
	return s
}

// splitDecimal 拆分符号、整数与小数部分，并限制小数位数。
func splitDecimal(s string, maxFrac int) (neg bool, intPart, fracPart string, err error) {
	if s == "" {
		return false, "", "", fmt.Errorf("empty decimal")
	}
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	if s == "" {
		return false, "", "", fmt.Errorf("empty decimal")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return false, "", "", fmt.Errorf("invalid decimal %q", s)
	}
	intPart = parts[0]
	if intPart == "" {
		intPart = "0"
	}
	if len(parts) == 2 {
		fracPart = parts[1]
		if fracPart == "" {
			return false, "", "", fmt.Errorf("invalid decimal %q", s)
		}
		if len(fracPart) > maxFrac {
			return false, "", "", fmt.Errorf("too many fractional digits (max %d) in %q", maxFrac, s)
		}
	}
	return neg, intPart, fracPart, nil
}

func atoiDigits(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty digits")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-digit %q", r)
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}
