package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	channel "github.com/larksuite/channel-sdk-go"
	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/config"
	"github.com/zhangwei/codex-feishu-sync/internal/events"
	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/hooks"
	"github.com/zhangwei/codex-feishu-sync/internal/router"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

var errThreadNotIndexed = errors.New("Codex thread is not indexed yet")

type codexClient interface {
	ListThreads(context.Context) ([]appserver.Thread, error)
	ReadThread(context.Context, string) (appserver.Thread, error)
	ResumeThread(context.Context, string) (appserver.Thread, error)
	StartTurn(context.Context, string, string) (string, error)
	InterruptTurn(context.Context, string, string) error
	Close() error
}

type feishuClient interface {
	Start(context.Context) error
	Stop(context.Context) error
	SendText(context.Context, string, string) error
	SendCard(context.Context, string, string) error
	StartMarkdownStream(context.Context, string, string, string) (channel.StreamController, error)
	CreateThreadChat(context.Context, string, string, string) (string, error)
}

type question struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	IsSecret bool   `json:"isSecret"`
}

type pendingApproval struct {
	chatID string
	result chan string
}

type pendingQuestion struct {
	chatID    string
	questions []question
	result    chan string
}

type Bridge struct {
	cfg         config.Config
	credentials config.Credentials
	configDir   string
	codexBin    string
	store       *state.Store
	feishu      feishuClient
	codex       codexClient
	ctx         context.Context
	cancel      context.CancelFunc
	router      *router.Router

	mu               sync.Mutex
	threadLocks      map[string]*sync.Mutex
	threads          map[string]appserver.Thread
	busy             map[string]bool
	turnIDs          map[string]string
	observed         map[string]bool
	lastTakeover     map[string]time.Time
	assistantText    map[string]*strings.Builder
	assistantFinal   map[string]string
	streams          map[string]channel.StreamController
	streamErrors     map[string]bool
	echoSuppress     map[string][]string
	pendingApprovals map[string]pendingApproval
	pendingByChat    map[string]string
	pendingQuestions map[string]*pendingQuestion
}

func New(cfg config.Config, credentials config.Credentials, codexBin string) (*Bridge, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.OwnerOpenID == "" {
		return nil, errors.New("请先配置飞书 owner 的 Open ID")
	}
	configDir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(filepath.Join(configDir, "state"))
	if err != nil {
		return nil, err
	}
	chat, err := feishu.New(cfg, credentials)
	if err != nil {
		store.Close()
		return nil, err
	}
	b := &Bridge{
		cfg: cfg, credentials: credentials, configDir: configDir, codexBin: codexBin,
		store: store, feishu: chat,
		threadLocks: make(map[string]*sync.Mutex), threads: make(map[string]appserver.Thread),
		busy: make(map[string]bool), turnIDs: make(map[string]string),
		observed: make(map[string]bool), lastTakeover: make(map[string]time.Time),
		assistantText: make(map[string]*strings.Builder), assistantFinal: make(map[string]string),
		streams:      make(map[string]channel.StreamController),
		streamErrors: make(map[string]bool),
		echoSuppress: make(map[string][]string), pendingApprovals: make(map[string]pendingApproval),
		pendingByChat: make(map[string]string), pendingQuestions: make(map[string]*pendingQuestion),
	}
	b.router = router.New(store, cfg.OwnerOpenID)
	chat.OnMessage(b.onFeishuMessage)
	chat.OnCardAction(b.onCardAction)
	return b, nil
}

func (b *Bridge) Run(ctx context.Context) error {
	b.ctx, b.cancel = context.WithCancel(ctx)
	defer b.cancel()
	defer b.store.Close()
	client, err := appserver.Start(b.ctx, b.codexBin, b.onNotification, b.onServerRequest)
	if err != nil {
		return err
	}
	b.codex = client
	defer client.Close()
	if err := b.recoverThreads(b.ctx); err != nil {
		return err
	}
	chatDone := make(chan error, 1)
	go func() { chatDone <- b.feishu.Start(b.ctx) }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = b.feishu.Stop(stopCtx)
			return nil
		case err := <-chatDone:
			if b.ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return fmt.Errorf("飞书长连接退出: %w", err)
			}
			return errors.New("飞书长连接意外退出")
		case <-ticker.C:
			if err := hooks.Drain(b.configDir, b.registerSession); err != nil && !errors.Is(err, errThreadNotIndexed) {
				slog.Error("处理 Codex 会话登记失败", "error", err)
			}
			b.pollObserved(b.ctx)
			b.flushIdleQueues(b.ctx)
		}
	}
}

