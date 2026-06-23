package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/NightYuYyy/ainovel-cli/internal/domain"
	"github.com/NightYuYyy/ainovel-cli/internal/host"
	"github.com/NightYuYyy/ainovel-cli/internal/library"
)

// libraryState 管理图书馆视图的交互状态。
type libraryState struct {
	tasks        []domain.BookTask
	progress     map[domain.BookID]*domain.BookProgress
	selectedIdx  int
	showConfirm  bool
	confirmID    domain.BookID
	lastRefresh  time.Time
	err          string
}

var (
	libTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205")).
			MarginBottom(1)

	libTaskStyle = lipgloss.NewStyle().
			Padding(0, 1)

	libSelectedStyle = lipgloss.NewStyle().
				Padding(0, 1).
				Background(lipgloss.Color("57")).
				Foreground(lipgloss.Color("255"))

	libStatusQueued    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	libStatusRunning   = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	libStatusPaused    = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	libStatusCompleted = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	libStatusFailed    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))

	libHelpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
)

func statusStyle(status domain.TaskStatus) lipgloss.Style {
	switch status {
	case domain.TaskQueued:
		return libStatusQueued
	case domain.TaskRunning:
		return libStatusRunning
	case domain.TaskPaused:
		return libStatusPaused
	case domain.TaskCompleted:
		return libStatusCompleted
	case domain.TaskFailed:
		return libStatusFailed
	default:
		return libStatusQueued
	}
}

func statusLabel(status domain.TaskStatus) string {
	switch status {
	case domain.TaskQueued:
		return "排队中"
	case domain.TaskRunning:
		return "写作中"
	case domain.TaskPaused:
		return "已暂停"
	case domain.TaskCompleted:
		return "已完成"
	case domain.TaskFailed:
		return "失败"
	default:
		return string(status)
	}
}

func newLibraryState() *libraryState {
	return &libraryState{
		progress: make(map[domain.BookID]*domain.BookProgress),
	}
}

func (ls *libraryState) refresh(lib *library.Manager) {
	tasks, err := lib.ListTasks()
	if err != nil {
		ls.err = fmt.Sprintf("加载任务失败: %v", err)
		return
	}
	ls.err = ""

	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.After(tasks[j].CreatedAt)
	})
	ls.tasks = tasks

	for _, t := range tasks {
		ls.progress[t.ID] = lib.LoadBookProgress(t.ID)
	}
	ls.lastRefresh = time.Now()
}

func (m Model) renderLibrary(width, height int) string {
	ls := m.libraryState
	if ls == nil {
		return "图书馆未初始化"
	}

	var b strings.Builder

	b.WriteString(libTitleStyle.Render("📚 书籍管理"))
	b.WriteString("\n\n")

	if ls.err != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(ls.err))
		b.WriteString("\n\n")
	}

	if len(ls.tasks) == 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("  暂无书籍任务"))
		b.WriteString("\n")
		b.WriteString(libHelpStyle.Render("  输入书名和需求开始创作，如：写一本东方玄幻长篇，主角从边陲小城起步"))
		b.WriteString("\n")
	} else {
		maxVisible := (height - 8) / 3
		if maxVisible < 1 {
			maxVisible = 1
		}

		start := ls.selectedIdx - maxVisible/2
		if start < 0 {
			start = 0
		}
		end := start + maxVisible
		if end > len(ls.tasks) {
			end = len(ls.tasks)
			start = end - maxVisible
			if start < 0 {
				start = 0
			}
		}

		for i := start; i < end; i++ {
			task := ls.tasks[i]
			line := m.renderTaskLine(task, i, width)
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(libHelpStyle.Render("  ↑↓ 选择  Enter 开始/续写  d 删除  n 新任务  q 退出"))
	b.WriteString("\n")
	b.WriteString(libHelpStyle.Render(fmt.Sprintf("  共 %d 个任务", len(ls.tasks))))

	if ls.showConfirm {
		return m.renderConfirmDialog(b.String(), width, height)
	}

	return b.String()
}

func (m Model) renderTaskLine(task domain.BookTask, idx, width int) string {
	selected := idx == m.libraryState.selectedIdx

	st := statusStyle(task.Status)
	statusText := st.Render(fmt.Sprintf("[%s]", statusLabel(task.Status)))

	name := task.BookName
	if name == "" {
		name = "(未命名)"
	}
	if len(name) > 40 {
		name = name[:37] + "..."
	}

	progress := ""
	if bp := m.libraryState.progress[task.ID]; bp != nil && bp.Chapters > 0 {
		progress = fmt.Sprintf(" %d章/%d字", bp.Chapters, bp.WordCount)
	}

	timeStr := task.CreatedAt.Format("01-02 15:04")

	line := fmt.Sprintf("  %s %s  %s%s",
		statusText,
		name,
		lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(timeStr),
		lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render(progress),
	)

	if selected {
		return libSelectedStyle.Render(line)
	}
	return libTaskStyle.Render(line)
}

func (m Model) renderConfirmDialog(content string, width, height int) string {
	dialog := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("196")).
		Padding(1, 2).
		Width(50).
		Align(lipgloss.Center)

	msg := fmt.Sprintf("确定要删除任务 %s 吗？\n\n  y 确认  n 取消", m.libraryState.confirmID)
	rendered := dialog.Render(msg)

	lines := strings.Split(content, "\n")
	dl := strings.Split(rendered, "\n")
	mid := height / 2
	if mid >= len(lines) {
		mid = len(lines) - 1
	}
	if len(dl) > 0 {
		for i, dlLine := range dl {
			idx := mid - len(dl)/2 + i
			if idx >= 0 && idx < len(lines) {
				lines[idx] = dlLine
			}
		}
	}
	return strings.Join(lines, "\n")
}

