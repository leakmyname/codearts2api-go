package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCheckinTransitionsAndEligibility(t *testing.T) {
	tests := []struct {
		name, status string
		claimable    bool
		end          string
		start        string
		wantOK       bool
		wantWrites   []string
		wantCredits  float64
	}{
		{name: "fresh", status: "ELIGIBLE", claimable: true, wantOK: true, wantWrites: []string{EpCreditClaim, EpCreditConfirm}, wantCredits: 1000},
		{name: "unconfirmed", status: "CLAIMED", wantOK: true, wantWrites: []string{EpCreditConfirm}},
		{name: "confirmed", status: "CONFIRMED", wantOK: true},
		{name: "consumed", status: "CONSUMED", wantOK: true},
		{name: "not eligible", wantOK: true},
		// 上游说 ELIGIBLE 但 claimable=false：没有可领的东西，静默跳过。
		{name: "not yet claimable", status: "ELIGIBLE", wantOK: true},
		{name: "unknown status", status: "CHANGED"},
		{name: "expired", status: "ELIGIBLE", claimable: true, end: "2020-01-01T00:00:00Z", wantOK: true},
		{name: "not started", status: "ELIGIBLE", claimable: true, start: "2100-01-01T00:00:00+08:00", wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Agent-Type") != "PromptCenter" || r.Header.Get("X-Security-Token") != "token" || !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 ") {
					t.Error("missing signed PromptCenter credentials")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if r.URL.Path != EpCreditDelivery || r.URL.Query().Get("channel") != "IDE" {
						t.Errorf("wrong delivery URL: %s", r.URL)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []any{
						map[string]any{"campaignId": 1, "title": "daily", "status": tc.status, "claimable": tc.claimable, "benefitUnit": "CREDIT", "benefitAmount": 1000, "pendingTotalAmount": 1000, "extra": map[string]string{"endTime": tc.end, "startTime": tc.start}},
						map[string]any{"campaignId": 2, "claimable": false, "status": "ELIGIBLE"},
						map[string]any{"campaignId": 3, "claimable": true, "status": ""},
					}}})
					return
				}
				writes = append(writes, r.URL.Path)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["campaignId"] != float64(1) {
					t.Errorf("unexpected campaign: %v", body)
				}
				if r.URL.Path == EpCreditClaim {
					key, _ := body["idempotentKey"].(string)
					if body["channel"] != "IDE" || !strings.HasPrefix(key, "claim_1_") {
						t.Errorf("wrong claim body: %v", body)
					}
				}
				_, _ = fmt.Fprint(w, `{"code":0,"message":"ok"}`)
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "token"})
			if err != nil || res.OK != tc.wantOK || res.Credits != tc.wantCredits || !reflect.DeepEqual(writes, tc.wantWrites) {
				t.Fatalf("result=%+v err=%v writes=%v, want ok=%t credits=%g writes=%v", res, err, writes, tc.wantOK, tc.wantCredits, tc.wantWrites)
			}
		})
	}
}

func TestCheckinRecoversConfirmationAndDailyReset(t *testing.T) {
	status := "ELIGIBLE"
	claimCount, confirmCount := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":1,"status":%q,"claimable":true,"benefitAmount":1000}]}}`, status)
		case EpCreditClaim:
			claimCount++
			status = "CLAIMED"
			_, _ = fmt.Fprint(w, `{"code":0}`)
		case EpCreditConfirm:
			confirmCount++
			if confirmCount == 1 {
				// HTTP 200 的业务失败必须重试，不能当作确认成功。
				_, _ = fmt.Fprint(w, `{"code":40001,"message":"try again"}`)
				return
			}
			status = "CONFIRMED"
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	for i, wantOK := range []bool{false, true, true} {
		res, err := c.Checkin(context.Background(), SignCredential{})
		if err != nil || res.OK != wantOK {
			t.Fatalf("attempt %d result=%+v err=%v", i, res, err)
		}
	}
	if claimCount != 1 || confirmCount != 2 {
		t.Fatalf("duplicate or missing write: claim=%d confirm=%d", claimCount, confirmCount)
	}
	// 上游在次日重新变为 ELIGIBLE；本地不得永久缓存「已签」。
	status = "ELIGIBLE"
	res, err := c.Checkin(context.Background(), SignCredential{})
	if err != nil || !res.OK || res.Credits != 1000 || claimCount != 2 || confirmCount != 3 {
		t.Fatalf("daily reset not handled: result=%+v err=%v claim=%d confirm=%d", res, err, claimCount, confirmCount)
	}
}

