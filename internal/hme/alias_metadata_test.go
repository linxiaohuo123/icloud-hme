/**
 * [INPUT]: 实际 HME 解析器、客户端和本地 httptest 上游
 * [OUTPUT]: 别名说明读写往返与 JSON 字段契约回归测试
 * [POS]: hme 元数据维护流程验证，不访问真实 Apple 服务
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
package hme

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAliasNoteReadModifyWrite(t *testing.T) {
	label, note := "Existing label", "Existing important note"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/hme/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"hmeEmails": []any{
				map[string]any{"hme": "note-test@icloud.com", "anonymousId": "note-test", "isActive": true,
					"metaData": map[string]string{"label": label, "note": note}},
			}}})
		case "/v1/hme/updateMetaData":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["anonymousId"] != "note-test" {
				t.Errorf("unexpected ID: %#v", payload)
			}
			label, note = payload["label"], payload["note"]
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	client.SetServiceURL(upstream.URL)
	aliases, err := client.ListAliases()
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 1 || aliases[0].Note != "Existing important note" {
		t.Fatalf("note lost on read: %+v", aliases)
	}
	if err := client.UpdateMetaData(aliases[0].AnonymousID, "Renamed label", aliases[0].Note); err != nil {
		t.Fatal(err)
	}
	if note != "Existing important note" {
		t.Fatalf("label change erased note: %q", note)
	}
	if err := client.UpdateMetaData(aliases[0].AnonymousID, "Renamed label", ""); err != nil {
		t.Fatal(err)
	}
	aliases, err = client.ListAliases()
	if err != nil {
		t.Fatal(err)
	}
	if aliases[0].Note != "" {
		t.Fatalf("explicit clearing did not round trip: %+v", aliases[0])
	}
}

func TestParseAliasNotesAndJSONContract(t *testing.T) {
	for _, tc := range []struct{ name, fields, want string }{
		{"nested note", `"metaData":{"note":"nested note"}`, "nested note"},
		{"top-level note", `"note":"direct note"`, "direct note"},
		{"missing note", `"label":"label only"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"success":true,"result":{"hmeEmails":[{"hme":"test@icloud.com","anonymousId":"test",` + tc.fields + `}]}}`
			aliases, err := parseAliasList(body)
			if err != nil {
				t.Fatal(err)
			}
			exported, err := json.Marshal(aliases[0])
			if err != nil {
				t.Fatal(err)
			}
			var dto map[string]any
			if err := json.Unmarshal(exported, &dto); err != nil {
				t.Fatal(err)
			}
			if actual, exists := dto["note"]; !exists || actual != tc.want {
				t.Fatalf("note DTO mismatch: %s", exported)
			}
		})
	}
}
