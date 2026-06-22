package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/NightYuYyy/ainovel-cli/assets"
	"github.com/NightYuYyy/ainovel-cli/internal/bootstrap"
	"github.com/NightYuYyy/ainovel-cli/internal/domain"
	"github.com/NightYuYyy/ainovel-cli/internal/entry/startup"
	"github.com/NightYuYyy/ainovel-cli/internal/host"
	"github.com/NightYuYyy/ainovel-cli/internal/host/exp"
	"github.com/NightYuYyy/ainovel-cli/internal/logger"
	"github.com/NightYuYyy/ainovel-cli/internal/tools"
)

//go:embed index.html
var indexHTML []byte

// Options 配置 Web 服务启动参数。
type Options struct {
	Addr string // 监听地址，默认 :8080
}

// sseMsg 是推送给所有 SSE 客户端的消息。
type sseMsg struct {
	Type string      `json:"type"`           // event / stream / stream_clear / done / closed
	Data interface{} `json:"data,omitempty"` // Event / string / nil
}

// Server 是 Web 服务，包装一个 Host 实例并通过 HTTP/SSE 暴露其能力。
type Server struct {
	host   *host.Host
	cfg    bootstrap.Config
	bundle assets.Bundle

	// cocreate 会话
	cocreateMu      sync.Mutex
	cocreateHistory []host.CoCreateMessage

	clients   map[chan sseMsg]struct{}
	clientsMu sync.Mutex
}

// Run 启动 Web 服务。创建 Host、启动 SSE 广播泵、注册 HTTP 路由并监听。
func Run(cfg bootstrap.Config, bundle assets.Bundle, opts Options) error {
	eng, err := host.New(cfg, bundle)
	if err != nil {
		return err
	}
	eng.AskUser().SetHandler(autoAskUser)
	cleanup := logger.SetupFile(eng.Dir(), "web.log", false)
	defer cleanup()
	defer eng.Close()

	s := &Server{
		host:    eng,
		cfg:     cfg,
		bundle:  bundle,
		clients: make(map[chan sseMsg]struct{}),
	}

	// SSE 广播泵：读 Host 的 Events/Stream/Done 通道，扇出给所有连接的客户端。
	go s.pump()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/start", s.handleStart)
	mux.HandleFunc("/api/resume", s.handleResume)
	mux.HandleFunc("/api/message", s.handleMessage)
	mux.HandleFunc("/api/abort", s.handleAbort)
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/model", s.handleModel)
	mux.HandleFunc("/api/thinking", s.handleThinking)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/provider", s.handleProvider)
	mux.HandleFunc("/api/cocreate", s.handleCoCreate)

	addr := opts.Addr
	if addr == "" {
		addr = ":8080"
	}

	slog.Info("web 服务启动", "module", "web", "addr", addr, "output", eng.Dir())
	fmt.Printf("ainovel-cli web 服务: http://localhost%s\n", addr)

	srv := &http.Server{Addr: addr, Handler: mux}
	return srv.ListenAndServe()
}

// ── SSE 广播泵 ──

func (s *Server) pump() {
	for {
		select {
		case ev, ok := <-s.host.Events():
			if !ok {
				s.broadcast(sseMsg{Type: "closed"})
				return
			}
			s.broadcast(sseMsg{Type: "event", Data: ev})
		case delta, ok := <-s.host.Stream():
			if !ok {
				continue
			}
			if delta == host.StreamClearSentinel {
				s.broadcast(sseMsg{Type: "stream_clear"})
			} else if delta != "" {
				s.broadcast(sseMsg{Type: "stream", Data: delta})
			}
		case _, ok := <-s.host.Done():
			if !ok {
				s.broadcast(sseMsg{Type: "closed"})
				return
			}
			s.drainPending()
			s.broadcast(sseMsg{Type: "done"})
		}
	}
}

// drainPending 在 Done() 触发后排空 Events/Stream 中的残留消息。
func (s *Server) drainPending() {
	for {
		select {
		case ev, ok := <-s.host.Events():
			if ok {
				s.broadcast(sseMsg{Type: "event", Data: ev})
			}
		case delta, ok := <-s.host.Stream():
			if !ok {
				continue
			}
			if delta == host.StreamClearSentinel {
				s.broadcast(sseMsg{Type: "stream_clear"})
			} else if delta != "" {
				s.broadcast(sseMsg{Type: "stream", Data: delta})
			}
		default:
			return
		}
	}
}

