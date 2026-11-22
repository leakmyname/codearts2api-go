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
	// OnDemandRemain 按需包剩余积分（实时结算，随时可能为 0）。
	OnDemandRemain float64 `json:"ondemand_remain"`
	// PackageName 套餐展示名（如「体验版」）；上游没给时为空。
	PackageName string `json:"package_name,omitempty"`
	// SpecCode 套餐规格码（如 codearts.agent.individual.trial）。
	SpecCode string `json:"spec_code,omitempty"`
	// PackageStatus 套餐状态（normal 等）。
	PackageStatus string `json:"package_status,omitempty"`
	// IsCreditPackage 是否为积分计费账户（Token 计费账户不参与签到活动）。
	IsCreditPackage bool `json:"is_credit_package"`
	// IsTokenPackage 是否为旧的 Token 计费账户。
	IsTokenPackage bool `json:"is_token_package"`
	// PackageEndDate 套餐周期结束日期（YYYY-MM-DD）。
	PackageEndDate string `json:"package_end_date,omitempty"`
}

// creditPackageRaw 是接口返回的单类积分包。
type creditPackageRaw struct {
	Amount   float64 `json:"package_credit_amount"`
	Used     float64 `json:"package_credit_used"`
	Remain   float64 `json:"package_credit_remain"`
	Expiring float64 `json:"expiring_credit_amount"`
}

// creditMetricRaw 是 statistics/plugin 里的一条 metric。
//
// 积分口径靠 name 区分（对齐 IDE 的 accountInfoPane）：
//
//	usageTotalPackageCredit    总额（基础 + 按需 + 赠送 的汇总行）
//	usageBasicPackageCredit     基础积分包（随套餐周期重置）
//	usageOnDemandPackageCredit  按需积分包（实时结算）
//	usageBonusPackageCredit     赠送积分包（签到/活动，逐笔 30 天）
type creditMetricRaw struct {
	Name     string   `json:"name"`
	Amount   float64  `json:"package_credit_amount"`
	Used     float64  `json:"package_credit_used"`
	Remain   float64  `json:"package_credit_remain"`
	Expiring *float64 `json:"package_credit_expiring_amount"`
}

// statisticsPluginRaw 是 statistics/plugin 的响应（裸对象，无 code/data 信封）。
type statisticsPluginRaw struct {
	Metrics []creditMetricRaw `json:"metrics"`
	Package struct {
		SpecCode        string `json:"spec_code"`
		Status          string `json:"status"`
		PackageNameCN   string `json:"package_name_cn"`
		PackageNameEN   string `json:"package_name_en"`
		IsTokenPackage  bool   `json:"is_token_package"`
		IsCreditPackage bool   `json:"is_credit_package"`
	} `json:"package"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
}

// fetchStatisticsPlugin 查询 IDE「账户信息」面板的数据源。
//
// 与 package/overview 的关系：后者是控制台「套餐概览」口径（余额分类齐全但没有
// 套餐名/账户类型），前者额外给出套餐名、套餐周期、账户计费类型与按需包。
// 两者数值口径一致（实测同一账号都是总额 4600 / 已用 4000.03 / 剩余 599.97）。
func (c *Client) fetchStatisticsPlugin(ctx context.Context, cred SignCredential) (*statisticsPluginRaw, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.snapURL(EpStatisticsPlugin), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	// Agent-Type / X-Language 在**签名之后**追加：不参与 SDK-HMAC-SHA256 的
	// canonical request（官方 IDE 的 fetchSignedGet 就是这个顺序）。
	signRequest(req, nil, cred)
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{
			Code: resp.StatusCode, Status: resp.StatusCode,
			Path: EpStatisticsPlugin, Message: truncateStr(string(raw), 300),
		}
	}
	var out statisticsPluginRaw
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse statistics plugin: %w", err)
	}
	return &out, nil
}

// metricsToOverview 把 metrics[] 折算成 CreditOverview。
func metricsToOverview(sp *statisticsPluginRaw) (CreditOverview, bool) {
	ov := CreditOverview{
		PackageName:     firstNonEmpty(sp.Package.PackageNameCN, sp.Package.PackageNameEN),
		SpecCode:        sp.Package.SpecCode,
		PackageStatus:   sp.Package.Status,
		IsCreditPackage: sp.Package.IsCreditPackage,
		IsTokenPackage:  sp.Package.IsTokenPackage,
		PackageEndDate:  sp.EndDate,
	}
	saw := false
	for _, m := range sp.Metrics {
		switch m.Name {
		case "usageTotalPackageCredit":
			ov.Total, ov.Used, ov.Remain = m.Amount, m.Used, m.Remain
			if m.Expiring != nil {
				ov.Expiring = *m.Expiring
			}
			saw = true
		case "usageBasicPackageCredit":
			ov.BasicRemain = m.Remain
		case "usageOnDemandPackageCredit":
			ov.OnDemandRemain = m.Remain
		case "usageBonusPackageCredit":
			ov.BonusRemain = m.Remain
			// 分类上的 expiring 与总额口径重复（实测两处同值），不再单独取。
		}
	}
	return ov, saw
}

// FetchCreditOverview 查询账号积分余额。
func (c *Client) FetchCreditOverview(ctx context.Context, cred SignCredential) (CreditOverview, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 优先用 statistics/plugin：它同时给出套餐名、账户类型与按需包。
	if sp, err := c.fetchStatisticsPlugin(ctx, cred); err == nil {
		if ov, ok := metricsToOverview(sp); ok && ov.Total > 0 {
			return ov, nil
		}
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
		All      creditPackageRaw  `json:"all_credit_package"`
		Basic    creditPackageRaw  `json:"basic_package"`
		OnDemand *creditPackageRaw `json:"ondemand_credit_package"`
		Bonus    creditPackageRaw  `json:"bonus_credit_package"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return CreditOverview{}, fmt.Errorf("parse credit overview: %w", err)
	}
	ov := CreditOverview{
		Total:       out.All.Amount,
		Used:        out.All.Used,
		Remain:      out.All.Remain,
		BonusRemain: out.Bonus.Remain,
		BasicRemain: out.Basic.Remain,
		Expiring:    out.All.Expiring,
	}
	if out.OnDemand != nil {
		ov.OnDemandRemain = out.OnDemand.Remain
	}
	if ov.Remain == 0 && ov.Total == 0 && ov.Used == 0 {
		return CreditOverview{}, fmt.Errorf("credit overview returned no data")
	}
	return ov, nil
}

// CreditOverviewTimeout 积分余额查询的默认超时。
const CreditOverviewTimeout = 20 * time.Second
