package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

type adminBenefitClient struct {
	calls  int
	failOn string
}

func (c *adminBenefitClient) ClaimBenefit(cred upstream.SignCredential) error {
	c.calls++
	if c.failOn != "" && cred.SecurityToken == c.failOn {
		return errors.New("benefit gateway: STS5.1806 token expired")
	}
	return nil
}

func TestAdminClaimBenefits(t *testing.T) {
	bc := &adminBenefitClient{}
	a1 := auth.New("u1", "user1", "domain", "tok1", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	a2 := auth.New("u2", "user2", "domain", "tok2", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a1, a2}, pool.Config{BenefitClient: bc}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: "test-key"})

	// 未授权
	{
		req := httptest.NewRequest(http.MethodPost, "/admin/api/benefits/claim", strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized status = %d, want 401", rec.Code)
		}
	}

	// 账号不存在
	{
		req := httptest.NewRequest(http.MethodPost, "/admin/api/benefits/claim", strings.NewReader(`{"uid":"missing"}`))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("missing account status = %d, want 404", rec.Code)
		}
	}

	// 单账号
	{
		req := httptest.NewRequest(http.MethodPost, "/admin/api/benefits/claim", strings.NewReader(`{"uid":"u1"}`))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("single claim status = %d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			OK      bool           `json:"ok"`
			Results []actionResult `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK || len(resp.Results) != 1 || resp.Results[0].UID != "u1" || !resp.Results[0].OK {
			t.Fatalf("unexpected single claim response: %s", rec.Body.String())
		}
	}

	// 全部账号
	{
		req := httptest.NewRequest(http.MethodPost, "/admin/api/benefits/claim", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("all claim status = %d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			OK      bool           `json:"ok"`
			Results []actionResult `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK || len(resp.Results) != 2 {
			t.Fatalf("unexpected all-claim response: %s", rec.Body.String())
		}
	}
}

func TestAdminClaimBenefitsMixedResults(t *testing.T) {
	bc := &adminBenefitClient{failOn: "tok2"}
	a1 := auth.New("u1", "user1", "domain", "tok1", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	a2 := auth.New("u2", "user2", "domain", "tok2", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a1, a2}, pool.Config{BenefitClient: bc}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: "test-key"})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/benefits/claim", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool           `json:"ok"`
		Message string         `json:"message"`
		Results []actionResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Error("mixed results should not report ok")
	}
	if !strings.Contains(resp.Message, "1 成功") || !strings.Contains(resp.Message, "1 失败") {
		t.Errorf("summary should mention success/failure split, got %q", resp.Message)
	}
	failed := ""
	for _, r := range resp.Results {
		if !r.OK {
			failed = r.Message
		}
	}
	if !strings.Contains(failed, "STS5.1806") {
		t.Errorf("failed result should carry the upstream reason, got %q", failed)
	}
}

// 账号错误明细必须能在 overview 里看到原文，而不是只有一个计数。
func TestOverviewExposesAccountErrorDetails(t *testing.T) {
	a1 := auth.New("u1", "user1", "domain", "tok", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a1}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.NoteError("u1", 3, time.Minute, "upstream 500 internal error: model busy")
	h := NewHandler(Config{Pool: p, APIKey: "test-key"})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(resp.Accounts))
	}
	acct := resp.Accounts[0]
	if msg, _ := acct["last_error_msg"].(string); !strings.Contains(msg, "model busy") {
		t.Errorf("last_error_msg = %q, want it to contain the upstream reason", msg)
	}
	if acct["err_count"].(float64) != 1 {
		t.Errorf("err_count = %v, want 1", acct["err_count"])
	}
	if _, ok := acct["last_error_at"].(string); !ok {
		t.Error("last_error_at should be present")
	}
	recent, ok := acct["recent_errors"].([]any)
	if !ok || len(recent) != 1 {
		t.Fatalf("recent_errors = %v, want 1 entry", acct["recent_errors"])
	}
	entry, _ := recent[0].(map[string]any)
	if m, _ := entry["message"].(string); !strings.Contains(m, "model busy") {
		t.Errorf("recent error message = %q", m)
	}
}