func (b *Bridge) Close(ctx context.Context) error {
	if b.cancel != nil {
		b.cancel()
	}
	if b.feishu != nil {
		_ = b.feishu.Stop(ctx)
	}
	if b.codex != nil {
		_ = b.codex.Close()
	}
	return b.store.Close()
}

func (b *Bridge) recoverThreads(ctx context.Context) error {
	threads, err := b.codex.ListThreads(ctx)
	if err != nil {
		return err
	}
	for _, thread := range threads {
		b.rememberThread(thread)
	}
	// Pending first registrations must catch up the latest completed turn
	// before ordinary recovery establishes a history baseline.
	if err := hooks.Drain(b.configDir, b.registerSession); err != nil && !errors.Is(err, errThreadNotIndexed) {
		return err
	}
	bindings := b.store.Bindings()
	bound := make(map[string]bool, len(bindings))
	for threadID := range bindings {
		bound[threadID] = true
		if _, ok := b.findThread(threads, threadID); !ok {
			continue
		}
		if err := b.resume(ctx, threadID, false); err != nil {
			slog.Warn("恢复已绑定 Codex 会话失败", "thread_id", threadID, "error", err)
		}
	}
	for _, thread := range threads {
		if bound[thread.ID] || !thread.IsBusy() || !supportedSource(thread.SourceKind()) {
			continue
		}
		if !b.cfg.AutoCreateGroup {
			continue
		}
		if err := b.ensureThread(ctx, thread); err != nil {
			slog.Warn("自动绑定运行中的 Codex 会话失败", "thread_id", thread.ID, "error", err)
		}
	}
	if err := hooks.Drain(b.configDir, b.registerSession); err != nil && !errors.Is(err, errThreadNotIndexed) {
		return err
	}
	return nil
}

func (b *Bridge) registerSession(registration hooks.Registration) error {
	threads, err := b.codex.ListThreads(b.ctx)
	if err != nil {
		return err
	}
	thread, ok := appserver.MatchRegistration(threads, registration)
	if !ok {
		return errThreadNotIndexed
	}
	b.rememberThread(thread)
	if _, bound := b.store.ChatForThread(thread.ID); !bound {
		if !b.cfg.AutoCreateGroup {
			return nil
		}
		if err := b.createBinding(b.ctx, thread); err != nil {
			return err
		}
	}
	return b.resume(b.ctx, thread.ID, true)
}

func (b *Bridge) ensureThread(ctx context.Context, thread appserver.Thread) error {
	if _, bound := b.store.ChatForThread(thread.ID); !bound {
		if err := b.createBinding(ctx, thread); err != nil {
			return err
		}
	}
	return b.resume(ctx, thread.ID, true)
}

func (b *Bridge) createBinding(ctx context.Context, thread appserver.Thread) error {
	if !b.cfg.AutoCreateGroup {
		return nil
	}
	name := thread.Name
	if name == "" {
		name = filepath.Base(thread.CWD)
	}
	chatID, err := b.feishu.CreateThreadChat(ctx, thread.ID, name, b.cfg.OwnerOpenID)
	if err != nil {
		return err
	}
	if err := b.store.Bind(thread.ID, chatID); err != nil {
		return err
	}
	intro := fmt.Sprintf("Codex 会话已绑定\n项目：%s\nThread：%s", name, thread.ID)
	return b.feishu.SendText(ctx, chatID, intro)
}

func (b *Bridge) resume(ctx context.Context, threadID string, catchLatest bool) error {
	threadLock := b.threadLock(threadID)
	threadLock.Lock()
	defer threadLock.Unlock()
	var thread appserver.Thread
	var err error
	if b.isObserved(threadID) {
		thread, err = b.codex.ReadThread(ctx, threadID)
	} else {
		thread, err = b.codex.ResumeThread(ctx, threadID)
		if isActiveWriterError(err) {
			thread, err = b.codex.ReadThread(ctx, threadID)
			if err == nil {
				b.setObserved(threadID, true)
				slog.Info("CLI 持有会话写入权限，改用只读轮询同步", "thread_id", threadID)
			}
		}
	}
	if err != nil {
		return err
	}
	b.rememberThread(thread)
	b.syncHistoryOnResume(ctx, thread, catchLatest)
	return nil
}

