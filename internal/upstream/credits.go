package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CreditOverview 是账号的积分余额概览（官方控制台「套餐概览」）。
//
// 端点：GET {snap-access}/snap-manager/v1/package/overview
// 用账号自己的 STS 临时 AK/SK 签名即可，不需要浏览器 Cookie。
type CreditOverview struct {
	// Total 账户总积分（赠送 + 基础包 + 按需包合计）。
	Total float64 `json:"total"`
	// Remain 剩余积分。
	Remain float64 `json:"remain"`
	// Used 已使用积分。
	Used float64 `json:"used"`
	// BonusRemain 赠送积分剩余（每日签到、新用户等赠送的部分）。
	BonusRemain float64 `json:"bonus_remain"`
	// BasicRemain 基础包剩余积分。
	BasicRemain float64 `json:"basic_remain"`
	// Expiring 即将过期的积分（赠送积分通常 30 天有效）。
	Expiring float64 `json:"expiring"`
}

// creditPackageRaw 是接口返回的单类积分包。
type creditPackageRaw struct {
	Amount   float64 `json:"package_credit_amount"`
	Used     float64 `json:"package_credit_used"`
	Remain   float64 `json:"package_credit_remain"`
	Expiring float64 `json:"expiring_credit_amount"`
}

// FetchCreditOverview 查询账号积分余额。
func (c *Client) FetchCreditOverview(ctx context.Context, cred SignCredential) (CreditOverview, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.snapURL(EpPackageOverview), nil)
	if err != nil {
		return CreditOverview{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	req.Header.Set("Agent-Type", "PromptCenter")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, nil, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return CreditOverview{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return CreditOverview{}, &ApiError{
			Code: resp.StatusCode, Status: resp.StatusCode,
			Path: EpPackageOverview, Message: truncateStr(string(raw), 300),
		}
	}
	var out struct {
		All  creditPackageRaw `json:"all_credit_package"`
		Basic creditPackageRaw `json:"basic_package"`
		Bonus creditPackageRaw `json:"bonus_credit_package"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return CreditOverview{}, fmt.Errorf("parse credit overview: %w", err)
	}
	ov := CreditOverview{
		Total:      out.All.Amount,
		Used:       out.All.Used,
		Remain:     out.All.Remain,
		BonusRemain: out.Bonus.Remain,
		BasicRemain: out.Basic.Remain,
		Expiring:   out.All.Expiring,
	}
	if ov.Remain == 0 && ov.Total == 0 && ov.Used == 0 {
		return CreditOverview{}, fmt.Errorf("credit overview returned no data")
	}
	return ov, nil
}

// CreditOverviewTimeout 积分余额查询的默认超时。
const CreditOverviewTimeout = 20 * time.Second
