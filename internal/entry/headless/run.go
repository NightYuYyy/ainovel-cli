package headless

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/NightYuYyy/ainovel-cli/assets"
	"github.com/NightYuYyy/ainovel-cli/internal/bootstrap"
	"github.com/NightYuYyy/ainovel-cli/internal/domain"
	"github.com/NightYuYyy/ainovel-cli/internal/library"
	"github.com/NightYuYyy/ainovel-cli/internal/logger"
)

// Options 配置 headless 运行参数。
type Options struct {
	Prompt string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// RunLibrary 以无界面模式通过图书馆管理器运行任务。
func RunLibrary(cfg bootstrap.Config, bundle assets.Bundle, opts Options) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	lib := library.New(cfg, bundle)
	cleanup := logger.SetupFile(lib.LibDir(), "headless.log", false)
	defer cleanup()
	defer lib.Close()

	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" {
		return listTasks(lib, stdout)
	}

	id, err := lib.SubmitTask(prompt, prompt)
	if err != nil {
		return fmt.Errorf("submit task: %w", err)
	}
	fmt.Fprintf(stdout, "任务已提交: %s\n", id)

	if err := lib.StartTask(id); err != nil {
		return fmt.Errorf("start task: %w", err)
	}

	return consumeLibraryEvents(lib, stdout, stderr)
}

func listTasks(lib *library.Manager, w io.Writer) error {
	tasks, err := lib.ListTasks()
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}

	if len(tasks) == 0 {
		fmt.Fprintln(w, "暂无书籍任务。")
		fmt.Fprintln(w, "使用 --prompt 提交新任务，如：ainovel-cli --headless --prompt '写一本武侠小说'")
		return nil
	}

	fmt.Fprintf(w, "共 %d 个任务：\n\n", len(tasks))
	for _, t := range tasks {
		status := taskStatusLabel(t.Status)
		name := t.BookName
		if name == "" {
			name = "(未命名)"
		}
		progress := ""
		if bp := lib.LoadBookProgress(t.ID); bp != nil && bp.Chapters > 0 {
			progress = fmt.Sprintf(" [%d章/%d字]", bp.Chapters, bp.WordCount)
		}
		fmt.Fprintf(w, "  %s  %s  %s%s\n", t.ID, status, name, progress)
	}
	return nil
}

func consumeLibraryEvents(lib *library.Manager, stdout, stderr io.Writer) error {
	for ev := range lib.Events() {
		switch ev.Kind {
		case "task_queued":
			fmt.Fprintf(stdout, "[队列] %s\n", ev.Summary)
		case "task_started":
			fmt.Fprintf(stdout, "[开始] %s\n", ev.Summary)
		case "task_paused":
			fmt.Fprintf(stdout, "[暂停] %s\n", ev.Summary)
			return nil
		case "task_completed":
			fmt.Fprintf(stdout, "[完成] %s\n", ev.Summary)
			return nil
		case "task_failed":
			fmt.Fprintf(stderr, "[失败] %s: %s\n", ev.Summary, ev.Error)
			return fmt.Errorf("task failed: %s", ev.Error)
		}
	}
	return nil
}

func taskStatusLabel(s domain.TaskStatus) string {
	switch s {
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
		return string(s)
	}
}