func TestCheckinNewUserAndMalformedResponses(t *testing.T) {
	tests := []struct {
		name, delivery, claim string
		wantErr, wantOK       bool
		wantConfirm           int
		wantCredits           float64
	}{
		{name: "new user", claim: `{"code":0}`, wantOK: true, wantConfirm: 1, wantCredits: 4000},
		{name: "claim business failure", claim: `{"code":40001,"message":"not ready"}`},
		{name: "claim missing code", claim: `{"message":"ok"}`},
		{name: "empty campaigns", delivery: `{"code":0,"data":{"items":[]}}`, wantOK: true},
		{name: "missing items", delivery: `{"code":0,"data":{}}`, wantErr: true},
		{name: "missing business code", delivery: `{"data":{"items":[]}}`, wantErr: true},
		{name: "delivery business failure", delivery: `{"code":40001,"message":"not ready"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			confirms := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case EpCreditDelivery:
					body := tc.delivery
					if body == "" {
						body = `{"code":0,"data":{"items":[{"campaignId":4,"title":"new user","status":"ELIGIBLE","claimable":true,"benefitAmount":4000}]}}`
					}
					_, _ = fmt.Fprint(w, body)
				case EpCreditClaim:
					var body struct {
						ID int `json:"campaignId"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.ID != 4 {
						t.Errorf("claimed other campaign: %d", body.ID)
					}
					_, _ = fmt.Fprint(w, tc.claim)
				case EpCreditConfirm:
					confirms++
					_, _ = fmt.Fprint(w, `{"code":0}`)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{})
			if (err != nil) != tc.wantErr || res.OK != tc.wantOK || confirms != tc.wantConfirm || res.Credits != tc.wantCredits {
				t.Fatalf("result=%+v err=%v confirms=%d", res, err, confirms)
			}
		})
	}
}

// TestCheckinStableAcrossCampaignIDRotation 固定「活动 ID 会变」这一上游行为。
// 每日签到从 1 轮换到 34 后，旧实现按 ID 过滤会静默跳过签到。现在以「上游资格
// 结论」为准：claimable=true 的活动一律领取，上游新增活动天然兼容；上游对无
// 资格的活动返回空 status 或 claimable=false，这些不领。
func TestCheckinStableAcrossCampaignIDRotation(t *testing.T) {
	var claimed []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[
				{"campaignId":34,"title":"每日签到领100积分","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":100,"pendingTotalAmount":100},
				{"campaignId":4,"title":"新用户注册送 4000 积分","status":"CONFIRMED","benefitUnit":"CREDIT","benefitAmount":4000},
				{"campaignId":39,"title":"2026年学生专属福利","status":"ELIGIBLE","claimable":false,"benefitUnit":"CREDIT","benefitAmount":5000},
				{"campaignId":2,"title":"学生认证领积分","status":"","claimable":true,"benefitUnit":"CREDIT","benefitAmount":4000}
			]}}`)
		case EpCreditClaim:
			var body struct {
				ID int `json:"campaignId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			claimed = append(claimed, body.ID)
			_, _ = fmt.Fprint(w, `{"code":0}`)
		case EpCreditConfirm:
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	res, err := c.Checkin(context.Background(), SignCredential{})
	if err != nil || !res.OK {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if !reflect.DeepEqual(claimed, []int{34}) {
		t.Fatalf("claimed campaigns = %v, want only daily check-in 34", claimed)
	}
	if res.Credits != 100 {
		t.Fatalf("credits = %g, want 100", res.Credits)
	}
}

// TestCheckinClaimsBrandNewCampaign 验证上游新增的活动（ID 未登记）只要上游
// 判定有资格就会被领取，不需要改代码。
func TestCheckinClaimsBrandNewCampaign(t *testing.T) {
	var claimed []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[
				{"campaignId":108,"title":"全新上线福利","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":300,"pendingTotalAmount":300}
			]}}`)
		case EpCreditClaim:
			var body struct {
				ID int `json:"campaignId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			claimed = append(claimed, body.ID)
			_, _ = fmt.Fprint(w, `{"code":0}`)
		case EpCreditConfirm:
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	res, err := c.Checkin(context.Background(), SignCredential{})
	if err != nil || !res.OK {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if !reflect.DeepEqual(claimed, []int{108}) || res.Credits != 300 {
		t.Fatalf("claimed=%v credits=%g, want [108] 300", claimed, res.Credits)
	}
}

// TestCheckinExcludeRules 验证排除项可按活动 ID 或标题阻止领取。
func TestCheckinExcludeRules(t *testing.T) {
	tests := []struct {
		name  string
		rules []string
	}{
		{name: "by id", rules: []string{"108"}},
		{name: "by title", rules: []string{"福利"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var claimed []int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case EpCreditDelivery:
					_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[
						{"campaignId":108,"title":"全新上线福利","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":300},
						{"campaignId":109,"title":"每日签到领100积分","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":100}
					]}}`)
				case EpCreditClaim:
					var body struct {
						ID int `json:"campaignId"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					claimed = append(claimed, body.ID)
					_, _ = fmt.Fprint(w, `{"code":0}`)
				case EpCreditConfirm:
					_, _ = fmt.Fprint(w, `{"code":0}`)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			c.SetCreditsExclude(tc.rules)
			if _, err := c.Checkin(context.Background(), SignCredential{}); err != nil {
				t.Fatalf("checkin: %v", err)
			}
			for _, id := range claimed {
				if id == 108 {
					t.Fatalf("excluded campaign claimed: %v", claimed)
				}
			}
			if !reflect.DeepEqual(claimed, []int{109}) {
				t.Fatalf("claimed=%v, want [109]", claimed)
			}
		})
	}
}
