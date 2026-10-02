// [POS]: 标识 HTTP 身份控制及引用改键错误契约回归
// [PROTOCOL]: 变更时检查 CLAUDE.md
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/store"
)

func TestTagHandlersOwnIdentityAndRejectReferencedRename(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	original := store.BusinessTag{ID: "original", Tag: "first", Name: "first", CreatedAt: "2026-01-01T00:00:00Z", LastAssignedAt: "2026-09-30T00:00:00Z", Status: "active"}
	if err := st.CreateTag(original); err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: st}
	router := gin.New()
	router.POST("/tags", srv.createTagHandler)
	router.PATCH("/tags/:id", srv.updateTagHandler)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		return w
	}
	w := send(http.MethodPost, "/tags", `{"id":"original","tag":"new","created_at":"client-time","last_assigned_at":"client-time"}`)
	var response struct {
		Data store.BusinessTag `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.Data.ID == "original" || response.Data.ID == "" || response.Data.CreatedAt == "client-time" || response.Data.LastAssignedAt != "" {
		t.Fatalf("client identity trusted: %d %s", w.Code, w.Body)
	}
	tags, err := st.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("tags=%+v", tags)
	}
	for _, tag := range tags {
		if tag.ID == "original" && tag != original {
			t.Fatalf("original overwritten: %+v", tag)
		}
	}
	if w := send(http.MethodPost, "/tags", `{"tag":" FIRST "}`); w.Code != 400 {
		t.Fatalf("duplicate accepted: %d %s", w.Code, w.Body)
	}
	if err := st.SaveAccount(&store.AccountRecord{ID: "acc", TagsJSON: `["FIRST"]`}); err != nil {
		t.Fatal(err)
	}
	w = send(http.MethodPatch, "/tags/original", `{"tag":"renamed","description":"changed"}`)
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("TAG_IN_USE")) {
		t.Fatalf("rename contract: %d %s", w.Code, w.Body)
	}
	w = send(http.MethodPatch, "/tags/original", `{"description":"safe change"}`)
	if w.Code != 200 {
		t.Fatalf("description edit blocked: %d %s", w.Code, w.Body)
	}
	w = send(http.MethodPatch, "/tags/missing", `{"description":"new"}`)
	if w.Code != 404 {
		t.Fatalf("missing update: %d %s", w.Code, w.Body)
	}
	w = send(http.MethodPatch, "/tags/"+response.Data.ID, `{"tag":"unreferenced"}`)
	if w.Code != 200 {
		t.Fatalf("unreferenced rename: %d %s", w.Code, w.Body)
	}
}
