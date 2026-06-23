package library

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NightYuYyy/ainovel-cli/assets"
	"github.com/NightYuYyy/ainovel-cli/internal/bootstrap"
	"github.com/NightYuYyy/ainovel-cli/internal/domain"
	"github.com/NightYuYyy/ainovel-cli/internal/host"
	"github.com/NightYuYyy/ainovel-cli/internal/store"
)

// Manager 管理所有书籍任务的调度与生命周期。
// 它是任务队列系统的核心：接收任务提交、管理队列、启动/暂停/恢复任务。
type Manager struct {
	cfg     bootstrap.Config // 用户配置（模型/凭证等），每本书可覆盖
	bundle  assets.Bundle
	libDir  string // 图书馆根目录，所有书的输出目录位于其下

	libStore *store.LibraryStore // 任务队列持久化

	mu       sync.Mutex
	tasks    map[domain.BookID]*runningTask // 活跃任务的内存索引
	events   chan ManagerEvent
	done     chan struct{}

	// 任务队列事件消费者（UI 投影用）
	subscribers map[chan<- ManagerEvent]struct{}
	subMu       sync.Mutex
}

// runningTask 追踪一个正在执行的任务的内部状态。
type runningTask struct {
	task   domain.BookTask
	host   *host.Host
	cancel context.CancelFunc
	done   chan struct{}
}

// ManagerEvent 是管理器级别的事件，供 UI 消费。
type ManagerEvent struct {
	Time    time.Time        `json:"time"`
	Kind    string           `json:"kind"` // task_queued / task_started / task_paused / task_completed / task_failed / task_progress
	TaskID  domain.BookID    `json:"task_id"`
	Summary string           `json:"summary,omitempty"`
	Error   string           `json:"error,omitempty"`
	Task    *domain.BookTask `json:"task,omitempty"` // 任务快照
}

// New 创建一个 Library Manager。
func New(cfg bootstrap.Config, bundle assets.Bundle) *Manager {
	cfg.FillDefaults()
	libDir := cfg.OutputDir
	if libDir == "" || libDir == filepath.Join("output", "novel") {
		// 迁移旧配置：output/novel → output/novels（图书馆根目录）
		libDir = filepath.Join("output", "novels")
	}

	return &Manager{
		cfg:         cfg,
		bundle:      bundle,
		libDir:      libDir,
		libStore:    store.NewLibraryStore(libDir),
		tasks:       make(map[domain.BookID]*runningTask),
		events:      make(chan ManagerEvent, 64),
		done:        make(chan struct{}),
		subscribers: make(map[chan<- ManagerEvent]struct{}),
	}
}

// LibDir 返回图书馆根目录。
func (m *Manager) LibDir() string { return m.libDir }

// RecoverStaleTasks 将残留的 running 状态任务（服务重启/崩溃）标记为 paused。
func (m *Manager) RecoverStaleTasks() {
	tasks, err := m.libStore.LoadTasks()
	if err != nil {
		slog.Warn("恢复任务状态失败", "module", "library", "err", err)
		return
	}
	for i := range tasks {
		if tasks[i].Status == domain.TaskRunning {
			tasks[i].Status = domain.TaskPaused
			tasks[i].Error = "服务重启，任务已暂停，可手动恢复"
			slog.Info("恢复残留任务", "module", "library", "task_id", tasks[i].ID, "name", tasks[i].BookName)
		}
	}
	if err := m.libStore.RewriteTasks(tasks); err != nil {
		slog.Warn("保存恢复状态失败", "module", "library", "err", err)
	}
}

// Events 返回管理器事件通道。
func (m *Manager) Events() <-chan ManagerEvent { return m.events }

// Subscribe 订阅管理器事件。
func (m *Manager) Subscribe(ch chan<- ManagerEvent) {
	m.subMu.Lock()
	m.subscribers[ch] = struct{}{}
	m.subMu.Unlock()
}

// Unsubscribe 取消订阅。
func (m *Manager) Unsubscribe(ch chan<- ManagerEvent) {
	m.subMu.Lock()
	delete(m.subscribers, ch)
	m.subMu.Unlock()
}

