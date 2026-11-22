package pool

import (
	"context"
	"fmt"
	"time"

	"codearts2api/internal/upstream"
)

// CheckinClient 允许注入独立的活动客户端；默认使用各账号的上游客户端。
type CheckinClient interface {
	Checkin(context.Context, upstream.SignCredential) (upstream.CheckinResult, error)
}

// BenefitClient 领取限时福利（福利网关 benefit/claim）。
type BenefitClient interface {
	ClaimBenefit(upstream.SignCredential) error
}

// CreditsClient 查询积分余额（官方控制台套餐概览）。
type CreditsClient interface {
	FetchCreditOverview(context.Context, upstream.SignCredential) (upstream.CreditOverview, error)
}

type CheckinStatus struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	CheckedAt time.Time `json:"checked_at"`
}

// BenefitStatus 限时福利（模型福利网关）领取结果。
//
// 与 CheckinStatus 分开：福利网关是「模型权益」，签到是「活动积分」，
// 两条链路的上游故障互不相关，混在一起会让面板上的错误原因失真。
type BenefitStatus struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	CheckedAt time.Time `json:"checked_at"`
}

// Checkin 串行领取指定账号的每日签到和新手福利。活动故障只记录签到结果，
// 不计入推理错误、冷却或禁用；token 仍由原有 Validate/RefreshToken 处理。
func (p *Pool) Checkin(ctx context.Context, name string) (upstream.CheckinResult, error) {
	a := p.Get(name)
	if a == nil {
		return upstream.CheckinResult{}, fmt.Errorf("account not found")
	}
	a.checkinMu.Lock()
	defer a.checkinMu.Unlock()
	if err := ctx.Err(); err != nil {
		return upstream.CheckinResult{}, err
	}
	a.mu.Lock()
	disabled := a.disabled
	a.mu.Unlock()
	if disabled {
		return upstream.CheckinResult{}, fmt.Errorf("account disabled")
	}
	ok, err := p.Validate(a)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("token invalid; re-login required")
		}
		return upstream.CheckinResult{}, err
	}
	token, ak, sk := a.Auth.Credentials()
	client := p.cfg.CheckinClient
	if client == nil {
		client = a.Client
	}
	result, err := client.Checkin(ctx, upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk})
	if err != nil {
		result.OK, result.Message = false, err.Error()
	}
	a.mu.Lock()
	a.checkin = &CheckinStatus{OK: result.OK, Message: result.Message, CheckedAt: time.Now()}
	a.mu.Unlock()
	return result, err
}

// ClaimBenefit 领取指定账号的限时福利（模型福利网关）。
//
// 上游接口是幂等的：已领取同样返回成功，因此重复调用安全。
// 这里串行化同一账号的领取，避免并发重复提交。
//
// 失败只记录福利状态，不计入推理错误计数、不冷却也不禁用账号：
// 福利网关故障（例如 error_code=4000 invalid channel, group not found）
// 与账号 token 无关，不该让账号看起来「不健康」。
func (p *Pool) ClaimBenefit(ctx context.Context, name string) error {
	a := p.Get(name)
	if a == nil {
		return fmt.Errorf("account not found")
	}
	a.checkinMu.Lock()
	defer a.checkinMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	disabled := a.disabled
	a.mu.Unlock()
	if disabled {
		return fmt.Errorf("account disabled")
	}
	if ok, err := p.Validate(a); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("token invalid; re-login required")
	}
	token, ak, sk := a.Auth.Credentials()
	client := BenefitClient(a.Client)
	if p.cfg.BenefitClient != nil {
		client = p.cfg.BenefitClient
	}
	if client == nil {
		return fmt.Errorf("account client unavailable")
	}
	err := client.ClaimBenefit(upstream.SignCredential{
		SecurityToken:   token,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
	})
	a.mu.Lock()
	if err != nil {
		a.benefit = &BenefitStatus{OK: false, Message: err.Error(), CheckedAt: time.Now()}
	} else {
		a.benefit = &BenefitStatus{OK: true, Message: "已领取", CheckedAt: time.Now()}
	}
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

// QueryCredits 查询指定账号的积分余额。
//
// 积分余额来自官方「套餐概览」接口（AK/SK 签名），与签到/福利两条链路互不影响；
// 查询失败只记录状态，不会把账号标记为不健康。
func (p *Pool) QueryCredits(ctx context.Context, name string) (CreditsStatus, error) {
	a := p.Get(name)
	if a == nil {
		return CreditsStatus{}, fmt.Errorf("account not found")
	}
	token, ak, sk := a.Auth.Credentials()
	client := CreditsClient(a.Client)
	if p.cfg.CreditsClient != nil {
		client = p.cfg.CreditsClient
	}
	if client == nil {
		return CreditsStatus{}, fmt.Errorf("account client unavailable")
	}
	ov, err := client.FetchCreditOverview(ctx, upstream.SignCredential{
		SecurityToken:   token,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
	})
	status := CreditsStatus{
		Total:           ov.Total,
		Used:            ov.Used,
		Remain:          ov.Remain,
		BonusRemain:     ov.BonusRemain,
		BasicRemain:     ov.BasicRemain,
		OnDemandRemain:  ov.OnDemandRemain,
		Expiring:        ov.Expiring,
		PackageName:     ov.PackageName,
		SpecCode:        ov.SpecCode,
		PackageStatus:   ov.PackageStatus,
		IsCreditPackage: ov.IsCreditPackage,
		IsTokenPackage:  ov.IsTokenPackage,
		PackageEndDate:  ov.PackageEndDate,
	}
	if err != nil {
		status.OK = false
		status.Message = err.Error()
	} else {
		status.OK = true
		status.Message = "ok"
	}
	status.CheckedAt = time.Now()
	a.mu.Lock()
	a.credits = &status
	a.mu.Unlock()
	return status, err
}
