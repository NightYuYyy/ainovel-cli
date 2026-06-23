package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/NightYuYyy/ainovel-cli/assets"
	"github.com/NightYuYyy/ainovel-cli/internal/bootstrap"
	"github.com/NightYuYyy/ainovel-cli/internal/domain"
	"github.com/NightYuYyy/ainovel-cli/internal/entry/startup"
	"github.com/NightYuYyy/ainovel-cli/internal/host"
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
	cfg    *bootstrap.Config
	bundle assets.Bundle

	clients   map[chan sseMsg]struct{}
	clientsMu sync.Mutex

	// 共创规划
	cocreateHost *host.Host
	cocreateSess *startup.CoCreateSession
	cocreateMu   sync.Mutex
}

// RunLibrary 启动支持多任务管理的 Web 服务。
func RunLibrary(cfg bootstrap.Config, bundle assets.Bundle, opts Options) error {
	lib := library.New(cfg, bundle)
	cleanup := logger.SetupFile(lib.LibDir(), "web.log", false)
	lib.RecoverStaleTasks()
	defer cleanup()
	defer lib.Close()

	s := &Server{
		lib:     lib,
		cfg:     &cfg,
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
	mux.HandleFunc("/api/cocreate", s.handleCoCreate)
	mux.HandleFunc("/api/start", s.handleStart)
	mux.HandleFunc("/api/events", s.handleEvents)

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

	case r.Method == http.MethodGet && action == "snapshot":
		h := s.lib.GetHost(id)
		if h == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not running"})
			return
		}
		writeJSON(w, http.StatusOK, h.Snapshot())

	case r.Method == http.MethodGet && action == "progress":
		bp := s.lib.LoadBookProgress(id)
		if bp == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "progress not found"})
			return
		}
		writeJSON(w, http.StatusOK, bp)

	case r.Method == http.MethodGet && action == "events":
		h := s.lib.GetHost(id)
		if h == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not running"})
			return
		}
		s.serveHostEvents(w, r, h, id)

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

// configView 是前端设置面板所需的数据格式。
type configView struct {
	Provider      string                       `json:"provider"`
	Model         string                       `json:"model"`
	Style         string                       `json:"style,omitempty"`
	Thinking      string                       `json:"thinking,omitempty"`
	ContextWindow int                          `json:"context_window,omitempty"`
	Providers     []string                     `json:"providers"`           // provider 名称列表
	Models        map[string][]string          `json:"models"`              // provider → model 列表
	Roles         []roleView                   `json:"roles"`               // 角色配置数组
	Budget        bootstrap.BudgetConfig       `json:"budget,omitempty"`
	Notify        bootstrap.NotifyConfig       `json:"notify,omitempty"`
}

type roleView struct {
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Thinking string `json:"thinking,omitempty"`
}

// makeConfigView 将服务端 Config 转为前端需要的格式。
func makeConfigView(cfg *bootstrap.Config) configView {
	v := configView{
		Provider:      cfg.Provider,
		Model:         cfg.ModelName,
		Style:         cfg.Style,
		Thinking:      cfg.Thinking,
		ContextWindow: cfg.ContextWindow,
		Budget:        cfg.Budget,
		Notify:        cfg.Notify,
		Providers:     make([]string, 0, len(cfg.Providers)),
		Models:        make(map[string][]string),
	}

	for name := range cfg.Providers {
		v.Providers = append(v.Providers, name)
	}
	// 收集每个 provider 的候选模型
	for _, name := range v.Providers {
		v.Models[name] = cfg.CandidateModels(name)
	}

	// 已知角色列表
	knownRoles := []string{"coordinator", "architect", "writer", "editor"}
	for _, role := range knownRoles {
		rv := roleView{Role: role}
		if rc, ok := cfg.Roles[role]; ok {
			rv.Provider = rc.Provider
			rv.Model = rc.Model
			rv.Thinking = rc.Thinking
		}
		v.Roles = append(v.Roles, rv)
	}

	return v
}

