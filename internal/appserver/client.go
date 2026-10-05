package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type NotificationHandler func(method string, params json.RawMessage)
type ServerRequestHandler func(context.Context, json.RawMessage, string, json.RawMessage) (any, error)

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type Client struct {
	ctx            context.Context
	cancel         context.CancelFunc
	command        *exec.Cmd
	stdin          io.WriteCloser
	writeMu        sync.Mutex
	pendingMu      sync.Mutex
	pending        map[string]chan rpcResult
	sequence       atomic.Uint64
	notifications  chan rpcMessage
	notificationFn NotificationHandler
	requestFn      ServerRequestHandler
	closeOnce      sync.Once
	done           chan struct{}
}

func Start(ctx context.Context, executable string, notificationFn NotificationHandler, requestFn ServerRequestHandler) (*Client, error) {
	if executable == "" {
		executable = "codex"
	}
	processCtx, cancel := context.WithCancel(ctx)
	cmd := appServerCommand(processCtx, executable)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}
	client := &Client{
		ctx: processCtx, cancel: cancel, command: cmd, stdin: stdin,
		pending: make(map[string]chan rpcResult), notifications: make(chan rpcMessage, 1024),
		notificationFn: notificationFn, requestFn: requestFn, done: make(chan struct{}),
	}
	go client.readLoop(stdout)
	go client.notificationLoop()
	initCtx, initCancel := context.WithTimeout(ctx, 20*time.Second)
	defer initCancel()
	_, err = client.Call(initCtx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name": "codex-feishu-sync", "title": "Codex Feishu Sync", "version": "0.1.0",
		},
		"capabilities": map[string]bool{"experimentalApi": true},
	})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize codex app-server: %w", err)
	}
	if err := client.Notify("initialized", map[string]any{}); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func appServerCommand(ctx context.Context, executable string) *exec.Cmd {
	if runtime.GOOS == "windows" && (strings.HasSuffix(strings.ToLower(executable), ".cmd") || strings.HasSuffix(strings.ToLower(executable), ".bat")) {
		line := `"` + strings.ReplaceAll(executable, `"`, `""`) + `" app-server --listen stdio://`
		return exec.CommandContext(ctx, "cmd.exe", "/D", "/S", "/C", line)
	}
	return exec.CommandContext(ctx, executable, "app-server", "--listen", "stdio://")
}

func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	requestID := c.sequence.Add(1)
	id := strconv.FormatUint(requestID, 10)
	resultCh := make(chan rpcResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = resultCh
	c.pendingMu.Unlock()
	message := map[string]any{"id": requestID, "method": method, "params": params}
	if err := c.write(message); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}
	select {
	case result := <-resultCh:
		return result.result, result.err
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("codex app-server exited")
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *Client) Respond(id json.RawMessage, result any) error {
	return c.write(map[string]any{"id": json.RawMessage(id), "result": result})
}

func (c *Client) RespondError(id json.RawMessage, code int, message string) error {
	return c.write(map[string]any{"id": json.RawMessage(id), "error": rpcError{Code: code, Message: message}})
}

func (c *Client) ListThreads(ctx context.Context) ([]Thread, error) {
	var threads []Thread
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{
			"limit":         100,
			"sortKey":       "updated_at",
			"sortDirection": "desc",
			"archived":      false,
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		response, err := c.Call(ctx, "thread/list", params)
		if err != nil {
			return nil, err
		}
		pageThreads, nextCursor, err := parseThreadListResponse(response)
		if err != nil {
			return nil, err
		}
		threads = append(threads, pageThreads...)
		if nextCursor == "" {
			return threads, nil
		}
		cursor = nextCursor
	}
	return threads, errors.New("thread/list exceeded 100 pages")
}

