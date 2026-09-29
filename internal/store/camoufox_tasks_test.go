package store

import (
	"testing"
	"time"
)

func TestCamoufoxTaskPersistence(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	created := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if err := st.SaveCamoufoxTask(CamoufoxTask{
		AccountID: "account-1",
		TaskID:    "task-1",
		BaseURL:   "https://agent.example.test",
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCamoufoxTask(CamoufoxTask{
		AccountID: "account-1",
		TaskID:    "task-2",
		BaseURL:   "https://agent.example.test/v2",
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ListCamoufoxTasks()
	if err != nil || len(tasks) != 1 || tasks[0].TaskID != "task-2" {
		t.Fatalf("unexpected persisted tasks: tasks=%+v err=%v", tasks, err)
	}
	if err := st.DeleteCamoufoxTask("account-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	if tasks, err := st.ListCamoufoxTasks(); err != nil || len(tasks) != 1 {
		t.Fatalf("stale task deletion removed or retained wrong rows: tasks=%+v err=%v", tasks, err)
	}
	if err := st.DeleteCamoufoxTask("account-1", "task-2"); err != nil {
		t.Fatal(err)
	}
	if tasks, err := st.ListCamoufoxTasks(); err != nil || len(tasks) != 0 {
		t.Fatalf("task was not deleted: tasks=%+v err=%v", tasks, err)
	}
}

func TestCamoufoxTaskMigrationFromV6(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TABLE camoufox_tasks; PRAGMA user_version = 6;`); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if err := upgraded.SaveCamoufoxTask(CamoufoxTask{
		AccountID: "account-1", TaskID: "task-1", BaseURL: "https://agent.example.test",
	}); err != nil {
		t.Fatalf("v6 to v7 migration did not create task table: %v", err)
	}
}