// handleConfig 获取/更新配置。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, makeConfigView(s.cfg))

	case http.MethodPost:
		var req bootstrap.Config
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		// 合并写入：只覆盖前端传了的字段
		if req.Provider != "" {
			s.cfg.Provider = req.Provider
		}
		if req.ModelName != "" {
			s.cfg.ModelName = req.ModelName
		}
		if req.Style != "" {
			s.cfg.Style = req.Style
		}
		if req.Thinking != "" {
			s.cfg.Thinking = req.Thinking
		}
		if req.ContextWindow > 0 {
			s.cfg.ContextWindow = req.ContextWindow
		}
		if len(req.Providers) > 0 {
			s.cfg.Providers = req.Providers
		}
		if len(req.Roles) > 0 {
			s.cfg.Roles = req.Roles
		}
		if err := s.persistConfig(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, makeConfigView(s.cfg))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleModel 切换模型。
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Role     string `json:"role"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Role == "" || req.Role == "default" {
		if req.Provider != "" {
			s.cfg.Provider = req.Provider
		}
		if req.Model != "" {
			s.cfg.ModelName = req.Model
		}
	} else {
		if s.cfg.Roles == nil {
			s.cfg.Roles = make(map[string]bootstrap.RoleConfig)
		}
		rc := s.cfg.Roles[req.Role]
		if req.Provider != "" {
			rc.Provider = req.Provider
		}
		if req.Model != "" {
			rc.Model = req.Model
		}
		s.cfg.Roles[req.Role] = rc
	}

	if err := s.persistConfig(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleThinking 切换思考强度。
func (s *Server) handleThinking(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Role  string `json:"role"`
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Role == "" || req.Role == "default" {
		s.cfg.Thinking = req.Level
	} else {
		if s.cfg.Roles == nil {
			s.cfg.Roles = make(map[string]bootstrap.RoleConfig)
		}
		rc := s.cfg.Roles[req.Role]
		rc.Thinking = req.Level
		s.cfg.Roles[req.Role] = rc
	}

	if err := s.persistConfig(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleProvider 管理 provider 配置。
func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		provider := s.cfg.Provider
		models := s.cfg.CandidateModels(provider)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"provider": provider,
			"model":    s.cfg.ModelName,
			"models":   models,
		})

	case http.MethodPost:
		var req struct {
			Action  string   `json:"action"`            // "save" or "delete"
			Name    string   `json:"name"`              // provider 名称
			Type    string   `json:"type,omitempty"`    // API 协议类型
			APIKey  string   `json:"api_key,omitempty"` // API Key
			BaseURL string   `json:"base_url,omitempty"`// Base URL
			Models  []string `json:"models,omitempty"`  // 模型列表
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		switch req.Action {
		case "save":
			if req.Name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
				return
			}
			if s.cfg.Providers == nil {
				s.cfg.Providers = make(map[string]bootstrap.ProviderConfig)
			}
			pc := s.cfg.Providers[req.Name]
			if req.Type != "" {
				pc.Type = req.Type
			}
			if req.APIKey != "" {
				pc.APIKey = req.APIKey
			}
			if req.BaseURL != "" {
				pc.BaseURL = req.BaseURL
			}
			if req.Models != nil {
				pc.Models = req.Models
			}
			s.cfg.Providers[req.Name] = pc

			if err := s.persistConfig(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			slog.Info("provider 已保存", "module", "web", "name", req.Name)
			writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})

		case "delete":
			if req.Name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
				return
			}
			// 检查是否被默认配置引用
			if s.cfg.Provider == req.Name {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "该渠道正在被默认配置引用，无法删除"})
				return
			}
			// 检查是否被角色引用
			for role, rc := range s.cfg.Roles {
				if rc.Provider == req.Name {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("该渠道正在被角色 %q 引用，无法删除", role)})
					return
				}
				for _, f := range rc.Fallbacks {
					if f.Provider == req.Name {
						writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("该渠道正在被角色 %q 的 fallback 引用，无法删除", role)})
						return
					}
				}
			}
			if _, ok := s.cfg.Providers[req.Name]; !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider not found"})
				return
			}
			delete(s.cfg.Providers, req.Name)

			if err := s.persistConfig(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			slog.Info("provider 已删除", "module", "web", "name", req.Name)
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action: " + req.Action})
		}

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// cocreateSessionState 是可持久化的共创会话状态。
type cocreateSessionState struct {
	Messages    []cocreateMsgState `json:"messages"`
	Draft       string             `json:"draft"`
	Ready       bool               `json:"ready"`
	Suggestions []string           `json:"suggestions"`
}

type cocreateMsgState struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *Server) cocreateStatePath() string {
	return bootstrap.DefaultConfigDir() + "/cocreate_session.json"
}

func (s *Server) saveCoCreateSession() {
	if s.cocreateSess == nil {
		return
	}
	state := cocreateSessionState{
		Draft:       s.cocreateSess.DraftPrompt(),
		Ready:       s.cocreateSess.Ready(),
		Suggestions: s.cocreateSess.Suggestions(),
	}
	for _, m := range s.cocreateSess.History() {
		state.Messages = append(state.Messages, cocreateMsgState{Role: m.Role, Content: m.Content})
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		slog.Error("cocreate 序列化失败", "module", "web", "err", err)
		return
	}
	path := s.cocreateStatePath()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Error("cocreate 保存失败", "module", "web", "path", path, "err", err)
		return
	}
	slog.Info("cocreate 会话已保存", "module", "web", "path", path)
}


// handleCoCreate 处理共创规划的多轮对话。
func (s *Server) handleCoCreate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.cocreateMu.Lock()
		defer s.cocreateMu.Unlock()
		if s.cocreateSess == nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{"messages": []cocreateMsgState{}})
			return
		}
		state := cocreateSessionState{
			Draft:       s.cocreateSess.DraftPrompt(),
			Ready:       s.cocreateSess.Ready(),
			Suggestions: s.cocreateSess.Suggestions(),
		}
		for _, m := range s.cocreateSess.History() {
			state.Messages = append(state.Messages, cocreateMsgState{Role: m.Role, Content: m.Content})
		}
		writeJSON(w, http.StatusOK, state)
		return

	case http.MethodPost:
		var req struct {
			Message string `json:"message"`
			Reset   bool   `json:"reset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		s.cocreateMu.Lock()
		defer s.cocreateMu.Unlock()

		// 重置会话
		if req.Reset {
			s.cocreateSess = nil
			os.Remove(s.cocreateStatePath())
			writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
			return
		}

		if req.Message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
			return
		}

		// 延迟初始化 host 和 session
		if s.cocreateHost == nil {
			h, err := host.New(*s.cfg, s.bundle)
			if err != nil {
				slog.Error("cocreate host 创建失败", "module", "web", "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建会话失败: " + err.Error()})
				return
			}
			s.cocreateHost = h
		}
		if s.cocreateSess == nil {
			s.cocreateSess = startup.NewCoCreateSession(req.Message)
		} else {
			s.cocreateSess.AppendUser(req.Message)
		}

		slog.Info("cocreate 消息", "module", "web", "msg", req.Message)

		// 设置 SSE 响应头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
			return
		}

		// 发送 thinking 事件
		sseWrite(w, flusher, `{"type":"thinking"}`)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		reply, err := s.cocreateHost.CoCreateStream(ctx, s.cocreateSess.History(), func(kind, text string) {
			if kind == string(host.CoCreateProgressReply) {
				sseWrite(w, flusher, `{"type":"reply"}`)
			}
		})

		if err != nil {
			slog.Error("cocreate 调用失败", "module", "web", "err", err)
			sseWrite(w, flusher, `{"type":"error","data":"`+err.Error()+`"}`)
			return
		}

		s.cocreateSess.ApplyReply(reply)
		s.saveCoCreateSession()

		done := map[string]interface{}{
			"type": "cocreate_done",
			"data": map[string]interface{}{
				"message":     reply.Message,
				"prompt":      s.cocreateSess.DraftPrompt(),
				"ready":       s.cocreateSess.Ready(),
				"suggestions": s.cocreateSess.Suggestions(),
			},
		}
		data, _ := json.Marshal(done)
		sseWrite(w, flusher, string(data))

		slog.Info("cocreate 完成", "module", "web", "ready", s.cocreateSess.Ready())

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleStart 从共创规划或快速开始启动创作。
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Prompt   string `json:"prompt"`
		BookName string `json:"book_name"`
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

	slog.Info("启动创作", "module", "web", "book_name", req.BookName)

	id, err := s.lib.SubmitTask(req.BookName, req.Prompt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := s.lib.StartTask(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	slog.Info("创作已启动", "module", "web", "task_id", id)
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "started",
		"id":     string(id),
	})
}

