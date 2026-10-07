// Package control shares one App Server connection between Feishu and native
// Codex terminal clients. Only the current sender can steer, interrupt or
// answer approvals. Submitting new input explicitly transfers that authority.
package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
)

const Feishu = "feishu"

type Backend interface {
	Call(context.Context, string, any) (json.RawMessage, error)
	Initialization() json.RawMessage
}

type Hooks struct {
	// Call serializes terminal writes with the bridge's per-thread locks.
	Call          func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	Takeover      func(context.Context, string, string) error
	FeishuRequest appserver.ServerRequestHandler
}

type threadState struct {
	op       sync.Mutex
	owner    string
	epoch    uint64
	lease    context.Context
	cancel   context.CancelFunc
	turn     string
	idle     chan struct{}
	revision uint64
}

type packet struct {
	ID     json.RawMessage     `json:"id,omitempty"`
	Method string              `json:"method,omitempty"`
	Params json.RawMessage     `json:"params,omitempty"`
	Result json.RawMessage     `json:"result,omitempty"`
	Error  *appserver.RPCError `json:"error,omitempty"`
}

type answer struct {
	result json.RawMessage
	err    error
}
type pending struct {
	peer   *peer
	result chan answer
}

type peer struct {
	id          string
	conn        *websocket.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	out         chan packet
	initialized bool
	threads     map[string]bool
	optOut      map[string]bool
}

type Server struct {
	backend   Backend
	hooks     Hooks
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	threads   map[string]*threadState
	peers     map[string]*peer
	pending   map[string]pending
	http      *http.Server
	listener  net.Listener
	directory string
	endpoint  Endpoint
}

func New(ctx context.Context, backend Backend, hooks Hooks) *Server {
	ctx, cancel := context.WithCancel(ctx)
	return &Server{backend: backend, hooks: hooks, ctx: ctx, cancel: cancel,
		threads: make(map[string]*threadState), peers: make(map[string]*peer), pending: make(map[string]pending)}
}

func randomID() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func (s *Server) Start(directory string) error {
	token, err := randomID()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.listener, s.directory = listener, directory
	s.endpoint = Endpoint{URL: "ws://" + listener.Addr().String(), Token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.accept)
	s.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := saveEndpoint(directory, s.endpoint); err != nil {
		_ = listener.Close()
		return err
	}
	go func() { _ = s.http.Serve(listener) }()
	return nil
}

func (s *Server) Close() error {
	s.cancel()
	s.mu.Lock()
	for _, p := range s.peers {
		p.cancel()
		_ = p.conn.Close()
	}
	s.mu.Unlock()
	if s.http != nil {
		_ = s.http.Close()
	}
	return removeEndpoint(s.directory, s.endpoint)
}

func (s *Server) Manage(thread appserver.Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threads[thread.ID] != nil {
		return
	}
	idle := make(chan struct{})
	turn := ""
	if thread.IsBusy() {
		turn = thread.ActiveTurnID()
	}
	if turn == "" {
		close(idle)
	}
	lease, cancel := context.WithCancel(s.ctx)
	s.threads[thread.ID] = &threadState{lease: lease, cancel: cancel, idle: idle, turn: turn}
}

func (s *Server) Managed(thread string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threads[thread] != nil
}
func (s *Server) Owner(thread string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.threads[thread]; state != nil {
		return state.owner
	}
	return ""
}
func (s *Server) ActiveTurn(thread string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.threads[thread]; state != nil {
		return state.turn
	}
	return ""
}

func OwnerLabel(owner string) string {
	if owner == Feishu {
		return "飞书"
	}
	return "CLI"
}

func identity(raw json.RawMessage) (thread, turn string) {
	var params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(raw, &params)
	thread, turn = params.ThreadID, params.TurnID
	if thread == "" {
		thread = params.Thread.ID
	}
	if turn == "" {
		turn = params.Turn.ID
	}
	return
}

