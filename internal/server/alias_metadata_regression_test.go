/**
 * [INPUT]: 实际 managerBackend、临时账号配置和本地 httptest 上游
 * [OUTPUT]: 单条/批量元数据修改与列表缓存一致性测试
 * [POS]: server 别名维护回归防线，部分失败不覆盖原元数据
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

func TestAliasMetadataCacheAfterSingleAndBatchUpdates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hme/updateMetaData" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["anonymousId"] == "failed" {
			_, _ = w.Write([]byte(`{"success":false,"error":{"errorMessage":"injected failure"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer upstream.Close()
	mgr, err := account.NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	sum, err := mgr.AddAccountWithInput(account.AddAccountInput{Name: "metadata", ICloudEmail: "owner@icloud.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SaveSession(sum.ID, map[string]string{"X-APPLE-WEBAUTH-TOKEN": "test-token"}, upstream.URL); err != nil {
		t.Fatal(err)
	}
	backend := &managerBackend{mgr: mgr}
	backend.setCachedAliases(sum.ID, []hme.Alias{
		{AnonymousID: "single", Label: "old label", Note: "old note"},
		{AnonymousID: "batch", Label: "old label", Note: "old note"},
		{AnonymousID: "failed", Label: "old label", Note: "old note"},
	})
	if err := backend.UpdateAlias(sum.ID, "single", "renamed", "new note"); err != nil {
		t.Fatal(err)
	}
	result, err := backend.BatchUpdateAliases(sum.ID, []string{"batch", "failed"}, "batch label", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Succeeded) != 1 || len(result.Failed) != 1 {
		t.Fatalf("unexpected batch result: %+v", result)
	}
	// Read through the public list path: it must reflect writes without a live refresh.
	aliases, err := backend.ListAliases(sum.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{"single": {"renamed", "new note"}, "batch": {"batch label", ""}, "failed": {"old label", "old note"}}
	for _, alias := range aliases {
		if actual := [2]string{alias.Label, alias.Note}; actual != want[alias.AnonymousID] {
			t.Fatalf("stale metadata: %+v", alias)
		}
	}
	if len(aliases) != len(want) {
		t.Fatalf("incomplete list: %+v", aliases)
	}
}
