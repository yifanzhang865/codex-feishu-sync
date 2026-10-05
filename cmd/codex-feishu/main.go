package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/bridge"
	"github.com/zhangwei/codex-feishu-sync/internal/config"
	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/hooks"
	"github.com/zhangwei/codex-feishu-sync/internal/service"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
	"golang.org/x/term"
)

var version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "setup":
		return setup()
	case "run":
		return runBridge()
	case "hook":
		return runHook()
	case "bind":
		return bind(args[1:])
	case "threads":
		return listThreads()
	case "diagnose":
		return diagnoseAppServer()
	case "status":
		return showStatus()
	case "install-service":
		return installService()
	case "uninstall":
		return uninstall(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

func setup() error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("setup 需要交互式终端，以便安全输入飞书 App Secret")
	}
	reader := bufio.NewReader(os.Stdin)
	cfg := config.Defaults()
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	previousConfig, previousConfigErr := config.Load(configDir)
	previousCredentials, previousCredentialsErr := config.LoadCredentials(configDir)
	if previousConfigErr == nil {
		cfg = previousConfig
	}
	region, err := ask(reader, "服务区域 feishu/lark", cfg.Region)
	if err != nil {
		return err
	}
	cfg.Region = region
	syncLevel, err := ask(reader, "同步范围 conversation_status/with_tools/all_visible", string(cfg.SyncLevel))
	if err != nil {
		return err
	}
	cfg.SyncLevel = config.SyncLevel(syncLevel)
	sendTiming, err := ask(reader, "发送时机 after_turn/streaming", string(cfg.SendTiming))
	if err != nil {
		return err
	}
	cfg.SendTiming = config.SendTiming(sendTiming)
	defaultReadOnly := "no"
	if cfg.ReadOnly {
		defaultReadOnly = "yes"
	}
	readOnly, err := ask(reader, "仅同步回复，不接受飞书指令 yes/no", defaultReadOnly)
	if err != nil {
		return err
	}
	cfg.ReadOnly = !strings.EqualFold(readOnly, "no")
	defaultAllSessions := "no"
	if cfg.SyncAllSessions {
		defaultAllSessions = "yes"
	}
	allSessions, err := ask(reader, "同步所有本机主会话 yes/no", defaultAllSessions)
	if err != nil {
		return err
	}
	cfg.SyncAllSessions = !strings.EqualFold(allSessions, "no")
	if cfg.SyncAllSessions {
		cfg.ReadOnly = true
		fmt.Println("已启用本机主会话只读同步。")
		value, err := ask(reader, "纳入最近多少小时发生对话的会话（0 表示不限）", strconv.Itoa(cfg.SessionActiveHours))
		if err != nil {
			return err
		}
		cfg.SessionActiveHours, err = strconv.Atoi(value)
		if err != nil {
			return errors.New("会话活动窗口必须是整数小时")
		}
	}
	if cfg.ReadOnly && cfg.SyncAllSessions {
		defaultCleanup := "no"
		if cfg.AutoDeleteInactiveGroups {
			defaultCleanup = "yes"
		}
		value, err := ask(reader, "自动解散超过期限无对话的自动创建群 yes/no", defaultCleanup)
		if err != nil {
			return err
		}
		cfg.AutoDeleteInactiveGroups = !strings.EqualFold(value, "no")
		if cfg.AutoDeleteInactiveGroups {
			value, err := ask(reader, "群连续多少小时无对话后解散", strconv.Itoa(cfg.GroupIdleHours))
			if err != nil {
				return err
			}
			cfg.GroupIdleHours, err = strconv.Atoi(value)
			if err != nil {
				return errors.New("群过期时间必须是整数小时")
			}
		}
	} else {
		cfg.AutoDeleteInactiveGroups = false
	}
	if cfg.OwnerOpenID, err = ask(reader, "飞书 owner Open ID", cfg.OwnerOpenID); err != nil {
		return err
	}
	if cfg.OwnerOpenID == "" {
		return errors.New("飞书 owner Open ID 不能为空")
	}
	marketplace, err := ask(reader, "Codex marketplace 名称", cfg.Marketplace)
	if err != nil {
		return err
	}
	if marketplace != "" {
		cfg.Marketplace = marketplace
	}
	defaultAutoGroups := "no"
	if cfg.AutoCreateGroup {
		defaultAutoGroups = "yes"
	}
	autoGroups, err := ask(reader, "自动为新会话创建私有群 yes/no", defaultAutoGroups)
	if err != nil {
		return err
	}
	cfg.AutoCreateGroup = !strings.EqualFold(autoGroups, "no")
	if err := cfg.Validate(); err != nil {
		return err
	}
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return errors.New("找不到 codex 命令，请先将 Codex CLI 加入 PATH")
	}
	cfg.CodexBinary, err = filepath.Abs(codexPath)
	if err != nil {
		return err
	}
	defaultAppID := ""
	if previousCredentialsErr == nil {
		defaultAppID = previousCredentials.AppID
	}
	appID, err := ask(reader, "飞书 App ID", defaultAppID)
	if err != nil {
		return err
	}
	fmt.Print("飞书 App Secret（留空则保留已保存凭据）: ")
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return err
	}
	defer clear(secret)
	appSecret := strings.TrimSpace(string(secret))
	if appSecret == "" && previousCredentialsErr == nil && appID == previousCredentials.AppID {
		appSecret = previousCredentials.AppSecret
	}
	credentials := config.Credentials{AppID: strings.TrimSpace(appID), AppSecret: appSecret}
	chat, err := feishu.New(cfg, credentials)
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := chat.Check(checkCtx); err != nil {
		return fmt.Errorf("飞书连通性检查失败：%w", err)
	}
	if cfg.InstalledBinary != "" {
		if err := service.Uninstall(); err != nil {
			return fmt.Errorf("停止现有服务以升级程序失败：%w", err)
		}
	}
	binaryPath, err := installBinary(cfg.CodexBinary)
	if err != nil {
		return err
	}
	cfg.InstalledBinary = binaryPath
	if err := config.Save(configDir, cfg); err != nil {
		return err
	}
	if err := config.SaveCredentials(configDir, credentials); err != nil {
		return err
	}
	if err := service.Install(binaryPath); err != nil {
		return fmt.Errorf("已保存本地配置，但常驻服务安装失败：%w", err)
	}
	fmt.Println("飞书连接检查通过，用户级服务已安装并启动。")
	fmt.Println("请在飞书开放平台确认已发布机器人、接收 im.message.receive_v1，并启用长连接。")
	fmt.Println("免 @ 群消息需要申请敏感权限 im:message.group_msg。")
	return nil
}

