package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const pluginFixture = `{
  "metrics":[
    {"name":"usageTotalPackageCredit","package_credit_amount":4600,"package_credit_used":4000.03,"package_credit_remain":599.97,"package_credit_expiring_amount":99.97},
    {"name":"usageBasicPackageCredit","package_credit_amount":500,"package_credit_remain":500},
    {"name":"usageOnDemandPackageCredit","package_credit_amount":0,"package_credit_remain":0},
    {"name":"usageBonusPackageCredit","package_credit_amount":4100,"package_credit_used":4000.03,"package_credit_remain":99.97,"package_credit_expiring_amount":99.97}
  ],
  "start_date":"2026-11-16","end_date":"2026-12-17",
  "package":{"spec_code":"codearts.agent.individual.trial","status":"normal","package_name_cn":"体验版","package_name_en":"Trial","is_token_package":false,"is_credit_package":true}
}`

func TestFetchCreditOverviewPrefersStatisticsPlugin(t *testing.T) {
	var pluginCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpStatisticsPlugin:
			pluginCalls++
			// Agent-Type/X-Language 是签名后追加的，不参与 SignedHeaders。
			if auth := r.Header.Get("Authorization"); auth == "" {
				t.Error("missing signature")
			}
			_, _ = fmt.Fprint(w, pluginFixture)
		default:
			t.Errorf("unexpected fallback call: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	ov, err := c.FetchCreditOverview(context.Background(), SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st"})
	if err != nil {
		t.Fatal(err)
	}
	if pluginCalls != 1 {
		t.Fatalf("plugin calls=%d", pluginCalls)
	}
	if ov.Total != 4600 || ov.Used != 4000.03 || ov.Remain != 599.97 ||
		ov.BasicRemain != 500 || ov.BonusRemain != 99.97 || ov.Expiring != 99.97 {
		t.Fatalf("unexpected balance: %+v", ov)
	}
	if ov.PackageName != "体验版" || ov.SpecCode != "codearts.agent.individual.trial" ||
		ov.PackageEndDate != "2026-12-17" || !ov.IsCreditPackage {
		t.Fatalf("unexpected package info: %+v", ov)
	}
}

func TestFetchCreditOverviewFallsBackToPackageOverview(t *testing.T) {
	var fallbackCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpStatisticsPlugin:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error_code":"APIG.0301","error_msg":"signature fail"}`)
		case EpPackageOverview:
			fallbackCalls++
			_, _ = fmt.Fprint(w, `{
              "all_credit_package":{"package_credit_amount":100,"package_credit_used":40,"package_credit_remain":60,"expiring_credit_amount":20},
              "basic_package":{"package_credit_remain":10},
              "ondemand_credit_package":{"package_credit_remain":5},
              "bonus_credit_package":{"package_credit_remain":45}
            }`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	ov, err := c.FetchCreditOverview(context.Background(), SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback calls=%d", fallbackCalls)
	}
	if ov.Total != 100 || ov.Remain != 60 || ov.BasicRemain != 10 ||
		ov.OnDemandRemain != 5 || ov.BonusRemain != 45 || ov.Expiring != 20 {
		t.Fatalf("unexpected fallback result: %+v", ov)
	}
}

// 没有积分口径（total 为 0）时 statistics/plugin 不能当作成功返回，
// 否则 Token 计费账户会显示「余额 0」而不是回退到套餐概览。
func TestMetricsToOverviewRequiresTotalMetric(t *testing.T) {
	var sp statisticsPluginRaw
	sp.Package.IsTokenPackage = true
	if _, ok := metricsToOverview(&sp); ok {
		t.Fatal("expected no credit metric")
	}
}
