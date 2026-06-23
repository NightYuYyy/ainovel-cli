package domain

import "time"

// TaskQueueEvent 表示任务队列级别的事件，用于 UI/日志。
type TaskQueueEvent struct {
	Time    time.Time  `json:"time"`
	TaskID  BookID     `json:"task_id"`
	Kind    string     `json:"kind"`    // task_queued / task_started / task_paused / task_completed / task_failed
	Summary string     `json:"summary"` // 人类可读摘要
	Error   string     `json:"error,omitempty"`
}

// TaskQueueSnapshot 是任务队列的当前状态快照，供 UI 消费。
type TaskQueueSnapshot struct {
	Tasks      []BookTask `json:"tasks"`
	ActiveTask *BookID    `json:"active_task,omitempty"` // 当前正在执行的任务
	QueueSize  int        `json:"queue_size"`             // 排队中的任务数
}

// LibraryConfig 是图书馆级别的配置，不影响单本书的 Config。
type LibraryConfig struct {
	// MaxConcurrent 最大并发写作任务数，0 或 1 表示串行。
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// LibraryDir 图书馆根目录，所有书的输出目录位于其下。
	// 默认为 output/novels。
	LibraryDir string `json:"library_dir,omitempty"`
}
