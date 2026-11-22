package server

import (
	"os"
	"path/filepath"
	"testing"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 上游 oauth2/tokens 有时把 IAM principal id 放在 user_id，把同一个人真正的
// 账号标识放在 domain_id。若不规范化，同一个人会在账号池里出现两次。
func TestSaveLoginResultNormalizesPrincipalIDToExistingUID(t *testing.T) {
	authDir := t.TempDir()
	existing := auth.New("u1", "hid_tester", "domain-1", "tok", "ak", "sk",
		"2026-09-03T00:00:00Z", "rt", "verifier")
	if err := auth.SaveNew(authDir, existing); err != nil {
		t.Fatal(err)
	}

	p, err := pool.New(nil, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(existing)
	h := NewHandler(Config{APIKey: "k", AuthDir: authDir, Listen: ":7866", Pool: p})

	tok := &upstream.TokenResponse{
		UserID:       "iam-principal",
		UserName:     "hid_tester",
		DomainID:     "u1",
		RefreshToken: "rt2",
		Credentials: upstream.Credentials{
			AccessKeyID:     "ak2",
			SecretAccessKey: "sk2",
			SecurityToken:   "st2",
			Expiration:      "2026-09-03T00:00:00Z",
		},
	}
	uid, name, err := h.saveLoginResult(tok, "verifier2")
	if err != nil {
		t.Fatal(err)
	}
	if uid != "u1" {
		t.Fatalf("uid not normalized: got %q want u1", uid)
	}
	if name != "hid_tester" {
		t.Fatalf("unexpected name %q", name)
	}
	if n := len(p.Accounts()); n != 1 {
		t.Fatalf("pool should still hold one account, got %d", n)
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("auth dir should hold one credential, got %d", len(entries))
	}
	if entries[0].Name() != "codearts-u1.json" {
		t.Fatalf("unexpected credential file %q", entries[0].Name())
	}
	list, err := auth.LoadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Refresh() != "rt2" {
		t.Fatalf("credential not updated in place: %+v", list)
	}
	if _, err := os.Stat(filepath.Join(authDir, "codearts-iam-principal.json")); !os.IsNotExist(err) {
		t.Fatal("stale principal-id credential file was not removed")
	}
}
