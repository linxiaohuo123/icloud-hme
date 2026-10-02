// [POS]: 标识原子创建/更新、大小写查重及母号引用保护回归
// [PROTOCOL]: 变更时检查 CLAUDE.md
package store

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTagCreateRejectsExistingIDAndConcurrentCaseDuplicates(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTag(BusinessTag{ID: "original", Tag: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTag(BusinessTag{ID: "original", Tag: "replacement"}); err == nil {
		t.Fatal("existing ID overwritten")
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "BUSINESS"
			if i%2 == 0 {
				key = " business "
			}
			err := s.CreateTag(BusinessTag{Tag: key})
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrTagExists) {
				t.Errorf("unexpected create error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	tags, err := s.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	if successes.Load() != 1 || len(tags) != 2 {
		t.Fatalf("successes=%d tags=%+v", successes.Load(), tags)
	}
	for _, tag := range tags {
		if tag.ID == "original" && tag.Tag != "first" {
			t.Fatal("original changed")
		}
	}
}

func TestTagUpdateProtectsReferencesAndRetainsHistory(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	original := BusinessTag{ID: "tag", Name: "name", Tag: "old", Description: "description", Status: "active", CreatedAt: "2026-01-01T00:00:00Z", LastAssignedAt: "2026-09-30T00:00:00Z"}
	if err := s.CreateTag(original); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAccount(&AccountRecord{ID: "acc", TagsJSON: `[" OLD "]`}); err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateTag("tag", func(tag *BusinessTag) { tag.Tag = "new"; tag.Description = "changed" })
	if !errors.Is(err, ErrTagInUse) {
		t.Fatalf("referenced rename: %v", err)
	}
	tags, err := s.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	if tags[0] != original {
		t.Fatalf("failed rename partially wrote: %+v", tags)
	}
	updated, err := s.UpdateTag("tag", func(tag *BusinessTag) { tag.Name = "renamed"; tag.Description = ""; tag.Status = "disabled" })
	if err != nil || updated.Name != "renamed" || updated.Tag != "old" || updated.CreatedAt != original.CreatedAt || updated.LastAssignedAt != original.LastAssignedAt {
		t.Fatalf("safe edit: %+v err=%v", updated, err)
	}
	if err := s.SaveAccount(&AccountRecord{ID: "acc", TagsJSON: `[]`}); err != nil {
		t.Fatal(err)
	}
	updated, err = s.UpdateTag("tag", func(tag *BusinessTag) { tag.Tag = "new" })
	if err != nil || updated.Tag != "new" {
		t.Fatalf("unreferenced rename: %+v err=%v", updated, err)
	}
	if _, err := s.DeleteTag("tag"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTag("tag", func(*BusinessTag) {}); !errors.Is(err, ErrTagNotFound) {
		t.Fatalf("deleted tag resurrected: %v", err)
	}
}

func TestTagUpdateDoesNotIgnoreCorruptAccountTags(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTag(BusinessTag{ID: "tag", Tag: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAccount(&AccountRecord{ID: "acc", TagsJSON: `broken`}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTag("tag", func(tag *BusinessTag) { tag.Tag = "new" }); err == nil {
		t.Fatal("corrupt references treated as absent")
	}
}