func parseThreadListResponse(response json.RawMessage) ([]Thread, string, error) {
	var page struct {
		Data            []Thread `json:"data"`
		NextCursor      string   `json:"nextCursor"`
		BackwardsCursor string   `json:"backwardsCursor"`
	}
	if err := json.Unmarshal(response, &page); err != nil {
		return nil, "", fmt.Errorf("decode thread/list response: %w", err)
	}
	return page.Data, page.NextCursor, nil
}

func (c *Client) ResumeThread(ctx context.Context, threadID string) (Thread, error) {
	response, err := c.Call(ctx, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": false})
	if err != nil {
		return Thread{}, err
	}
	var result struct {
		Thread Thread `json:"thread"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return Thread{}, fmt.Errorf("decode thread/resume response: %w", err)
	}
	return result.Thread, nil
}

// ReadThread reads persisted turns without acquiring the thread's writer lock.
func (c *Client) ReadThread(ctx context.Context, threadID string) (Thread, error) {
	response, err := c.Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	if err != nil {
		return Thread{}, err
	}
	var result struct {
		Thread Thread `json:"thread"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return Thread{}, fmt.Errorf("decode thread/read response: %w", err)
	}
	return result.Thread, nil
}

func (c *Client) StartTurn(ctx context.Context, threadID, text string) (string, error) {
	response, err := c.Call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		return "", err
	}
	return responseTurnID(response), nil
}

func (c *Client) SteerTurn(ctx context.Context, threadID, turnID, text string) (string, error) {
	response, err := c.Call(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"expectedTurnId": turnID,
		"input":          []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		return "", err
	}
	return responseTurnID(response), nil
}

func responseTurnID(response json.RawMessage) string {
	var result struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(response, &result) != nil {
		return ""
	}
	return result.Turn.ID
}

func (c *Client) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	_, err := c.Call(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID})
	return err
}

func (c *Client) Close() error {
	var waitErr error
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.stdin.Close()
		if c.command != nil && c.command.Process != nil {
			waitErr = c.command.Wait()
		}
	})
	return waitErr
}

func (c *Client) readLoop(reader io.Reader) {
	defer close(c.done)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var message rpcMessage
		if err := json.Unmarshal(line, &message); err != nil {
			continue
		}
		if message.Method != "" {
			if len(message.ID) != 0 && !bytes.Equal(message.ID, []byte("null")) {
				go c.handleServerRequest(message)
				continue
			}
			select {
			case c.notifications <- message:
			case <-c.ctx.Done():
				return
			}
			continue
		}
		if len(message.ID) == 0 {
			continue
		}
		key := strings.Trim(string(message.ID), `"`)
		c.pendingMu.Lock()
		resultCh := c.pending[key]
		delete(c.pending, key)
		c.pendingMu.Unlock()
		if resultCh == nil {
			continue
		}
		if message.Error != nil {
			resultCh <- rpcResult{err: fmt.Errorf("app-server %s (%d): %s", "request failed", message.Error.Code, message.Error.Message)}
		} else {
			resultCh <- rpcResult{result: message.Result}
		}
	}
	if err := scanner.Err(); err != nil {
		c.pendingMu.Lock()
		for id, resultCh := range c.pending {
			delete(c.pending, id)
			resultCh <- rpcResult{err: fmt.Errorf("read app-server response: %w", err)}
		}
		c.pendingMu.Unlock()
	}
}

func (c *Client) notificationLoop() {
	for {
		select {
		case message := <-c.notifications:
			if c.notificationFn != nil {
				c.notificationFn(message.Method, message.Params)
			}
		case <-c.done:
			return
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) handleServerRequest(message rpcMessage) {
	if c.requestFn == nil {
		_ = c.RespondError(message.ID, -32601, "server request is not supported")
		return
	}
	result, err := c.requestFn(c.ctx, message.ID, message.Method, message.Params)
	if err != nil {
		_ = c.RespondError(message.ID, -32000, err.Error())
		return
	}
	_ = c.Respond(message.ID, result)
}

func (c *Client) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write app-server request: %w", err)
	}
	return nil
}
