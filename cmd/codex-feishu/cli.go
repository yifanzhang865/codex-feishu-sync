package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zhangwei/codex-feishu-sync/internal/config"
	"github.com/zhangwei/codex-feishu-sync/internal/control"
)

func runCLI(args []string) error {
	for _, arg := range args {
		if arg == "--remote" || arg == "--remote-auth-token-env" || strings.HasPrefix(arg, "--remote=") || strings.HasPrefix(arg, "--remote-auth-token-env=") || arg == "--no-daemon" {
			return errors.New("cli 自动使用本机控制入口，不接受 --remote、--remote-auth-token-env 或 --no-daemon")
		}
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	endpoint, err := control.LoadEndpoint(dir)
	if err != nil {
		return err
	}
	executable := cfg.CodexBinary
	if executable == "" {
		executable = "codex"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	helpCommand, err := nativeCommand(executable, "--help")
	if err != nil {
		return err
	}
	helpCommand = exec.CommandContext(ctx, helpCommand.Path, helpCommand.Args[1:]...)
	help, err := helpCommand.Output()
	if err != nil {
		return fmt.Errorf("检查 Codex CLI: %w", err)
	}
	if !strings.Contains(string(help), "--remote") {
		return errors.New("当前 Codex CLI 不支持 --remote，请先升级 Codex CLI（本项目验证版本为 0.159.2）")
	}
	header := http.Header{"Authorization": {"Bearer " + endpoint.Token}}
	conn, response, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).DialContext(ctx, endpoint.URL, header)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return errors.New("本机控制入口不可用，请先启动或重启 codex-feishu run")
	}
	_ = conn.Close()
	fmt.Fprintln(os.Stderr, "CLI/飞书自动交接已启用：另一端提交指令会接管并中断旧轮次；本端继续同步回复，再次提交可接管。")
	command, err := nativeCommand(executable, append([]string{"--remote", endpoint.URL, "--remote-auth-token-env", "CODEX_FEISHU_CONTROL_TOKEN"}, args...)...)
	if err != nil {
		return err
	}
	command.Env = controlEnvironment(os.Environ(), endpoint.Token)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C reaches the native foreground TUI as well. Keep the wrapper alive
	// so native Codex can interrupt a turn without losing the terminal process.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				if sig != os.Interrupt {
					_ = command.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()
	return command.Wait()
}

func controlEnvironment(environment []string, token string) []string {
	var result []string
	var bypass []string
	for _, value := range environment {
		key, content, _ := strings.Cut(value, "=")
		if strings.EqualFold(key, "NO_PROXY") {
			if content != "" {
				bypass = append(bypass, content)
			}
			continue
		}
		if key == "CODEX_FEISHU_CONTROL_TOKEN" {
			continue
		}
		result = append(result, value)
	}
	// Native WebSocket clients can honor HTTP_PROXY/ALL_PROXY. The local
	// controller must never be sent through an external HTTP proxy.
	bypass = append(bypass, "127.0.0.1", "localhost")
	value := strings.Join(bypass, ",")
	return append(result, "NO_PROXY="+value, "no_proxy="+value, "CODEX_FEISHU_CONTROL_TOKEN="+token)
}

func nativeCommand(executable string, args ...string) (*exec.Cmd, error) {
	if runtime.GOOS == "windows" && (strings.HasSuffix(strings.ToLower(executable), ".cmd") || strings.HasSuffix(strings.ToLower(executable), ".bat")) {
		// Use npm's JS entry directly, avoiding cmd.exe interpretation of
		// prompts containing shell metacharacters.
		entry := filepath.Join(filepath.Dir(executable), "node_modules", "@openai", "codex", "bin", "codex.js")
		if _, err := os.Stat(entry); err != nil {
			return nil, errors.New("请把 codex_binary 指向 codex.exe，或使用 npm 安装的 Codex CLI")
		}
		node, err := exec.LookPath("node")
		if err != nil {
			return nil, err
		}
		return exec.Command(node, append([]string{entry}, args...)...), nil
	}
	return exec.Command(executable, args...), nil
}