// ── 任务管理 API ──

// SubmitTask 提交一个新的书籍写作任务到队列。
// 返回分配的任务 ID。
func (m *Manager) SubmitTask(bookName, prompt string) (domain.BookID, error) {
	id := domain.BookID(newTaskID())
	now := time.Now()
	outputDir := filepath.Join(m.libDir, string(id))

	task := domain.BookTask{
		ID:        id,
		BookName:  bookName,
		Prompt:    prompt,
		Status:    domain.TaskQueued,
		Priority:  0,
		CreatedAt: now,
		OutputDir: outputDir,
	}

	if err := m.libStore.AppendTask(task); err != nil {
		return "", fmt.Errorf("persist task: %w", err)
	}

	slog.Info("任务已入队", "module", "library", "task_id", id, "name", bookName)
	m.emit(ManagerEvent{
		Time:   now,
		Kind:   "task_queued",
		TaskID: id,
		Task:   &task,
		Summary: fmt.Sprintf("任务已入队: %s", bookName),
	})

	return id, nil
}

// ListTasks 返回所有任务。
func (m *Manager) ListTasks() ([]domain.BookTask, error) {
	tasks, err := m.libStore.LoadTasks()
	if err != nil {
		return nil, fmt.Errorf("load tasks: %w", err)
	}
	if tasks == nil {
		tasks = []domain.BookTask{}
	}
	return tasks, nil
}

// GetTask 按 ID 获取任务。
func (m *Manager) GetTask(id domain.BookID) (*domain.BookTask, error) {
	m.mu.Lock()
	if rt, ok := m.tasks[id]; ok {
		m.mu.Unlock()
		return &rt.task, nil
	}
	m.mu.Unlock()
	return m.libStore.LoadTaskByID(id)
}

// DeleteTask 删除任务及其输出目录。
func (m *Manager) DeleteTask(id domain.BookID) error {
	m.mu.Lock()
	if rt, ok := m.tasks[id]; ok {
		if rt.task.Status == domain.TaskRunning {
			m.mu.Unlock()
			return fmt.Errorf("任务正在运行，请先暂停")
		}
		delete(m.tasks, id)
	}
	m.mu.Unlock()

	if err := m.libStore.DeleteTask(id); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}

	// 删除输出目录
	outputDir := filepath.Join(m.libDir, string(id))
	if err := os.RemoveAll(outputDir); err != nil {
		slog.Warn("删除任务输出目录失败", "module", "library", "task_id", id, "dir", outputDir, "err", err)
	}

	slog.Info("任务已删除", "module", "library", "task_id", id)
	return nil
}

// ── 任务生命周期 ──

// StartTask 启动一个排队中的任务。
// 同一时间最多一个任务在运行（串行模式）。
func (m *Manager) StartTask(id domain.BookID) error {
	task, err := m.GetTask(id)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task %s not found", id)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查是否有已在运行的任务
	for _, rt := range m.tasks {
		if rt.task.Status == domain.TaskRunning {
			return fmt.Errorf("已有任务在运行: %s", rt.task.ID)
		}
	}

	if task.IsTerminal() {
		return fmt.Errorf("任务已完成/失败，不可启动")
	}
	if task.Status != domain.TaskQueued && task.Status != domain.TaskPaused {
		return fmt.Errorf("任务状态 %s 不可启动", task.Status)
	}

	// 为每本书创建独立的 Config（覆盖 OutputDir）
	bookCfg := m.cfg
	bookCfg.OutputDir = task.OutputDir

	eng, err := host.New(bookCfg, m.bundle)
	if err != nil {
		return fmt.Errorf("create host: %w", err)
	}

	now := time.Now()
	task.Status = domain.TaskRunning
	task.StartedAt = &now
	task.Error = ""

	ctx, cancel := context.WithCancel(context.Background())
	rt := &runningTask{
		task:   *task,
		host:   eng,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.tasks[id] = rt

	if err := m.libStore.UpdateTask(*task); err != nil {
		cancel()
		delete(m.tasks, id)
		return fmt.Errorf("update task: %w", err)
	}

	// 异步启动任务执行
	go m.runTask(ctx, rt)

	return nil
}

// PauseTask 暂停正在运行的任务。
func (m *Manager) PauseTask(id domain.BookID) error {
	m.mu.Lock()
	rt, ok := m.tasks[id]
	if !ok || rt.task.Status != domain.TaskRunning {
		m.mu.Unlock()
		return fmt.Errorf("任务 %s 未在运行", id)
	}
	m.mu.Unlock()

	rt.host.Abort()
	return nil
}

// ResumeTask 恢复一个暂停的任务。
func (m *Manager) ResumeTask(id domain.BookID) error {
	task, err := m.GetTask(id)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task %s not found", id)
	}
	if task.Status != domain.TaskPaused {
		return fmt.Errorf("任务 %s 状态为 %s，无法恢复", id, task.Status)
	}
	return m.StartTask(id)
}