func (s *Server) broadcast(msg sseMsg) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- msg:
		default:
			// 客户端缓冲已满，丢弃——慢消费者不应阻塞广播。
		}
	}
}

func (s *Server) register() chan sseMsg {
	ch := make(chan sseMsg, 64)
	s.clientsMu.Lock()
	s.clients[ch] = struct{}{}
	s.clientsMu.Unlock()
	return ch
}

func (s *Server) unregister(ch chan sseMsg) {
	s.clientsMu.Lock()
	delete(s.clients, ch)
	s.clientsMu.Unlock()
	close(ch)
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

type startRequest struct {
	Prompt string `json:"prompt"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}

	plan, err := startup.PrepareQuick(startup.Request{
		Mode:       startup.ModeQuick,
		UserPrompt: req.Prompt,
		OutputDir:  s.host.Dir(),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.host.StartPrepared(plan.StartPrompt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 恢复前先把 runtime queue 里的历史事件回放给客户端（与 headless 一致）。
	items, err := s.host.ReplayQueue(0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, item := range items {
		switch item.Kind {
		case domain.RuntimeQueueUIEvent:
			s.broadcast(sseMsg{Type: "event", Data: map[string]any{
				"time":     item.Time,
				"category": item.Category,
				"summary":  item.Summary,
			}})
		case domain.RuntimeQueueStreamClear:
			s.broadcast(sseMsg{Type: "stream_clear"})
		case domain.RuntimeQueueStreamDelta:
			text := host.ReplayDeltaText(item)
			if text != "" {
				s.broadcast(sseMsg{Type: "stream", Data: text})
			}
		}
	}

	label, err := s.host.Resume()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if label == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "nothing_to_resume"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed", "label": label})
}

type messageRequest struct {
	Text string `json:"text"`
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req messageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}
	if err := s.host.Continue(req.Text); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.host.Abort() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
	} else {
		writeJSON(w, http.StatusOK, map[string]string{"status": "not_running"})
	}
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.host.Snapshot()
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Nginx 透传

	ch := s.register()
	defer s.unregister(ch)

	// 连接建立后立即推一份快照，让客户端有初始状态。
	snap := s.host.Snapshot()
	writeSSE(w, sseMsg{Type: "snapshot", Data: snap})
	flusher.Flush()

	// 心跳，防止代理超时断连。
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// ── 配置管理 ──

// roleInfo 描述单个角色的当前模型配置。
type roleInfo struct {
	Role     string   `json:"role"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Thinking string   `json:"thinking"`
}

// configResponse 是 /api/config 的返回结构。
type configResponse struct {
	Providers []string                  `json:"providers"`
	Styles    []string                  `json:"styles"`
	Roles     []roleInfo                `json:"roles"`
	Models    map[string][]string       `json:"models"` // provider → 可选模型列表
}

var allRoles = []string{"default", "coordinator", "architect", "writer", "editor"}
var allStyles = []string{"default", "suspense", "fantasy", "romance"}

// handleConfig 返回当前配置概览：providers、styles、各角色当前模型/思考强度。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	providers := s.host.ConfiguredProviders()
	models := make(map[string][]string, len(providers))
	for _, p := range providers {
		models[p] = s.host.ConfiguredModels(p)
	}

	roles := make([]roleInfo, 0, len(allRoles))
	for _, role := range allRoles {
		p, m, _ := s.host.CurrentModelSelection(role)
		roles = append(roles, roleInfo{
			Role:     role,
			Provider: p,
			Model:    m,
			Thinking: s.host.CurrentThinking(role),
		})
	}

	writeJSON(w, http.StatusOK, configResponse{
		Providers: providers,
		Styles:    allStyles,
		Roles:     roles,
		Models:    models,
	})
}

// modelRequest 切换角色模型。
type modelRequest struct {
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req modelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Provider == "" || req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and model are required"})
		return
	}
	role := req.Role
	if role == "" {
		role = "default"
	}
	if err := s.host.SwitchModel(role, req.Provider, req.Model); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// thinkingRequest 设置角色思考强度。
type thinkingRequest struct {
	Role  string `json:"role"`
	Level string `json:"level"` // off/minimal/low/medium/high/xhigh 或空=继承
}

