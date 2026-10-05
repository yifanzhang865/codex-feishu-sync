# Codex Feishu Sync

Codex Feishu Sync 是一个可单独安装的 Codex 插件与本地 Go 桥接服务。它为每个 Codex thread 绑定一个飞书群，在 Codex 与飞书之间同步可见消息、运行状态、工具输出和需要人工处理的审批或问题。所有 Codex thread 通过 `thread_id` 独立路由；内部推理不会转发。

## 功能

- 支持飞书和 Lark，使用官方 Go Channel SDK 建立 WebSocket 长连接。
- 支持 Codex CLI 和可由本机 App Server 列出的 VS Code 会话。
- 支持全会话只读同步：自动发现本机已有和新增的 CLI/VS Code 主会话，覆盖 tmux 中恢复的会话，无需依赖 `SessionStart` hook；不接管写入权限，不接受飞书指令或审批。
- 每个 thread 可自动创建私有群，也可手动绑定已有群；群在会话结束后保留。
- 飞书消息发送到对应 thread；忙碌时排队，`/interrupt <指令>` 请求中断当前轮次并优先提交该指令。若当前轮次编号暂不可用，指令仍会优先排队，并在群里明确提示无法立即中断。
- 飞书卡片可通过或拒绝工具审批；Codex 的问题可在对应群里用自由文本回答。
- 同步助手消息、用户消息、计划更新和状态；可配置工具输出范围：`conversation_status`、`with_tools`、`all_visible`；发送时机：`after_turn`、`streaming`。
- 首次绑定时记录当前历史基线，避免把旧对话整段转发；后续服务重启会从 App Server 恢复已完成 turn，只补发本地未记录的条目。App Server 不可见或未保留的会话不在恢复范围内。
- 用户级服务支持 Linux/WSL systemd、macOS launchd 和 Windows Task Scheduler。

## 安装

需要 Codex CLI、Go 1.24 或更新版本，以及一个飞书或 Lark 自建应用。Windows 运行时可直接使用发布文件；从源码构建则需要 Go。

先构建并把 `codex-feishu` 加入 `PATH`。下面以 Linux、macOS 或 WSL 为例：

```bash
mkdir -p "$HOME/.local/bin"
go build -trimpath -o "$HOME/.local/bin/codex-feishu" ./cmd/codex-feishu
export PATH="$HOME/.local/bin:$PATH"
```

Windows 可在 PowerShell 中构建，并把输出目录加入用户 `PATH`：

```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\bin" | Out-Null
go build -trimpath -o "$env:USERPROFILE\bin\codex-feishu.exe" .\cmd\codex-feishu
```

将下方 `<owner>/<repo>` 替换为本仓库的 Git 地址：

```bash
codex plugin marketplace add https://github.com/<owner>/<repo>
codex plugin add codex-feishu-sync@codex-feishu-sync
codex-feishu setup
```

`setup` 会交互式检查飞书凭据、保存用户级配置、将可执行文件安装到 Codex CLI 所在目录，并安装和启动当前用户的系统服务。配置所需的应用权限见[飞书应用配置](docs/feishu-app-setup.md)。

安装后可检查服务和本机 thread：

```bash
codex-feishu status
codex-feishu threads
codex-feishu diagnose
```

## 绑定已有群

`codex-feishu threads` 会列出当前 App Server 可见的 thread。将目标 thread 与已有飞书群绑定：

```bash
codex-feishu bind --thread <thread_id> --chat <chat_id>
```

不自动建群时，把 `setup` 中的自动建群选项设为 `no`，之后用此命令绑定。`chat_id` 可从飞书群详情或开发者工具中获取。

## 配置与数据

全会话只读同步可在 `setup` 中选择“同步所有本机主会话”，或在本机
`config.json` 中设置以下字段后重启桥接服务：

```json
{
  "read_only": true,
  "sync_all_sessions": true,
  "auto_create_group": true
}
```

这些字段合并到现有配置中，保留原应用及 owner 设置。会话发现包含所有模型
服务商的历史会话。桥接每十秒发现未归档的
主会话，每两秒轮询已绑定会话的完成回复；建群在后台进行。内部子代理和临时
会话跳过。新纳入的历史会话先建立基线，从启用后同步新回复；之后新建的会话
会同步第一轮完成回复。发送仍以完整轮次为单位，流式配置在只读模式下也按
完成轮次发送。旧的飞书队列保留，但只读模式不会提交它们。

设置向导会把配置和状态写入当前操作系统的用户配置目录下的 `codex-feishu-sync` 子目录。应用 Secret 单独保存在 `credentials.json`；Unix 系统目录权限为 `0700`，凭据文件权限为 `0600`。Windows 使用当前用户配置目录的访问控制。

本地状态包括 thread 与群的绑定、飞书事件和 Codex 条目去重记录、历史基线、尚未提交的飞书消息队列和服务日志。排队中的飞书消息会暂存为明文 JSON 文件；应按本机 Codex 数据的敏感级别保护用户配置目录。插件 hook 仅暂存 thread/session 标识、工作目录和来源，不保存 prompt。

## 卸载与清理

```bash
codex-feishu uninstall
```

此命令停止并移除用户级服务、移除 Codex 插件并删除安装的桥接程序，保留本地配置、凭据和绑定。确认也要删除本地数据后运行：

```bash
codex-feishu uninstall --purge-data
```

卸载不会移除你主动添加的 marketplace 来源，也不会删除飞书群。升级时先安装新版桥接程序，再运行 `codex-feishu setup`；向导会验证连接并替换旧服务。

对于 Git marketplace，可先刷新来源并重新安装插件，再用新版桥接程序运行 `setup`：

```bash
codex plugin marketplace upgrade codex-feishu-sync
codex plugin remove codex-feishu-sync@codex-feishu-sync
codex plugin add codex-feishu-sync@codex-feishu-sync
codex-feishu setup
```

WSL 使用 Linux 构建文件和 systemd 用户服务；需要在发行版中启用 systemd。运行 `codex-feishu bind` 时，如果桥接服务正在运行，程序会短暂停止服务、写入绑定并重新启动，以加载最新映射。

## 验证范围

运行 `codex-feishu diagnose` 可检查 App Server thread 列表，并尝试在一个 App Server 连接中并发恢复两个历史 CLI thread。恢复补发以 App Server 返回的已完成 turn 与条目 ID 为准；第一次记录某 thread 时只建历史基线，后续重启才补发未见过的完成条目。它不能代替两个同时活动 CLI 任务的实时订阅验收；只有本机同时存在两个活动 CLI thread 时才可检查该场景。VS Code thread 可见性不代表其他 Codex 客户端来源都受支持。

Linux、macOS、Windows 与 WSL 的服务构建由 CI 交叉编译检查。系统服务登录自启仍需在对应操作系统做安装和重启验收。飞书 WebSocket 实际收发、群创建和卡片回调需要在已发布并安装应用的测试租户完成端到端验证。

## 开发

```bash
go test ./...
go build ./cmd/codex-feishu
```

在仓库根目录添加 GitHub tag（例如 `v0.1.0`）会构建 Linux、macOS 和 Windows 发布文件。WSL 使用 Linux 构建文件。

## 许可

本项目使用 MIT License，见 [LICENSE](LICENSE)。

## 相关资料

- [Codex 插件打包](https://developers.openai.com/plugins/build/plugins)
- [Codex App Server](https://learn.chatgpt.com/docs/app-server)
- [飞书机器人概述](https://open.feishu.cn/document/client-docs/bot-v3/bot-overview)
- [Lark Go Channel SDK](https://github.com/larksuite/channel-sdk-go)
