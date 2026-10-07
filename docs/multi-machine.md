# 无网关多机部署

两台机器使用同一个飞书机器人应用和 owner，无需第二台连接第一台，无需公网 IP、端口映射、VPN、Redis 或独立服务器。每台只需能够访问飞书 OpenAPI/WebSocket，并能运行本机 Codex CLI。

## 第二台机器从源码启动

先安装 Git、Go 1.24 或更新版本、Codex CLI、tmux，并完成本机 Codex 登录。以下适用于 Linux、macOS 和 WSL：

```bash
git clone https://github.com/yifanzhang865/codex-feishu-sync.git
cd codex-feishu-sync
mkdir -p "$HOME/.local/bin"
go build -trimpath -o "$HOME/.local/bin/codex-feishu" ./cmd/codex-feishu
export PATH="$HOME/.local/bin:$PATH"
codex-feishu setup --no-service
codex-feishu diagnose-routing
tmux new-session -d -s codex-feishu-sync 'exec codex-feishu run'
codex-feishu cli
```

在向导中输入第一台使用的 **App ID、App Secret、owner Open ID**，多机路由选择 `yes`，本机名称填写便于区分的名称，例如 `laptop`、`gpu-server`。应用凭据不会随 Git 分发，所以每台首次部署仍需这一次配置；App Secret 在终端隐藏输入。新安装默认接受 owner 的飞书指令，需要只读同步时选择“仅同步回复” `yes`。两种模式都会发送完成通知。

向导把程序安装到 Codex 命令所在目录，系统服务选项被 `--no-service` 跳过。用 `tmux attach -t codex-feishu-sync` 查看前台，`Ctrl-b d` 离开 tmux；程序日志在用户配置目录的 `codex-feishu-sync/logs/service.log`。tmux 会在 SSH 断开后继续运行；机器重启后需重新启动，或改用 `setup` 安装系统服务。不要同时启动两种常驻方式。

默认全会话发现可独立工作，无需额外安装 hook。若也需要会话启动 hook，可从当前源码目录注册 marketplace 并安装仓库中的插件；它仍使用相同的本机程序。

## 共享哪些设置

| 参数或数据 | 两台机器的关系 |
| --- | --- |
| App ID、App Secret、owner Open ID、区域 | 相同，在每台分别配置 |
| `multi_machine` | 均设为 `true` |
| `machine_id` | 每台首次启动自动生成；重启保持稳定；不能复制 |
| `machine_name` | 每台独立，显示在新建会话群名中 |
| `routing_chat_id` | 程序自动发现同一个机器人专用私有群 |
| Codex 登录、工作目录、会话记录、`state/`、群绑定和队列 | 各机独立，不能复制或共用 |
| CLI 控制入口 `control.json`、访问令牌、当前控制端 | 各机独立，自动生成；不能复制或共用 |
| `read_only` | 各机独立；为 `true` 时不执行该机的群指令或审批 |
| `message_poll_seconds` | 各机独立，默认 `5`，允许 `2`–`300` 秒 |

“参数共享”指使用相同值；配置文件仍保存在各机本地。更换应用 Secret 或 owner 时，需要分别更新两台机器的配置。

## 路由如何工作

飞书会把同一应用的长连接事件分配给一个连接，不能假设两台都能收到。因此多机模式不依赖 WebSocket 消息事件来执行指令。

1. 本机为自己的 Codex 会话创建群，群描述包含本机 ID，群名包含本机名称。只读取本机状态文件中绑定的群，不扫描其他机器的会话。
2. 每台从飞书消息历史读取绑定群里的 owner 文本与富文本指令，保留消息 ID 和读取位置。读取成功后先持久化收件箱，再推进位置，随后执行或排队；网络恢复后从保存的位置续读。
3. 新审批卡片携带目标机器 ID 和随机请求标识。任意机器收到按钮回调后，把审批标识和决定签名写入 `Codex Feishu Sync Routing` 机器人专用私有群；目标机轮询该群处理，其他机器跳过。转交不包含任务正文、App Secret 或任意表单值。
4. Codex 回复始终由持有该会话的机器发送。每个终止轮次发送对应状态，包括本机 CLI 的只读完成轮次；已发送条目与轮次按 ID 去重。

不需要在飞书中输入机器地址；在对应会话群里发指令即可。手动绑定时，一个群只能由一台机器处理。不要把第一台整个配置目录复制到第二台，否则会复制机器身份和群绑定，破坏路由隔离。两台机器也不要同步同一份 Codex 数据目录。

机器人专用路由群不会邀请 owner，不属于会话群自动回收范围。不要删除它、修改绑定描述或转移群主。现有会话群升级后继续使用，旧群名无需立即改变；本机绑定仍是路由依据。

## 权限、离线与升级

多机模式需要应用身份权限 `im:chat:create`、`im:chat:read`，以及接口认可的消息历史读取权限（例如 `im:message` 或 `im:message:readonly`）。发送消息需要 `im:message:send_as_bot`；审批仍需长连接 `card.action.trigger` 回调。免 @ 的普通群消息需确保机器人可读取这类消息。权限变更后要发布并安装新的应用版本，详见[飞书应用配置](feishu-app-setup.md)。

旧版本配置不会自动开启多机模式。停止旧服务，重新运行 `setup --no-service` 并选择多机路由 `yes`，或者在原配置中加入 `multi_machine: true`、`message_poll_seconds: 5` 后重启。不要手动填写机器 ID。初次启用会为旧绑定群建立当前读取基线，不重新执行过去的群指令；新建群从建群时间开始读取。

机器离线时无法执行本机任务。历史仍在飞书保留的情况下，重新启动后可续读离线指令；超过飞书保留期限、消息被删除或群被解散的内容无法恢复。默认指令和按钮转交会有约一个轮询周期的延迟；大量群会增加 API 请求数和处理时间，可调大轮询间隔。

若服务在提交指令时异常退出，执行结果可能无法确认。程序会提示核对本机 Codex，不盲目重放这类指令；确认后重新发送。尚未开始处理的收件箱消息可正常恢复。审批请求随 App Server 连接结束而失效，重启后需重新发起；随机请求标识防止旧卡片误批准新的请求。正常消息去重不代表远程调用在任意崩溃下都能保证严格恰好执行一次。

使用 `codex-feishu cli` 或 `codex-feishu cli resume <thread_id>` 可启用本机 CLI/飞书自动交接。这个入口只连接本机 `127.0.0.1`，无需其他机器访问；飞书跨机器路由保持原样。必须使用支持 `--remote` 的 Codex CLI，已验证 `0.159.2`。详见[自动交接](control-handoff.md)。独立启动的 `codex` 或 VS Code 仍在退出后才能释放写入权限；只断开 tmux 不会释放它们。`read_only: true` 始终不接受本机群指令。

```bash
codex-feishu diagnose-routing
codex-feishu version
```

诊断检查机器人身份、路由群和已绑定群的历史读取权限，不启动第二个 Codex 写入连接。凭据保存在本机用户配置目录，不要提交到 Git。