func (b *Bridge) rememberThread(thread appserver.Thread) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.threads[thread.ID] = thread
	b.busy[thread.ID] = thread.IsBusy()
	if turnID := thread.ActiveTurnID(); turnID != "" {
		b.turnIDs[thread.ID] = turnID
	} else {
		delete(b.turnIDs, thread.ID)
	}
}

func (b *Bridge) findThread(threads []appserver.Thread, threadID string) (appserver.Thread, bool) {
	for _, thread := range threads {
		if thread.ID == threadID {
			return thread, true
		}
	}
	return appserver.Thread{}, false
}

func supportedSource(source string) bool {
	return source == "cli" || source == "vscode"
}

func (b *Bridge) onFeishuMessage(ctx context.Context, message feishu.Inbound) error {
	if message.SenderID != b.cfg.OwnerOpenID {
		return nil
	}
	if handled, err := b.handleApprovalText(ctx, message); handled || err != nil {
		return err
	}
	if answer := b.pendingQuestionForChat(message.ChatID); answer != nil {
		accepted, err := b.store.MarkEvent(message.EventID)
		if err != nil || !accepted {
			return err
		}
		select {
		case answer.result <- message.Text:
			return nil
		default:
			return nil
		}
	}
	b.mu.Lock()
	threadID, bound := b.store.ThreadForChat(message.ChatID)
	b.mu.Unlock()
	if !bound {
		return nil
	}
	threadLock := b.threadLock(threadID)
	threadLock.Lock()
	defer threadLock.Unlock()
	b.mu.Lock()
	busy := b.busy[threadID]
	b.mu.Unlock()
	action, err := b.router.Route(router.Incoming{
		EventID: message.EventID, ChatID: message.ChatID, SenderID: message.SenderID, Text: message.Text,
	}, busy)
	if err != nil {
		return err
	}
	switch action.Kind {
	case router.Submit:
		if err := b.submit(ctx, action.ThreadID, action.Content); err != nil {
			queueErr := b.store.PrependQueue(action.ThreadID, []state.QueuedMessage{{EventID: message.EventID, Text: action.Content}})
			if queueErr == nil {
				if isActiveWriterError(err) {
					return b.feishu.SendText(ctx, message.ChatID, "此会话由本机 Codex CLI 控制，回复仍会同步。指令已排队，退出该 CLI 会话释放写入权限后会提交。")
				}
				return b.feishu.SendText(ctx, message.ChatID, "Codex 暂时无法接收消息，已保留在本地队列中。")
			}
			return errors.Join(err, queueErr)
		}
		return nil
	case router.Queued:
		if b.isObserved(action.ThreadID) {
			return b.feishu.SendText(ctx, message.ChatID, "消息已排队，Codex 回复仍会同步。退出本机 CLI 会话释放写入权限后会提交。")
		}
		return b.feishu.SendText(ctx, message.ChatID, "消息已排队，会在当前轮次完成后提交。")
	case router.Interrupt:
		if b.isObserved(action.ThreadID) {
			if !busy {
				if err := b.store.PrependQueue(action.ThreadID, []state.QueuedMessage{{EventID: message.EventID, Text: action.Content, Interrupt: true}}); err != nil {
					return err
				}
			}
			return b.feishu.SendText(ctx, message.ChatID, "此会话由本机 Codex CLI 控制，无法从飞书立即中断；指令已优先排队，退出该 CLI 会话释放写入权限后会提交。")
		}
		if !busy {
			return b.submit(ctx, action.ThreadID, action.Content)
		}
		b.mu.Lock()
		turnID := b.turnIDs[action.ThreadID]
		b.mu.Unlock()
		if turnID == "" {
			return b.feishu.SendText(ctx, message.ChatID, "当前轮次编号不可用，无法立即中断；指令已优先排队，会在当前轮次结束后提交。")
		}
		if err := b.codex.InterruptTurn(ctx, action.ThreadID, turnID); err != nil {
			return errors.Join(err, b.feishu.SendText(ctx, message.ChatID, "中断请求未能送达，指令仍已优先排队，会在当前轮次结束后提交。"))
		}
		return b.feishu.SendText(ctx, message.ChatID, "已请求中断当前轮次，新指令已优先排队。")
	default:
		return nil
	}
}

