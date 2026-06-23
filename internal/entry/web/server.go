package web

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/NightYuYyy/ainovel-cli/assets"
	"github.com/NightYuYyy/ainovel-cli/internal/bootstrap"
	"github.com/NightYuYyy/ainovel-cli/internal/domain"
	"github.com/NightYuYyy/ainovel-cli/internal/library"
	"github.com/NightYuYyy/ainovel-cli/internal/logger"
)

//go:embed index.html
var indexHTML []byte

// Options 配置 Web 服务启动参数。
type Options struct {
	Addr string
}

// sseMsg 是推送给所有 SSE 客户端的消息。
type sseMsg struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// Server 是 Web 服务，通过 Library Manager 管理多本书。
type Server struct {
	lib    *library.Manager
	cfg    bootstrap.Config
	bundle assets.Bundle

	clients   map[chan sseMsg]struct{}
	clientsMu sync.Mutex
}
// RunLibrary 启动支持多任务管理的 Web 服务。
func RunLibrary(cfg bootstrap.Config, bundle assets.Bundle, opts Options) error {
	lib := library.New(cfg, bundle)
	cleanup := logger.SetupFile(lib.LibDir(), "web.log", false)
	defer cleanup()
	defer lib.Close()

	s := &Server{
		lib:     lib,
		cfg:     cfg,
		bundle:  bundle,
		clients: make(map[chan sseMsg]struct{}),
	}

	// 广播 Manager 事件
	go s.pumpLibrary()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	// 任务管理 API
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/", s.handleTaskByID)
	// 原有 API（兼容单书模式，使用活跃任务的 host）
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/model", s.handleModel)
	mux.HandleFunc("/api/thinking", s.handleThinking)
	mux.HandleFunc("/api/provider", s.handleProvider)

	addr := opts.Addr
	if addr == "" {
		addr = ":8080"
	}

	slog.Info("web 服务启动", "module", "web", "addr", addr, "lib_dir", lib.LibDir())
	fmt.Printf("ainovel-cli web 服务: http://localhost%s\n", addr)

	srv := &http.Server{Addr: addr, Handler: mux}
	return srv.ListenAndServe()
}

// pumpLibrary 广播 Library Manager 事件。
func (s *Server) pumpLibrary() {
	for ev := range s.lib.Events() {
		s.broadcast(sseMsg{Type: "library_event", Data: ev})
	}
	s.broadcast(sseMsg{Type: "closed"})
}

// ── HTTP handlers ──

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// handleTasks 处理任务集合操作。
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tasks, err := s.lib.ListTasks()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		// 附加每本书的进度
		type taskWithProgress struct {
			domain.BookTask
			Progress *domain.BookProgress `json:"progress,omitempty"`
		}
		result := make([]taskWithProgress, len(tasks))
		for i, t := range tasks {
			result[i] = taskWithProgress{
				BookTask: t,
				Progress: s.lib.LoadBookProgress(t.ID),
			}
		}
		writeJSON(w, http.StatusOK, result)

	case http.MethodPost:
		var req struct {
			BookName string `json:"book_name"`
			Prompt   string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if req.Prompt == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
			return
		}
		if req.BookName == "" {
			req.BookName = req.Prompt
		}
		id, err := s.lib.SubmitTask(req.BookName, req.Prompt)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": string(id)})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleTaskByID 处理单个任务操作。
func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	// 从 URL 提取 task ID: /api/tasks/{id}/action
	path := r.URL.Path[len("/api/tasks/"):]
	id := domain.BookID(path)

	// 检查是否有 action 后缀
	action := ""
	if idx := len(path); idx > 0 {
		for i, c := range path {
			if c == '/' {
				id = domain.BookID(path[:i])
				action = path[i+1:]
				break
			}
		}
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		task, err := s.lib.GetTask(id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if task == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		writeJSON(w, http.StatusOK, task)

	case r.Method == http.MethodPost && action == "start":
		if err := s.lib.StartTask(id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "started"})

	case r.Method == http.MethodPost && action == "pause":
		if err := s.lib.PauseTask(id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})

	case r.Method == http.MethodDelete && action == "":
		if err := s.lib.DeleteTask(id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSnapshot 获取当前活跃任务的快照。
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.lib.Snapshot()
	writeJSON(w, http.StatusOK, snap)
}

// handleConfig 获取配置。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg)
}

// handleModel 切换模型。
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleThinking 切换思考强度。
func (s *Server) handleThinking(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleProvider 获取 provider 信息。
func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request) {
	provider := s.cfg.Provider
	models := s.cfg.CandidateModels(provider)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"provider": provider,
		"model":    s.cfg.ModelName,
		"models":   models,
	})
}

// ── 工具函数 ──

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) broadcast(msg sseMsg) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}