func ask(reader *bufio.Reader, label, defaultValue string) (string, error) {
	if defaultValue == "" {
		fmt.Printf("%s: ", label)
	} else {
		fmt.Printf("%s [%s]: ", label, defaultValue)
	}
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue, nil
	}
	return value, nil
}

func runBridge() error {
	cfg, credentials, err := loadConfiguration()
	if err != nil {
		return err
	}
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	release, err := service.AcquireRunLock(configDir)
	if err != nil {
		return err
	}
	defer release()
	instance, err := bridge.New(cfg, credentials, cfg.CodexBinary)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logDir := filepath.Join(configDir, "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	slog.SetDefault(slog.New(slog.NewTextHandler(logFile, nil)))
	if err := instance.Run(ctx); err != nil {
		slog.Error("桥接服务退出", "error", err)
		return err
	}
	return nil
}

func runHook() error {
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 2*1024*1024))
	if err != nil {
		return err
	}
	return hooks.Record(configDir, input)
}

func bind(args []string) (returnErr error) {
	flags := flag.NewFlagSet("bind", flag.ContinueOnError)
	threadID := flags.String("thread", "", "Codex thread ID")
	chatID := flags.String("chat", "", "Feishu chat ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *threadID == "" || *chatID == "" {
		return errors.New("用法：codex-feishu bind --thread <thread_id> --chat <chat_id>")
	}
	active, err := service.Active()
	if err != nil {
		return err
	}
	stopped := false
	if active {
		if err := service.Stop(); err != nil {
			return fmt.Errorf("写入会话绑定前停止桥接服务失败：%w", err)
		}
		stopped = true
		defer func() {
			if stopped {
				if err := service.Start(); err != nil {
					returnErr = errors.Join(returnErr, fmt.Errorf("重启桥接服务失败：%w", err))
				}
			}
		}()
	}
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	store, err := state.Open(filepath.Join(configDir, "state"))
	if err != nil {
		return err
	}
	bindErr := store.Bind(*threadID, *chatID)
	closeErr := store.Close()
	if err := errors.Join(bindErr, closeErr); err != nil {
		return err
	}
	if stopped {
		if err := service.Start(); err != nil {
			return fmt.Errorf("绑定已写入，但重启桥接服务失败：%w", err)
		}
		stopped = false
	}
	fmt.Println("已绑定 Codex thread 与飞书群。")
	return nil
}

