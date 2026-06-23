package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NightYuYyy/ainovel-cli/internal/domain"
)

// LibraryStore 管理书籍任务队列的持久化。
// 独立于单本书的 Store，使用图书馆根目录作为存储位置。
// 任务以 JSONL 格式追加到 meta/library/tasks.jsonl。
type LibraryStore struct {
	io *IO // 指向图书馆根目录（如 output/novels/）
}

// NewLibraryStore 创建图书馆持久化存储。
// dir 为图书馆根目录，所有书的输出目录位于其下。
func NewLibraryStore(dir string) *LibraryStore {
	return &LibraryStore{io: newIO(dir)}
}

const libraryTasksRel = "meta/library/tasks.jsonl"

// AppendTask 追加一条新的书籍任务到 JSONL 文件末尾。
func (s *LibraryStore) AppendTask(task domain.BookTask) error {
	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}
	data = append(data, '\n')
	return s.io.AppendLine(libraryTasksRel, data)
}

// LoadTasks 读取所有书籍任务。
func (s *LibraryStore) LoadTasks() ([]domain.BookTask, error) {
	return loadJSONLines[domain.BookTask](s.io, libraryTasksRel)
}

// UpdateTask 原地更新一条任务记录。
// JSONL 追加写模式下通过重写整个文件实现更新。
func (s *LibraryStore) UpdateTask(updated domain.BookTask) error {
	tasks, err := s.LoadTasks()
	if err != nil {
		return fmt.Errorf("load tasks: %w", err)
	}

	found := false
	for i, t := range tasks {
		if t.ID == updated.ID {
			tasks[i] = updated
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("task %s not found", updated.ID)
	}

	return s.rewriteTasks(tasks)
}

// DeleteTask 删除一条任务记录。
func (s *LibraryStore) DeleteTask(id domain.BookID) error {
	tasks, err := s.LoadTasks()
	if err != nil {
		return fmt.Errorf("load tasks: %w", err)
	}

	filtered := tasks[:0]
	for _, t := range tasks {
		if t.ID != id {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == len(tasks) {
		return fmt.Errorf("task %s not found", id)
	}

	return s.rewriteTasks(filtered)
}

// LoadTaskByID 按 ID 查找单条任务。
func (s *LibraryStore) LoadTaskByID(id domain.BookID) (*domain.BookTask, error) {
	tasks, err := s.LoadTasks()
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.ID == id {
			return &t, nil
		}
	}
	return nil, nil
}

// rewriteTasks 原子重写整个 tasks.jsonl（先写临时文件再 rename）。
func (s *LibraryStore) rewriteTasks(tasks []domain.BookTask) error {
	tmpRel := libraryTasksRel + ".tmp"
	tmpPath := s.io.path(tmpRel)
	finalPath := s.io.path(libraryTasksRel)

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	var buf bytes.Buffer
	for _, t := range tasks {
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("marshal task %s: %w", t.ID, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}

	if err := os.WriteFile(tmpPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
