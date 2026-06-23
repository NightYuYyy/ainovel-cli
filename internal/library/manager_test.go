package library

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NightYuYyy/ainovel-cli/assets"
	"github.com/NightYuYyy/ainovel-cli/internal/bootstrap"
	"github.com/NightYuYyy/ainovel-cli/internal/domain"
)

func TestManager_SubmitAndListTasks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "library-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := bootstrap.Config{OutputDir: tmpDir}
	cfg.FillDefaults()

	bundle := assets.Load("default")
	lib := New(cfg, bundle)
	defer lib.Close()

	// 提交两个任务
	id1, err := lib.SubmitTask("测试书1", "写一本测试小说")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	id2, err := lib.SubmitTask("测试书2", "写一本武侠小说")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 列出任务
	tasks, err := lib.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	// 验证任务属性
	for _, task := range tasks {
		if task.ID != id1 && task.ID != id2 {
			t.Errorf("unexpected task ID: %s", task.ID)
		}
		if task.Status != domain.TaskQueued {
			t.Errorf("expected status queued, got %s", task.Status)
		}
		if task.OutputDir == "" {
			t.Error("expected output dir to be set")
		}
		expectedDir := filepath.Join(tmpDir, string(task.ID))
		if task.OutputDir != expectedDir {
			t.Errorf("expected OutputDir %s, got %s", expectedDir, task.OutputDir)
		}
	}

	// 获取单个任务
	task, err := lib.GetTask(id1)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected task not nil")
	}
	if task.BookName != "测试书1" {
		t.Errorf("expected 测试书1, got %s", task.BookName)
	}

	// Snapshot
	snap := lib.Snapshot()
	if len(snap.Tasks) != 2 {
		t.Errorf("snapshot: expected 2 tasks, got %d", len(snap.Tasks))
	}
	if snap.ActiveTask != nil {
		t.Errorf("snapshot: expected no active task, got %v", *snap.ActiveTask)
	}
	if snap.QueueSize != 2 {
		t.Errorf("snapshot: expected queue size 2, got %d", snap.QueueSize)
	}
}

func TestManager_DeleteTask(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "library-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := bootstrap.Config{OutputDir: tmpDir}
	cfg.FillDefaults()

	bundle := assets.Load("default")
	lib := New(cfg, bundle)
	defer lib.Close()

	id, err := lib.SubmitTask("待删除", "prompt")
	if err != nil {
		t.Fatal(err)
	}

	if err := lib.DeleteTask(id); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	tasks, err := lib.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks after delete, got %d", len(tasks))
	}
}

func TestManager_LoadBookProgress(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "library-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := bootstrap.Config{OutputDir: tmpDir}
	cfg.FillDefaults()

	bundle := assets.Load("default")
	lib := New(cfg, bundle)
	defer lib.Close()

	id, err := lib.SubmitTask("进度测试", "prompt")
	if err != nil {
		t.Fatal(err)
	}

	// 无进度时返回 nil
	bp := lib.LoadBookProgress(id)
	if bp != nil {
		t.Errorf("expected nil progress for new task, got %+v", bp)
	}
}