// Notify is a nonblocking reader tap. Slow terminals are disconnected rather
// than holding up model output or a handoff's completion acknowledgement.
func (s *Server) Notify(method string, raw json.RawMessage) {
	thread, turn := identity(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.threads[thread]; state != nil {
		switch method {
		case "turn/started":
			if state.turn != turn {
				state.cancel()
				state.epoch++
				state.lease, state.cancel = context.WithCancel(s.ctx)
			}
			if state.turn == "" {
				state.idle = make(chan struct{})
			}
			state.turn = turn
			state.revision++
		case "turn/completed":
			if state.turn != "" && (turn == state.turn || turn == "") {
				state.cancel()
				state.epoch++
				state.turn = ""
				close(state.idle)
			}
			state.revision++
		case "thread/closed":
			state.cancel()
			if state.turn != "" {
				state.turn = ""
				close(state.idle)
			}
			delete(s.threads, thread)
		}
	}
	for _, p := range s.peers {
		if p.initialized && !p.optOut[method] && (thread == "" || p.threads[thread]) {
			s.sendLocked(p, packet{Method: method, Params: raw})
		}
	}
}

// Execute is called while holding the bridge thread lock for write operations.
// The operation lock additionally protects uses outside the bridge (tests).
func (s *Server) Execute(ctx context.Context, owner, method string, raw json.RawMessage) (json.RawMessage, error) {
	thread, _ := identity(raw)
	s.mu.Lock()
	state := s.threads[thread]
	s.mu.Unlock()
	if state == nil || thread == "" {
		return s.backend.Call(ctx, method, raw)
	}
	state.op.Lock()
	defer state.op.Unlock()
	takeover := method == "turn/start" || method == "turn/steer" || method == "review/start"
	s.mu.Lock()
	changed := takeover && state.owner != owner
	if !takeover && IsWrite(method) && state.owner != owner {
		s.mu.Unlock()
		return nil, &appserver.RPCError{Code: -32001, Message: "当前端仅同步回复；提交新指令可接管此会话。"}
	}
	oldTurn := state.turn
	if changed {
		state.cancel()
		state.owner = owner
		state.epoch++
		state.lease, state.cancel = context.WithCancel(s.ctx)
	}
	idle := state.idle
	s.mu.Unlock()
	if changed {
		// Revoke queued commands before allowing an interrupt to complete.
		if s.hooks.Takeover != nil {
			if err := s.hooks.Takeover(ctx, thread, owner); err != nil {
				return nil, err
			}
		}
		if oldTurn != "" {
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, err := s.backend.Call(waitCtx, "turn/interrupt", map[string]string{"threadId": thread, "turnId": oldTurn})
			if err != nil {
				return nil, fmt.Errorf("交接中断失败，未提交新指令: %w", err)
			}
			select {
			case <-idle:
			case <-waitCtx.Done():
				return nil, errors.New("旧轮次尚未确认停止，未提交新指令；请稍后重试")
			}
		}
		if method == "turn/steer" {
			// A native TUI submits followups as steer while it sees a running
			// turn. After takeover that turn has ended, so start a new one.
			var params map[string]json.RawMessage
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, err
			}
			delete(params, "expectedTurnId")
			raw, _ = json.Marshal(params)
			result, err := s.start(ctx, state, "turn/start", raw)
			if err != nil {
				return nil, err
			}
			_, turn := identity(result)
			return json.Marshal(map[string]string{"turnId": turn})
		}
	}
	return s.start(ctx, state, method, raw)
}

func (s *Server) start(ctx context.Context, state *threadState, method string, raw json.RawMessage) (json.RawMessage, error) {
	s.mu.Lock()
	revision := state.revision
	s.mu.Unlock()
	result, err := s.backend.Call(ctx, method, raw)
	if err == nil && (method == "turn/start" || method == "turn/steer") {
		_, turn := identity(result)
		s.mu.Lock()
		if state.revision == revision && turn != "" {
			if state.lease.Err() != nil {
				state.epoch++
				state.lease, state.cancel = context.WithCancel(s.ctx)
			}
			if state.turn == "" {
				state.idle = make(chan struct{})
			}
			state.turn = turn
		}
		s.mu.Unlock()
	}
	return result, err
}

func IsWrite(method string) bool {
	if strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "review/") {
		return true
	}
	if !strings.HasPrefix(method, "thread/") {
		return false
	}
	switch method {
	case "thread/read", "thread/items/list", "thread/turns/list", "thread/list", "thread/loaded/list", "thread/resume", "thread/goal/get", "thread/backgroundTerminals/list":
		return false
	default:
		return true
	}
}

// Request routes approvals to the turn's sender and invalidates outstanding
// answers as soon as ownership changes, including late clicks on old cards.
func (s *Server) Request(ctx context.Context, id json.RawMessage, method string, raw json.RawMessage) (any, error) {
	thread, _ := identity(raw)
	s.mu.Lock()
	state := s.threads[thread]
	if state == nil {
		s.mu.Unlock()
		return nil, errors.New("会话未登记")
	}
	owner, epoch, lease := state.owner, state.epoch, state.lease
	p := s.peers[owner]
	s.mu.Unlock()
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lease, cancel)
	defer func() { stop(); cancel() }()
	var result any
	var err error
	if owner == Feishu && s.hooks.FeishuRequest != nil {
		result, err = s.hooks.FeishuRequest(requestCtx, id, method, raw)
	} else if p != nil {
		key := string(id)
		ch := make(chan answer, 1)
		s.mu.Lock()
		s.pending[key] = pending{peer: p, result: ch}
		s.sendLocked(p, packet{ID: id, Method: method, Params: raw})
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.pending, key); s.mu.Unlock() }()
		select {
		case reply := <-ch:
			result, err = reply.result, reply.err
		case <-requestCtx.Done():
			err = requestCtx.Err()
		case <-p.ctx.Done():
			err = errors.New("CLI 已断开，审批未通过")
		}
	} else {
		return nil, errors.New("控制端不可用，审批未通过")
	}
	s.mu.Lock()
	valid := state.epoch == epoch && s.threads[thread] == state && lease.Err() == nil
	s.mu.Unlock()
	if !valid {
		return nil, errors.New("控制权已交接，旧请求已失效")
	}
	if err != nil {
		return nil, err
	}
	return appserver.GuardedResponse(func(send func(any, error) error) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if state.epoch != epoch || s.threads[thread] != state || lease.Err() != nil {
			return send(nil, errors.New("控制权已交接，旧请求已失效"))
		}
		return send(result, nil)
	}), nil
}