// 福利网关故障与账号健康无关：不能计入错误计数、不能写进「最近错误」。
func TestBenefitFailureDoesNotPolluteAccountHealth(t *testing.T) {
	bc := &adminBenefitClient{failOn: "tok1"}
	a1 := auth.New("u1", "user1", "domain", "tok1", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a1}, pool.Config{BenefitClient: bc}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: "test-key"})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/benefits/claim", strings.NewReader(`{"uid":"u1"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	ovReq := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	ovReq.Header.Set("Authorization", "Bearer test-key")
	ovRec := httptest.NewRecorder()
	h.ServeHTTP(ovRec, ovReq)
	var resp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(ovRec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	acct := resp.Accounts[0]
	if c, _ := acct["err_count"].(float64); c != 0 {
		t.Errorf("err_count = %v, want 0 (benefit failure must not count)", c)
	}
	if msg, _ := acct["last_error_msg"].(string); msg != "" {
		t.Errorf("last_error_msg = %q, want empty", msg)
	}
	benefit, ok := acct["benefit"].(map[string]any)
	if !ok {
		t.Fatalf("benefit status missing: %v", acct["benefit"])
	}
	if ok, _ := benefit["ok"].(bool); ok {
		t.Error("benefit status should be failed")
	}
	if m, _ := benefit["message"].(string); !strings.Contains(m, "STS5.1806") {
		t.Errorf("benefit message = %q, want the upstream reason", m)
	}
}

// 积分余额查询：成功返回余额，失败不影响账号健康。
func TestAdminQueryCredits(t *testing.T) {
	cc := &adminCreditsClient{remain: 599.29, bonus: 99.29, basic: 500}
	a1 := auth.New("u1", "user1", "domain", "tok", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a1}, pool.Config{CreditsClient: cc}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: "test-key"})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/credits/balance", strings.NewReader(`{"uid":"u1"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool           `json:"ok"`
		Results []actionResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || len(resp.Results) != 1 {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
	if resp.Results[0].Credits < 599 {
		t.Errorf("credits = %v, want ~599.29", resp.Results[0].Credits)
	}

	ovReq := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	ovReq.Header.Set("Authorization", "Bearer test-key")
	ovRec := httptest.NewRecorder()
	h.ServeHTTP(ovRec, ovReq)
	var ov struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(ovRec.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	credits, ok := ov.Accounts[0]["credits"].(map[string]any)
	if !ok {
		t.Fatalf("credits missing in overview: %v", ov.Accounts[0]["credits"])
	}
	if r, _ := credits["remain"].(float64); r < 599 {
		t.Errorf("overview remain = %v, want ~599.29", r)
	}
}

// 移除账号：池内条目与磁盘凭证文件都要清掉。
func TestAdminRemoveAccount(t *testing.T) {
	dir := t.TempDir()
	a1 := auth.New("u1", "user1", "domain", "tok", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	if err := auth.SaveNew(dir, a1); err != nil {
		t.Fatal(err)
	}
	p, err := pool.New([]*auth.Auth{a1}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: "test-key", AuthDir: dir})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/remove", strings.NewReader(`{"uid":"u1"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if p.Get("u1") != nil {
		t.Error("account should be removed from pool")
	}
	if _, err := os.Stat(filepath.Join(dir, auth.FileNameFor("u1"))); !os.IsNotExist(err) {
		t.Error("credential file should be deleted")
	}
}

type adminCreditsClient struct {
	remain float64
	bonus  float64
	basic  float64
	fail   bool
}

func (c *adminCreditsClient) FetchCreditOverview(_ context.Context, _ upstream.SignCredential) (upstream.CreditOverview, error) {
	if c.fail {
		return upstream.CreditOverview{}, errors.New("snap-access 503")
	}
	return upstream.CreditOverview{
		Remain: c.remain, BonusRemain: c.bonus, BasicRemain: c.basic,
		Total: c.remain + c.used(), Used: c.used(),
	}, nil
}

func (c *adminCreditsClient) used() float64 { return 4000.71 }