func (b *Bridge) submit(ctx context.Context, threadID, text string) error {
	if b.isObserved(threadID) {
		if err := b.takeOverObserved(ctx, threadID); err != nil {
			return err
		}
	}
	b.suppressEcho(threadID, text)
	b.mu.Lock()
	previousBusy := b.busy[threadID]
	previousTurnID := b.turnIDs[threadID]
	b.busy[threadID] = true
	b.mu.Unlock()
	turnID, err := b.codex.StartTurn(ctx, threadID, text)
	if err != nil {
		b.unsuppressEcho(threadID, text)
		b.mu.Lock()
		b.busy[threadID] = previousBusy
		if previousTurnID == "" {
			delete(b.turnIDs, threadID)
		} else {
			b.turnIDs[threadID] = previousTurnID
		}
		b.mu.Unlock()
		return err
	}
	b.mu.Lock()
	if b.busy[threadID] && turnID != "" {
		b.turnIDs[threadID] = turnID
	}
	b.mu.Unlock()
	return nil
}

func (b *Bridge) onNotification(method string, params json.RawMessage) {
	var identity struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &identity)
	if identity.ThreadID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 20*time.Second)
	defer cancel()
	switch method {
	case "turn/started":
		var notification struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &notification)
		b.mu.Lock()
		b.busy[identity.ThreadID] = true
		b.turnIDs[identity.ThreadID] = notification.Turn.ID
		b.mu.Unlock()
		b.sendStatus(ctx, identity.ThreadID, "新一轮任务已开始。")
	case "item/started":
		b.handleItem(ctx, identity.ThreadID, params, false)
	case "item/completed":
		b.handleItem(ctx, identity.ThreadID, params, true)
	case "item/agentMessage/delta":
		var delta struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(params, &delta)
		b.handleAssistantDelta(ctx, identity.ThreadID, delta.Delta)
	case "turn/completed":
		b.finishTurn(ctx, identity.ThreadID, params)
	case "turn/plan/updated":
		b.handlePlanUpdated(ctx, identity.ThreadID, params)
	case "thread/status/changed":
		b.handleStatusChanged(ctx, identity.ThreadID, params)
	}
}

