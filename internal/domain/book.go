package domain

import "time"

// BookID 唯一标识一个书籍任务。
type BookID string

// TaskStatus 表示书籍任务的生命周期状态。
type TaskStatus string

const (
	TaskQueued    TaskStatus = "queued"    // 已入队，等待执行
	TaskRunning   TaskStatus = "running"   // 正在执行
	TaskPaused    TaskStatus = "paused"    // 已暂停（用户手动暂停或异常中断）
	TaskCompleted TaskStatus = "completed" // 已完成
	TaskFailed    TaskStatus = "failed"    // 执行失败
)

// BookTask 表示一个书籍写作任务，是任务队列的基本单元。
// 持久化到 meta/library/tasks.jsonl。
type BookTask struct {
	ID          BookID     `json:"id"`
	BookName    string     `json:"book_name"`           // 书名（用户指定或从 premise 提取）
	Prompt      string     `json:"prompt"`              // 创作需求/前提
	Status      TaskStatus `json:"status"`
	Priority    int        `json:"priority"`            // 队列优先级，越大越优先
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	OutputDir   string     `json:"output_dir"`          // 输出目录，如 output/novels/{id}
	Error       string     `json:"error,omitempty"`     // 失败时的错误信息
}

// BookProgress 是书籍进度的轻量摘要，用于任务列表展示。
// 从各书的 meta/progress.json 读取后精简。
type BookProgress struct {
	Phase         string `json:"phase"`          // init/premise/outline/writing/complete
	Chapters      int    `json:"chapters"`       // 已完成章节数
	TotalChapters int    `json:"total_chapters"` // 规划总章节数
	WordCount     int    `json:"word_count"`     // 总字数
	NovelName     string `json:"novel_name"`     // 书名
	CurrentFlow   string `json:"current_flow"`   // writing/reviewing/rewriting/...
}

// IsTerminal 判断任务是否处于终态。
func (t *BookTask) IsTerminal() bool {
	return t.Status == TaskCompleted || t.Status == TaskFailed
}

// IsRunning 判断任务是否正在执行。
func (t *BookTask) IsRunning() bool {
	return t.Status == TaskRunning
}

// CanResume 判断任务是否可以从暂停状态恢复。
func (t *BookTask) CanResume() bool {
	return t.Status == TaskPaused || t.Status == TaskQueued
}