// handleEvents SSE 事件流，供工作台实时更新。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// 订阅库事件
	ch := make(chan library.ManagerEvent, 32)
	s.lib.Subscribe(ch)
	defer s.lib.Unsubscribe(ch)

	// 定期推送快照
	snapTicker := time.NewTicker(3 * time.Second)
	defer snapTicker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(map[string]interface{}{
				"type": "event",
				"data": ev,
			})
			sseWrite(w, flusher, string(data))
		case <-snapTicker.C:
			snap := s.lib.Snapshot()
			// Attach progress for display
			type taskWithProgress struct {
				domain.BookTask
				Progress *domain.BookProgress `json:"progress,omitempty"`
			}
			tasks := make([]taskWithProgress, len(snap.Tasks))
			for i, t := range snap.Tasks {
				tasks[i] = taskWithProgress{
					BookTask: t,
					Progress: s.lib.LoadBookProgress(t.ID),
				}
			}
			data, _ := json.Marshal(map[string]interface{}{
				"type": "snapshot",
				"data": map[string]interface{}{
					"tasks":       tasks,
					"active_task": snap.ActiveTask,
					"queue_size":  snap.QueueSize,
				},
			})
			sseWrite(w, flusher, string(data))
		}
	}
}

// serveHostEvents 推送单个任务的运行时事件和快照。
func (s *Server) serveHostEvents(w http.ResponseWriter, r *http.Request, h *host.Host, id domain.BookID) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// 发送初始快照
	snap := h.Snapshot()
	data, _ := json.Marshal(map[string]interface{}{"type": "snapshot", "data": snap})
	sseWrite(w, flusher, string(data))

	snapTicker := time.NewTicker(5 * time.Second)
	defer snapTicker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.Done():
			sseWrite(w, flusher, `{"type":"closed"}`)
			return
		case ev, ok := <-h.Events():
			if !ok {
				return
			}
			data, _ := json.Marshal(map[string]interface{}{"type": "event", "data": ev})
			sseWrite(w, flusher, string(data))
		case <-snapTicker.C:
			snap := h.Snapshot()
			data, _ := json.Marshal(map[string]interface{}{"type": "snapshot", "data": snap})
			sseWrite(w, flusher, string(data))
		}
	}
}
// sseWrite 写入一条 SSE 格式的消息。
func sseWrite(w http.ResponseWriter, flusher http.Flusher, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// persistConfig 将当前内存配置持久化到 ~/.ainovel/config.json。
func (s *Server) persistConfig() error {
	path := bootstrap.DefaultConfigPath()
	if path == "" {
		return fmt.Errorf("无法确定配置路径")
	}
	s.cfg.FillDefaults()
	return bootstrap.SaveConfig(path, *s.cfg)
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