func (s *Server) handleThinking(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req thinkingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	role := req.Role
	if role == "" {
		role = "default"
	}
	if err := s.host.SetRoleThinking(role, req.Level); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// exportRequest 导出已完成章节。
type exportRequest struct {
	Format    string `json:"format"`     // txt / epub，空则按 outpath 推断
	OutPath   string `json:"out_path"`   // 空=默认路径
	From      int    `json:"from"`       // 0=从第1章
	To        int    `json:"to"`         // 0=到最后一章
	Overwrite bool   `json:"overwrite"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req exportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	opts := exp.Options{
		Format:    exp.Format(req.Format),
		OutPath:   req.OutPath,
		From:      req.From,
		To:        req.To,
		Overwrite: req.Overwrite,
	}
	result, err := s.host.Export(r.Context(), opts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":     result.Path,
		"chapters": result.Chapters,
		"bytes":    result.Bytes,
		"skipped":  result.Skipped,
	})
}
// providerRequest 添加/更新/删除 provider。
type providerRequest struct {
	Action string   `json:"action"`           // save / delete
	Name   string   `json:"name"`             // provider 名称
	Type   string   `json:"type"`             // openai / anthropic / gemini，空=按 name 推断
	APIKey string   `json:"api_key"`
	BaseURL string  `json:"base_url"`
	Models []string `json:"models"`
}

func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req providerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	switch req.Action {
	case "delete":
		if err := s.host.DeleteProvider(req.Name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	case "save", "":
		pc := bootstrap.ProviderConfig{
			Type:    req.Type,
			APIKey:  req.APIKey,
			BaseURL: req.BaseURL,
			Models:  req.Models,
		}
		if err := s.host.SaveProvider(req.Name, pc); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action: " + req.Action})
	}
}
// handleCoCreate 处理共创规划的多轮对话，SSE 流式返回思考/回复/完成。
func (s *Server) handleCoCreate(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	var req struct {
		Message string `json:"message"`
		Reset   bool   `json:"reset"` // 重置对话历史
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	s.cocreateMu.Lock()
	if req.Reset {
		s.cocreateHistory = nil
	}
	if req.Message != "" {
		s.cocreateHistory = append(s.cocreateHistory, host.CoCreateMessage{Role: "user", Content: req.Message})
	}
	history := append([]host.CoCreateMessage(nil), s.cocreateHistory...)
	s.cocreateMu.Unlock()

	if len(history) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	reply, err := s.host.CoCreateStream(r.Context(), history, func(kind, text string) {
		msg := sseMsg{Type: kind, Data: text}
		d, _ := json.Marshal(msg)
		fmt.Fprintf(w, "data: %s\n\n", d)
		flusher.Flush()
	})
	if err != nil {
		d, _ := json.Marshal(sseMsg{Type: "error", Data: err.Error()})
		fmt.Fprintf(w, "data: %s\n\n", d)
		flusher.Flush()
		return
	}

	// 保存助手的回复到历史
	s.cocreateMu.Lock()
	s.cocreateHistory = append(s.cocreateHistory, host.CoCreateMessage{Role: "assistant", Content: reply.Raw})
	s.cocreateMu.Unlock()

	done := sseMsg{Type: "cocreate_done", Data: map[string]any{
		"message":     reply.Message,
		"prompt":      reply.Prompt,
		"ready":       reply.Ready,
		"suggestions": reply.Suggestions,
	}}
	d, _ := json.Marshal(done)
	fmt.Fprintf(w, "data: %s\n\n", d)
	flusher.Flush()
}
// ── helpers ──

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, msg sseMsg) {
	data, _ := json.Marshal(msg)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

// autoAskUser 在 MVP 阶段自动回复每个问题的第一个选项，不阻塞创作流程。
// 用户可通过 /api/message 事后干预修正方向。
func autoAskUser(ctx context.Context, questions []tools.Question) (*tools.AskUserResponse, error) {
	resp := &tools.AskUserResponse{
		Answers: make(map[string]string),
		Notes:   make(map[string]string),
	}
	for _, q := range questions {
		if len(q.Options) > 0 {
			resp.Answers[q.Question] = q.Options[0].Label
		}
	}
	slog.Info("ask_user 自动回复", "module", "web", "questions", len(questions))
	return resp, nil
}