func (b *Bridge) handleItem(ctx context.Context, threadID string, params json.RawMessage, completed bool) {
	var notification struct {
		Item map[string]any `json:"item"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.Item == nil {
		return
	}
	if _, bound := b.store.ChatForThread(threadID); !bound {
		return
	}
	threadLock := b.threadLock(threadID)
	threadLock.Lock()
	defer threadLock.Unlock()
	kind, _ := notification.Item["type"].(string)
	var event events.Event
	switch kind {
	case "userMessage":
		if codexItemMarked(b.store, threadID, notification.Item) {
			return
		}
		text := itemText(notification.Item)
		if text == "" {
			return
		}
		if b.consumeEcho(threadID, text) {
			b.markCodexItem(threadID, notification.Item)
			return
		}
		event = events.Event{Kind: events.UserMessage, Text: text}
	case "agentMessage":
		if completed {
			if text := itemText(notification.Item); text != "" {
				b.mu.Lock()
				b.assistantFinal[threadID] = text
				b.mu.Unlock()
			}
		}
		return
	case "reasoning", "reasoningSummary":
		return
	default:
		if !completed {
			return
		}
		if codexItemMarked(b.store, threadID, notification.Item) {
			return
		}
		var ok bool
		event, ok = itemEvent(notification.Item, b.cfg.SyncLevel)
		if !ok {
			b.markCodexItem(threadID, notification.Item)
			return
		}
	}
	if err := b.sendEvent(ctx, threadID, event); err == nil {
		b.markCodexItem(threadID, notification.Item)
	}
}

func (b *Bridge) handleAssistantDelta(ctx context.Context, threadID, delta string) {
	if delta == "" {
		return
	}
	chatID, ok := b.store.ChatForThread(threadID)
	if !ok {
		return
	}
	b.mu.Lock()
	if b.assistantText[threadID] == nil {
		b.assistantText[threadID] = &strings.Builder{}
	}
	b.assistantText[threadID].WriteString(delta)
	stream := b.streams[threadID]
	startStream := b.cfg.SendTiming == config.Streaming && stream == nil
	b.mu.Unlock()
	if startStream {
		initial := "Codex："
		created, err := b.feishu.StartMarkdownStream(ctx, chatID, "Codex", initial)
		if err == nil {
			stream = created
			b.mu.Lock()
			b.streams[threadID] = created
			b.mu.Unlock()
		}
	}
	if b.cfg.SendTiming == config.Streaming && stream != nil {
		if err := stream.Append(ctx, delta); err != nil {
			slog.Warn("更新飞书流式回复失败", "thread_id", threadID, "error", err)
			b.mu.Lock()
			b.streamErrors[threadID] = true
			b.mu.Unlock()
		}
	}
}

func (b *Bridge) finishTurn(ctx context.Context, threadID string, params json.RawMessage) {
	threadLock := b.threadLock(threadID)
	threadLock.Lock()
	defer threadLock.Unlock()
	var notification struct {
		Turn struct {
			Status json.RawMessage  `json:"status"`
			Error  json.RawMessage  `json:"error"`
			Items  []map[string]any `json:"items"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(params, &notification)
	status := statusValue(notification.Turn.Status)
	message := events.TurnStatus(status)
	if detail := itemTextFromRaw(notification.Turn.Error); detail != "" {
		message = "任务失败：" + truncate(detail, 1000)
	}
	var deltaText, completedText string
	b.mu.Lock()
	if builder := b.assistantText[threadID]; builder != nil {
		deltaText = builder.String()
	}
	delete(b.assistantText, threadID)
	completedText = b.assistantFinal[threadID]
	delete(b.assistantFinal, threadID)
	streamFailed := b.streamErrors[threadID]
	delete(b.streamErrors, threadID)
	delete(b.echoSuppress, threadID)
	stream := b.streams[threadID]
	delete(b.streams, threadID)
	b.busy[threadID] = false
	delete(b.turnIDs, threadID)
	b.mu.Unlock()
	if stream != nil {
		if err := stream.Close(ctx); err != nil {
			streamFailed = true
			slog.Warn("关闭飞书流式回复失败", "thread_id", threadID, "error", err)
		}
	}
	hasAssistant, assistantFullyDelivered := b.syncTurnItems(ctx, threadID, notification.Turn.Items, stream != nil && !streamFailed)
	assistant := assistantOutput(deltaText, completedText)
	_, bound := b.store.ChatForThread(threadID)
	streamDelivered := b.cfg.SendTiming == config.Streaming && stream != nil && !streamFailed
	if shouldFallbackAssistant(assistant != "", bound, streamDelivered, hasAssistant, assistantFullyDelivered) {
		b.sendEvent(ctx, threadID, events.Event{Kind: events.AssistantMessage, Text: assistant})
	}
	b.sendStatus(ctx, threadID, message)
	b.drainQueueLocked(ctx, threadID)
}

func shouldFallbackAssistant(hasOutput, bound, streamDelivered, hasAssistant, assistantFullyDelivered bool) bool {
	return hasOutput && bound && !streamDelivered && (!hasAssistant || !assistantFullyDelivered)
}

func (b *Bridge) handleStatusChanged(ctx context.Context, threadID string, params json.RawMessage) {
	var notification struct {
		Status json.RawMessage `json:"status"`
	}
	if json.Unmarshal(params, &notification) != nil {
		return
	}
	status := statusValue(notification.Status)
	b.mu.Lock()
	if status == "active" {
		b.busy[threadID] = true
	}
	b.mu.Unlock()
}

func (b *Bridge) drainQueue(ctx context.Context, threadID string) {
	threadLock := b.threadLock(threadID)
	threadLock.Lock()
	defer threadLock.Unlock()
	b.drainQueueLocked(ctx, threadID)
}

func (b *Bridge) drainQueueLocked(ctx context.Context, threadID string) {
	queued, err := b.store.Dequeue(threadID)
	if err != nil || len(queued) == 0 {
		return
	}
	first := queued[0]
	if err := b.submit(ctx, threadID, first.Text); err != nil {
		_ = b.store.PrependQueue(threadID, queued)
		slog.Warn("提交排队消息失败", "thread_id", threadID, "error", err)
		return
	}
	if first.Interrupt {
		if err := b.feishu.SendText(ctx, b.chatFor(threadID), "中断指令已提交给 Codex。"); err != nil {
			slog.Warn("发送队列状态失败", "thread_id", threadID, "error", err)
		}
	}
	if len(queued) > 1 {
		if err := b.store.PrependQueue(threadID, queued[1:]); err != nil {
			slog.Error("恢复未提交消息失败", "thread_id", threadID, "error", err)
		}
	}
}

func (b *Bridge) flushIdleQueues(ctx context.Context) {
	for _, threadID := range b.store.QueuedThreads() {
		b.mu.Lock()
		busy := b.busy[threadID]
		_, known := b.threads[threadID]
		b.mu.Unlock()
		if known && !busy {
			if b.isObserved(threadID) {
				continue
			}
			b.drainQueue(ctx, threadID)
		}
	}
}

func (b *Bridge) threadLock(threadID string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	lock := b.threadLocks[threadID]
	if lock == nil {
		lock = &sync.Mutex{}
		b.threadLocks[threadID] = lock
	}
	return lock
}

func (b *Bridge) sendEvent(ctx context.Context, threadID string, event events.Event) error {
	text := events.Format(b.cfg.SyncLevel, event)
	if text == "" {
		return nil
	}
	if len([]rune(text)) > 30000 {
		text = truncate(text, 30000) + "\n[内容已截断]"
	}
	err := sendWithRetry(ctx, func() error {
		return b.feishu.SendText(ctx, b.chatFor(threadID), text)
	})
	if err != nil {
		slog.Warn("发送飞书消息失败", "thread_id", threadID, "error", err)
		return err
	}
	return nil
}

func sendWithRetry(ctx context.Context, send func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = send(); err == nil {
			return nil
		}
		if attempt == 2 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func (b *Bridge) sendStatus(ctx context.Context, threadID, text string) {
	b.sendEvent(ctx, threadID, events.Event{Kind: events.Status, Text: text})
}

func (b *Bridge) chatFor(threadID string) string {
	chatID, _ := b.store.ChatForThread(threadID)
	return chatID
}

func (b *Bridge) suppressEcho(threadID, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.echoSuppress[threadID] = append(b.echoSuppress[threadID], strings.TrimSpace(text))
}

func (b *Bridge) unsuppressEcho(threadID, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.echoSuppress[threadID]
	for index, item := range items {
		if item == strings.TrimSpace(text) {
			b.echoSuppress[threadID] = append(items[:index], items[index+1:]...)
			return
		}
	}
}

func (b *Bridge) consumeEcho(threadID, text string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.echoSuppress[threadID]
	for index, item := range items {
		if item == strings.TrimSpace(text) {
			b.echoSuppress[threadID] = append(items[:index], items[index+1:]...)
			return true
		}
	}
	return false
}

func itemText(item map[string]any) string {
	for _, key := range []string{"text", "output", "aggregatedOutput", "result", "summary", "command"} {
		if value, ok := item[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	if content, ok := item["content"]; ok {
		return flattenText(content)
	}
	return ""
}

func itemTextFromRaw(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return flattenText(value)
}

func flattenText(value any) string {
	switch item := value.(type) {
	case string:
		return item
	case []any:
		parts := make([]string, 0, len(item))
		for _, entry := range item {
			if text := flattenText(entry); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"text", "output", "aggregatedOutput", "result", "content", "summary", "command"} {
			if entry, exists := item[key]; exists {
				if text := flattenText(entry); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func statusValue(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return "unknown"
	}
	for _, key := range []string{"type", "status", "kind"} {
		if json.Unmarshal(object[key], &value) == nil && value != "" {
			return value
		}
	}
	return "unknown"
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func assistantOutput(deltaText, completedText string) string {
	if text := strings.TrimSpace(deltaText); text != "" {
		return text
	}
	return strings.TrimSpace(completedText)
}