// ── 内部方法 ──

// runTask 在后台运行一个任务，监听其完成/失败事件。
func (m *Manager) runTask(ctx context.Context, rt *runningTask) {
	defer close(rt.done)
	defer rt.host.Close()

	id := rt.task.ID
	slog.Info("任务开始执行", "module", "library", "task_id", id)
	m.emit(ManagerEvent{
		Time:   time.Now(),
		Kind:   "task_started",
		TaskID: id,
		Task:   &rt.task,
		Summary: fmt.Sprintf("开始创作: %s", rt.task.BookName),
	})

	// 如果是新任务（非恢复），走 StartPrepared；否则走 Resume
	if rt.task.Status == domain.TaskRunning {
		// 检查是否有进度可以恢复
		label, err := m.tryResume(rt)
		if err != nil {
			m.handleTaskError(rt, fmt.Errorf("resume check: %w", err))
			return
		}
		if label != "" {
			// 恢复模式：Host.Resume 已在 tryResume 中调用
			slog.Info("恢复创作", "module", "library", "task_id", id, "label", label)
		} else {
			// 新建模式
			if err := rt.host.StartPrepared(rt.task.Prompt); err != nil {
				m.handleTaskError(rt, fmt.Errorf("start: %w", err))
				return
			}
		}
	}

	// 等待任务完成
	<-rt.host.Done()

	// 检查最终状态
	m.mu.Lock()
	progress := m.loadProgress(rt.task.OutputDir)
	m.mu.Unlock()

	now := time.Now()
	if progress != nil && progress.Phase == domain.PhaseComplete {
		rt.task.Status = domain.TaskCompleted
		rt.task.CompletedAt = &now
		slog.Info("任务完成", "module", "library", "task_id", id,
			"chapters", len(progress.CompletedChapters), "words", progress.TotalWordCount)
		m.emit(ManagerEvent{
			Time:   now,
			Kind:   "task_completed",
			TaskID: id,
			Task:   &rt.task,
			Summary: fmt.Sprintf("创作完成: %d 章 %d 字", len(progress.CompletedChapters), progress.TotalWordCount),
		})
	} else {
		rt.task.Status = domain.TaskPaused
		slog.Info("任务暂停", "module", "library", "task_id", id)
		m.emit(ManagerEvent{
			Time:   now,
			Kind:   "task_paused",
			TaskID: id,
			Task:   &rt.task,
			Summary: "创作暂停",
		})
	}

	_ = m.libStore.UpdateTask(rt.task)

	// 清理运行状态
	m.mu.Lock()
	delete(m.tasks, id)
	m.mu.Unlock()

	// 队列中还有任务 → 自动启动下一个
	m.runNextInQueue()
}

// tryResume 检查是否需要恢复并执行。
func (m *Manager) tryResume(rt *runningTask) (string, error) {
	// 需要创建一个临时 Store 来检查进度
	// Host.Resume 内部会处理恢复逻辑
	label, err := rt.host.Resume()
	if err != nil {
		return "", err
	}
	return label, nil
}

