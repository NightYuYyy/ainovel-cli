package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/NightYuYyy/ainovel-cli/assets"
	"github.com/NightYuYyy/ainovel-cli/internal/bootstrap"
	"github.com/NightYuYyy/ainovel-cli/internal/library"
	"github.com/NightYuYyy/ainovel-cli/internal/logger"
)

// RunLibrary 启动图书馆 TUI - 任务队列管理界面。
// 这是新的主入口，替代原有的单书 Run()。
func RunLibrary(cfg bootstrap.Config, bundle assets.Bundle, version string) error {
	lib := library.New(cfg, bundle)
	cleanup := logger.SetupFile(lib.LibDir(), "tui.log", false)
	defer cleanup()
	defer lib.Close()

	m := NewLibraryModel(lib, version)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