func listThreads() error {
	cfg, err := loadConfigOnly()
	if err != nil {
		cfg = config.Defaults()
		cfg.CodexBinary, err = exec.LookPath("codex")
		if err != nil {
			return errors.New("找不到 codex 命令，请先将 Codex CLI 加入 PATH")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := appserver.Start(ctx, cfg.CodexBinary, nil, nil)
	if err != nil {
		return err
	}
	defer client.Close()
	threads, err := client.ListThreads(ctx)
	if err != nil {
		return err
	}
	for _, thread := range threads {
		name := thread.Name
		if name == "" {
			name = thread.Preview
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", thread.ID, thread.SourceKind(), status(thread), strings.ReplaceAll(name, "\n", " "))
	}
	return nil
}

func diagnoseAppServer() error {
	cfg, err := loadConfigOnly()
	if err != nil {
		cfg = config.Defaults()
		cfg.CodexBinary, err = exec.LookPath("codex")
		if err != nil {
			return errors.New("找不到 codex 命令，请先将 Codex CLI 加入 PATH")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := appserver.Start(ctx, cfg.CodexBinary, nil, nil)
	if err != nil {
		return err
	}
	defer client.Close()
	threads, err := client.ListThreads(ctx)
	if err != nil {
		return err
	}
	var cli, vscode, activeCLI int
	var targets []appserver.Thread
	for _, thread := range threads {
		switch thread.SourceKind() {
		case "cli":
			cli++
			if thread.ParentID == "" && !thread.Ephemeral {
				if thread.IsBusy() {
					activeCLI++
				}
				if len(targets) < 2 {
					targets = append(targets, thread)
				}
			}
		case "vscode":
			vscode++
		}
	}
	fmt.Printf("本机可见 thread：CLI %d，VS Code %d；活动 CLI %d。\n", cli, vscode, activeCLI)
	if len(targets) < 2 {
		fmt.Println("不足两个 CLI thread，跳过并行恢复检查。")
		return nil
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for _, thread := range targets {
		threadID := thread.ID
		wait.Add(1)
		go func() {
			defer wait.Done()
			resumed, err := client.ResumeThread(ctx, threadID)
			if err == nil && resumed.ID == threadID {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	fmt.Printf("同一 App Server 并行恢复检查：%d/2 成功。\n", succeeded)
	if activeCLI < 2 {
		fmt.Println("当前没有两个同时活动的 CLI thread；本次只验证多个历史 CLI thread 可并行恢复。")
	}
	if succeeded != 2 {
		return errors.New("并行恢复检查未全部成功")
	}
	return nil
}

func showStatus() error {
	cfg, err := loadConfigOnly()
	if err != nil {
		fmt.Println("配置未完成：", err)
		return nil
	}
	active, err := service.Active()
	if err != nil {
		return err
	}
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	bindingCount, err := state.BindingCount(filepath.Join(configDir, "state"))
	if err != nil {
		return err
	}
	stateText := "已停止"
	if active {
		stateText = "运行中"
	}
	fmt.Printf("服务：%s\n飞书区域：%s\n同步范围：%s\n发送时机：%s\n已绑定会话：%d\n", stateText, cfg.Region, cfg.SyncLevel, cfg.SendTiming, bindingCount)
	return nil
}

func installService() error {
	cfg, _, err := loadConfiguration()
	if err != nil {
		return err
	}
	if cfg.InstalledBinary == "" {
		return errors.New("缺少安装后的可执行文件路径，请重新运行 setup")
	}
	return service.Install(cfg.InstalledBinary)
}

func uninstall(args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := flags.Bool("purge-data", false, "删除配置、凭据、会话绑定和本地队列")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := service.Uninstall(); err != nil {
		return fmt.Errorf("移除系统服务失败，已停止后续清理：%w", err)
	}
	cfg, configErr := loadConfigOnly()
	if configErr != nil {
		cfg = config.Defaults()
	}
	codexPath := cfg.CodexBinary
	if codexPath == "" {
		codexPath, _ = exec.LookPath("codex")
	}
	if codexPath != "" {
		args := []string{"plugin", "remove", "codex-feishu-sync@" + cfg.Marketplace}
		if _, err := runCodex(codexPath, args...); err != nil {
			return fmt.Errorf("系统服务已移除，但卸载 Codex 插件失败：%w", err)
		}
	}
	if cfg.InstalledBinary != "" {
		if err := service.RemoveInstalledBinary(cfg.InstalledBinary); err != nil {
			return fmt.Errorf("Codex 插件已移除，但删除程序文件失败：%w", err)
		}
	}
	if *purge {
		configDir, err := config.Dir()
		if err != nil {
			return err
		}
		if err := os.RemoveAll(configDir); err != nil {
			return err
		}
		fmt.Println("服务、插件和本地配置数据已移除。")
	} else {
		fmt.Println("服务和插件已移除，本地配置与会话绑定仍保留。")
	}
	return nil
}

func loadConfigOnly() (config.Config, error) {
	dir, err := config.Dir()
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(dir)
}

func loadConfiguration() (config.Config, config.Credentials, error) {
	dir, err := config.Dir()
	if err != nil {
		return config.Config{}, config.Credentials{}, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return config.Config{}, config.Credentials{}, fmt.Errorf("请先运行 codex-feishu setup：%w", err)
	}
	credentials, err := config.LoadCredentials(dir)
	if err != nil {
		return config.Config{}, config.Credentials{}, fmt.Errorf("读取本地飞书凭据失败：%w", err)
	}
	return cfg, credentials, nil
}

func installBinary(codexPath string) (string, error) {
	source, err := os.Executable()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		codexPath = strings.TrimSuffix(strings.TrimSuffix(codexPath, ".cmd"), ".bat")
	}
	directory := filepath.Dir(codexPath)
	name := "codex-feishu"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	destination := filepath.Join(directory, name)
	source, _ = filepath.Abs(source)
	destination, _ = filepath.Abs(destination)
	if source == destination {
		return destination, nil
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	if err := os.MkdirAll(directory, 0755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(directory, ".codex-feishu-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, input); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func runCodex(executable string, args ...string) (string, error) {
	command := exec.Command(executable, args...)
	if runtime.GOOS == "windows" && (strings.HasSuffix(strings.ToLower(executable), ".cmd") || strings.HasSuffix(strings.ToLower(executable), ".bat")) {
		parts := make([]string, 0, len(args)+1)
		parts = append(parts, `"`+strings.ReplaceAll(executable, `"`, `""`)+`"`)
		parts = append(parts, args...)
		commandArgs := []string{"/D", "/S", "/C", strings.Join(parts, " ")}
		command = exec.Command("cmd.exe", commandArgs...)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return string(output), nil
}

func status(thread appserver.Thread) string {
	if thread.IsBusy() {
		return "active"
	}
	return "idle"
}

func printUsage() {
	fmt.Println("Codex 飞书双向同步")
	fmt.Println("  codex-feishu setup                  配置凭据、连通性检查并安装服务")
	fmt.Println("  codex-feishu run                    前台运行桥接服务")
	fmt.Println("  codex-feishu status                 查看服务状态和绑定数")
	fmt.Println("  codex-feishu threads                列出本机可见 Codex thread")
	fmt.Println("  codex-feishu diagnose               检查 App Server 和并行恢复能力")
	fmt.Println("  codex-feishu bind --thread ID --chat ID 绑定已有飞书群")
	fmt.Println("  codex-feishu install-service        安装或重启系统服务")
	fmt.Println("  codex-feishu uninstall [--purge-data] 停止服务并卸载插件")
}