func (s *Server) sendLocked(p *peer, value packet) {
	select {
	case p.out <- value:
	default:
		p.cancel()
		_ = p.conn.Close()
	}
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.endpoint.Token)) != 1 {
		slog.Debug("CLI 控制连接认证失败", "valid_path", r.URL.Path == "/", "has_origin", r.Header.Get("Origin") != "", "has_bearer", strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("CLI WebSocket 握手失败", "error", err)
		return
	}
	id, err := randomID()
	if err != nil {
		_ = conn.Close()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	p := &peer{id: "cli-" + id, conn: conn, ctx: ctx, cancel: cancel, out: make(chan packet, 256), threads: make(map[string]bool), optOut: make(map[string]bool)}
	s.mu.Lock()
	s.peers[p.id] = p
	s.mu.Unlock()
	defer func() { cancel(); _ = conn.Close(); s.mu.Lock(); delete(s.peers, p.id); s.mu.Unlock() }()
	go func() {
		defer func() { cancel(); _ = conn.Close() }()
		for {
			select {
			case value := <-p.out:
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if conn.WriteJSON(value) != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	conn.SetReadLimit(32 * 1024 * 1024)
	// Preserve the terminal's request order; replies to server requests must
	// still be read while an earlier client RPC is waiting for completion.
	requests := make(chan packet, 64)
	go func() {
		for {
			select {
			case value := <-requests:
				s.handle(p, value)
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		var value packet
		if conn.ReadJSON(&value) != nil {
			return
		}
		if value.Method == "" {
			s.mu.Lock()
			request, ok := s.pending[string(value.ID)]
			s.mu.Unlock()
			if ok && request.peer == p {
				reply := answer{result: value.Result}
				if value.Error != nil {
					reply.err = value.Error
				}
				select {
				case request.result <- reply:
				default:
				}
			}
			continue
		}
		select {
		case requests <- value:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) handle(p *peer, value packet) {
	if value.Method == "initialized" {
		return
	}
	if len(value.ID) == 0 {
		return
	}
	var result json.RawMessage
	var err error
	if value.Method == "initialize" {
		var params struct {
			Capabilities struct {
				OptOut []string `json:"optOutNotificationMethods"`
			} `json:"capabilities"`
		}
		_ = json.Unmarshal(value.Params, &params)
		s.mu.Lock()
		if p.initialized {
			err = errors.New("already initialized")
		} else {
			p.initialized = true
			for _, method := range params.Capabilities.OptOut {
				p.optOut[method] = true
			}
			result = s.backend.Initialization()
		}
		s.mu.Unlock()
	} else {
		s.mu.Lock()
		initialized := p.initialized
		s.mu.Unlock()
		if !initialized {
			err = errors.New("client must initialize first")
		} else {
			thread, _ := identity(value.Params)
			if value.Method == "thread/unsubscribe" {
				s.mu.Lock()
				delete(p.threads, thread)
				s.mu.Unlock()
				result = json.RawMessage(`{"status":"unsubscribed"}`)
			} else {
				ctx, cancel := context.WithTimeout(p.ctx, 90*time.Second)
				if value.Method == "thread/resume" {
					s.mu.Lock()
					p.threads[thread] = true
					s.mu.Unlock()
				}
				if s.hooks.Call != nil {
					result, err = s.hooks.Call(ctx, p.id, value.Method, value.Params)
				} else {
					result, err = s.Execute(ctx, p.id, value.Method, value.Params)
				}
				cancel()
				if err == nil && (value.Method == "thread/start" || value.Method == "thread/resume" || value.Method == "thread/fork") {
					thread, _ = identity(result)
					s.mu.Lock()
					p.threads[thread] = true
					s.mu.Unlock()
				}
			}
		}
	}
	reply := packet{ID: value.ID, Result: result}
	if err != nil {
		reply.Error = &appserver.RPCError{Code: -32000, Message: err.Error()}
		var rpcErr *appserver.RPCError
		if errors.As(err, &rpcErr) {
			reply.Error = rpcErr
		}
	}
	s.mu.Lock()
	s.sendLocked(p, reply)
	s.mu.Unlock()
}
