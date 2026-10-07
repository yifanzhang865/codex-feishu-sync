# CLI 与飞书自动交接

从 0.3.0 起，`codex-feishu cli` 把原生 Codex 终端界面连接到桥接服务的本机控制入口。CLI 和飞书通过同一个 App Server 执行同一个 thread，服务按会话串行处理指令。无需手动退出 CLI 或停止桥接来交接。

## 启动与升级

需要已登录的 Codex CLI，且 `codex --help` 包含 `--remote`。已在 Linux 上验证 `codex-cli 0.159.2`。安装旧版本的机器先升级 Codex CLI，例如 npm 安装的用户可运行 `npm install -g @openai/codex`。

已有配置可以直接保留。把 `read_only` 设为 `false`，然后停止旧服务、构建并安装新程序。Linux/macOS/WSL 的 tmux 部署示例：

```bash
cd codex-feishu-sync
git pull --ff-only
# 已有 tmux 服务时，先正常停止，等待旧 App Server 退出。
tmux send-keys -t codex-feishu-sync C-c
# 确认该会话已结束后构建；若服务本来没启动，跳过上一行。
go build -trimpath -o "$(command -v codex-feishu)" ./cmd/codex-feishu
tmux new-session -d -s codex-feishu-sync 'exec codex-feishu run'
```

新机器先按[多机部署](multi-machine.md)执行 `setup --no-service`。系统服务部署使用 `codex-feishu install-service` 重启，不与 tmux 同时运行。

在项目工作目录打开新会话：

```bash
cd /path/to/your/project
codex-feishu cli
```

也可以传入原生 Codex 参数：

```bash
codex-feishu cli -C /path/to/your/project
codex-feishu cli resume <thread_id>
codex-feishu cli resume --last
```

保留原生终端界面、对话历史和审批操作。入口会自动添加 `--remote` 和令牌环境变量，不能自行指定 `--remote`、`--remote-auth-token-env` 或 `--no-daemon`。`login`、`plugin` 等管理命令仍直接使用 `codex`。

## 交接规则

| 动作 | 结果 |
| --- | --- |
| 打开或恢复 CLI 会话 | 订阅回复；不改变当前控制端 |
| CLI 提交新指令 | CLI 接管；飞书继续同步回复和完成通知 |
| 飞书 owner 在对应会话群提交新指令 | 飞书接管；原 CLI 继续显示回复 |
| 另一端再次提交新指令 | 再次交接；以本机服务串行接收的指令顺序为准 |
| 当前控制端继续提交 | 使用原有行为：CLI 可 steer；飞书忙碌时排队 |
| 当前只读 CLI 请求中断或修改会话 | 拒绝；提交新指令可接管 |
| 两个 CLI 查看同一会话 | 都可订阅；提交指令的终端接管，另一终端只读 |

跨端接管时，先撤销旧端的审批权限、清除旧飞书队列，再请求中断旧轮次。只有收到对应旧轮次的完成确认，才启动新指令；中断失败或确认超时不会同时启动第二个任务。旧端会继续显示新任务的回复。这里只读的是会话控制权限，原生 CLI 输入框仍可输入，新指令表示请求重新接管。

旧轮次会收到“任务已中断”状态，新轮次完成后发送“任务已完成”。中断不会回滚已经修改的文件、已经执行的命令或外部操作。不要依靠交接撤销已经产生的结果。

审批和问题只送给当前任务的控制端：CLI 任务在 CLI 回答，飞书任务通过飞书卡片或群文本回答。交接和轮次结束都会使旧审批失效，延迟点击不能批准后续任务。断开当前 CLI 的连接不会自动批准请求或把审批转给另一端；另一端提交新指令可接管。

`read_only: true` 保持固定飞书只读模式，飞书不能接管，CLI 正常使用。该全局设置与自动交接中的暂时只读权限不同。

## 兼容已有会话与多机部署

已经独立运行的 `codex` 或 VS Code 持有自己的写入连接，无法直接纳入交接。先正常退出原客户端，再执行 `codex-feishu cli resume <thread_id>`。只退出终端登录或 detach tmux 不等于退出 Codex。

每台机器独立运行桥接和 CLI 入口，共享飞书应用及 owner 即可。控制入口只监听本机回环地址的随机端口，使用随机令牌认证，并拒绝浏览器 Origin 请求；不会向其他机器开放端口。私有运行时文件 `control.json` 存在本机用户配置目录，正常退出后删除。机器之间不要复制这个文件、状态目录或 Codex 会话目录。

服务重启会断开连接的 CLI 和使审批失效。重启后重新执行 `codex-feishu cli resume <thread_id>` 即可；控制权不从旧进程恢复，首次新指令重新确定控制端。未执行的旧飞书队列在 CLI 重新接管时会被清除。

## 验证

```bash
go test ./...
go test -race ./internal/control ./internal/appserver ./internal/bridge
```

测试覆盖双向交接、旧轮次确认前禁止新任务启动、只读端中断拒绝、原生 steer 请求转换、旧队列清理、过期审批失效、连接身份隔离、回复订阅、认证及令牌文件清理。

在已登录的 Linux/macOS/WSL 本机上，还可验证真实原生 CLI 的启动兼容性：

```bash
CODEX_FEISHU_NATIVE_TEST=1 go test ./internal/control -run TestNativeRemoteCLIStartup -v
```

该检查使用隔离的临时 Codex 目录和伪终端，不提交模型任务，不连接飞书；需要本机安装 `python3`。它不替代实际租户中 CLI 与飞书轮流发送指令的验收。Windows/macOS 跨平台编译由 CI 检查，具体终端运行仍需目标机器验证。

协议依据：[官方 App Server](https://learn.chatgpt.com/docs/app-server)、[原生 CLI 的 remote 参数](https://learn.chatgpt.com/docs/developer-commands)。