// handleTaskError 处理任务执行失败。
func (m *Manager) handleTaskError(rt *runningTask, err error) {
	now := time.Now()
	rt.task.Status = domain.TaskFailed
	rt.task.Error = err.Error()
	rt.task.CompletedAt = &now

	slog.Error("任务失败", "module", "library", "task_id", rt.task.ID, "err", err)
	_ = m.libStore.UpdateTask(rt.task)

	m.emit(ManagerEvent{
		Time:   now,
		Kind:   "task_failed",
		TaskID: rt.task.ID,
		Task:   &rt.task,
		Error:  err.Error(),
		Summary: fmt.Sprintf("任务失败: %s", err.Error()),
	})

	m.mu.Lock()
	delete(m.tasks, rt.task.ID)
	m.mu.Unlock()

	m.runNextInQueue()
}

// runNextInQueue 从队列中取下一个排队任务并启动。
func (m *Manager) runNextInQueue() {
	tasks, err := m.libStore.LoadTasks()
	if err != nil {
		slog.Warn("加载队列失败", "module", "library", "err", err)
		return
	}
	for _, t := range tasks {
		if t.Status == domain.TaskQueued {
			slog.Info("自动启动下一个任务", "module", "library", "task_id", t.ID)
			if err := m.StartTask(t.ID); err != nil {
				slog.Error("启动任务失败", "module", "library", "task_id", t.ID, "err", err)
			}
			return
		}
	}
}

// loadProgress 从输出目录读取进度摘要。
func (m *Manager) loadProgress(outputDir string) *domain.Progress {
	s := store.NewStore(outputDir)
	p, err := s.Progress.Load()
	if err != nil {
		return nil
	}
	return p
}

// LoadBookProgress 读取指定书籍的进度摘要，供 UI 使用。
func (m *Manager) LoadBookProgress(id domain.BookID) *domain.BookProgress {
	task, err := m.GetTask(id)
	if err != nil || task == nil {
		return nil
	}
	p := m.loadProgress(task.OutputDir)
	if p == nil {
		return nil
	}
	return &domain.BookProgress{
		Phase:         string(p.Phase),
		Chapters:      len(p.CompletedChapters),
		TotalChapters: p.TotalChapters,
		WordCount:     p.TotalWordCount,
		NovelName:     p.NovelName,
		CurrentFlow:   string(p.Flow),
	}
}

// Snapshot 返回图书馆当前状态快照。
func (m *Manager) Snapshot() domain.TaskQueueSnapshot {
	tasks, _ := m.libStore.LoadTasks()
	if tasks == nil {
		tasks = []domain.BookTask{}
	}

	var active *domain.BookID
	queued := 0
	for _, t := range tasks {
		if t.Status == domain.TaskRunning {
			id := t.ID
			active = &id
		}
		if t.Status == domain.TaskQueued {
			queued++
		}
	}

	return domain.TaskQueueSnapshot{
		Tasks:      tasks,
		ActiveTask: active,
		QueueSize:  queued,
	}
}

// GetHost 获取指定任务的 Host 实例（仅对运行中的任务有效）。
func (m *Manager) GetHost(id domain.BookID) *host.Host {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt, ok := m.tasks[id]; ok {
		return rt.host
	}
	return nil
}

// ── 事件分发 ──

func (m *Manager) emit(ev ManagerEvent) {
	// 防止 Close() 后发送到已关闭的 channel
	select {
	case <-m.done:
		return
	default:
	}

	select {
	case m.events <- ev:
	default:
	}

	m.subMu.Lock()
	for ch := range m.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
	m.subMu.Unlock()
}

// Close 关闭管理器，清理所有运行中的任务。
func (m *Manager) Close() {
	m.mu.Lock()
	for _, rt := range m.tasks {
		rt.host.Close()
		rt.cancel()
	}
	m.mu.Unlock()
	close(m.done)
	close(m.events)
}

// Done 返回 done channel。
func (m *Manager) Done() <-chan struct{} { return m.done }