// handleLibraryNav 处理图书馆视图的上下导航。
func (m *Model) handleLibraryNav(delta int) {
	ls := m.libraryState
	if ls == nil || len(ls.tasks) == 0 {
		return
	}
	ls.selectedIdx += delta
	if ls.selectedIdx < 0 {
		ls.selectedIdx = 0
	}
	if ls.selectedIdx >= len(ls.tasks) {
		ls.selectedIdx = len(ls.tasks) - 1
	}
}

// handleLibraryEnter 处理图书馆视图的 Enter 键。
func (m *Model) handleLibraryEnter() (tea.Model, tea.Cmd) {
	ls := m.libraryState
	if ls == nil {
		return m, nil
	}

	// 确认对话框处理
	if ls.showConfirm {
		return m, nil // 由 rune 键盘处理 y/n
	}

	text := strings.TrimSpace(m.textarea.Value())
	m.textarea.Reset()
	m.refitTextareaHeight()

	// 有输入文本 → 提交新任务
	if text != "" {
		_, err := m.libManager.SubmitTask(text, text) // prompt 与 bookName 相同，后续从 premise 提取书名
		if err != nil {
			ls.err = fmt.Sprintf("提交任务失败: %v", err)
			return m, nil
		}
		ls.refresh(m.libManager)
		return m, nil
	}

	// 无输入 → 启动选中任务
	if len(ls.tasks) == 0 {
		return m, nil
	}
	task := ls.tasks[ls.selectedIdx]
	return m.startLibraryTask(task)
}

func (m *Model) startLibraryTask(task domain.BookTask) (tea.Model, tea.Cmd) {
	if task.Status == domain.TaskRunning {
		return m, nil
	}

	if err := m.libManager.StartTask(task.ID); err != nil {
		m.libraryState.err = fmt.Sprintf("启动任务失败: %v", err)
		return m, nil
	}

	// 获取任务的 Host 实例（已在 runTask goroutine 中启动）
	eng := m.libManager.GetHost(task.ID)
	if eng == nil {
		m.libraryState.err = "无法获取任务 Host"
		return m, nil
	}

	// 设置 runtime 和 bridge
	m.runtime = eng
	m.askBridge = newAskUserBridge()
	eng.AskUser().SetHandler(m.askBridge.handler)


	// 直接进入运行模式：Host 已被 Manager 启动，无需再调 Resume/Prompt
	m.textarea.Placeholder = defaultSteerPlaceholder()
	enableMouse := m.enterRunning()

	return m, tea.Batch(
		enableMouse,
		listenEvents(m.runtime),
		listenAskUser(m.askBridge),
		listenDone(m.runtime),
		listenStream(m.runtime),
		tickSnapshot(m.runtime),
	)
}

func (m *Model) handleLibraryTaskDone(msg doneMsg) (tea.Model, tea.Cmd, bool) {
	// 清理运行时状态
	m.runtime = nil
	m.askBridge = nil
	m.snapshot = host.UISnapshot{}
	m.events = nil
	m.eventIndex = make(map[string]int)
	m.streamRounds = nil
	m.streamBuf.Reset()

	// 返回图书馆视图
	m.mode = modeLibrary
	m.libraryState.refresh(m.libManager)
	m.textarea.Placeholder = "输入新书名和需求，如：写一本东方玄幻长篇..."
	m.textarea.Focus()
	m.mouseOff = true

	return m, tea.DisableMouse, true
}
func (m *Model) handleLibraryKeyRune(r rune) (tea.Model, tea.Cmd, bool) {
	ls := m.libraryState
	if ls == nil {
		return m, nil, false
	}

	switch r {
	case 'd', 'D':
		if len(ls.tasks) == 0 {
			return m, nil, true
		}
		task := ls.tasks[ls.selectedIdx]
		if task.Status == domain.TaskRunning {
			ls.err = "请先暂停正在运行的任务"
			return m, nil, true
		}
		if !ls.showConfirm {
			ls.showConfirm = true
			ls.confirmID = task.ID
			return m, nil, true
		}
		return m, nil, true
	case 'y', 'Y':
		if ls.showConfirm {
			if err := m.libManager.DeleteTask(ls.confirmID); err != nil {
				ls.err = fmt.Sprintf("删除失败: %v", err)
			}
			ls.showConfirm = false
			ls.refresh(m.libManager)
			return m, nil, true
		}
	case 'n', 'N':
		if ls.showConfirm {
			ls.showConfirm = false
			return m, nil, true
		}
	case 'q', 'Q':
		return m, tea.Quit, true
	}
	return m, nil, false
}
