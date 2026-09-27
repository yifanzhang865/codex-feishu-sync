# 飞书应用配置

本指南用于配置独立的飞书/Lark 自建应用。应用凭据保存在本机用户配置目录，不写入 Codex 全局配置或插件仓库。

## 创建应用

1. 在飞书或 Lark 开放平台创建企业自建应用，并启用机器人能力。
2. 记录 App ID；App Secret 在本地运行 `codex-feishu setup` 时输入，不要提交到仓库或发到群聊。
3. 将机器人应用安装到用于测试的租户，并发布包含所需权限和事件订阅的应用版本。
4. 在事件订阅中启用长连接/WebSocket，并订阅 `im.message.receive_v1`。
5. 使用审批卡片时订阅回调 `card.action.trigger`。

飞书与 Lark 的区域分别选 `feishu` 和 `lark`。程序根据区域连接对应的 OpenAPI 服务。

## 权限

按实际启用功能申请应用权限：

| 能力 | 需要的配置 |
| --- | --- |
| 接收群消息和普通消息 | `im:message`，并订阅 `im.message.receive_v1` |
| 机器人发送文本和交互卡片 | `im:message:send_as_bot` |
| 卡片审批按钮 | 订阅 `card.action.trigger` 回调 |
| 自动创建会话私有群并加入会话所有者 | `im:chat:create`；开放平台也可能列出较宽的 `im:chat` 权限 |
| 群内不 @ 机器人直接发送指令 | 申请敏感权限 `im:message.group_msg`，并在 SDK 策略中关闭必须 @ 的要求 |

创建群接口会自动把调用机器人的应用加入新群，代码在同一请求中邀请配置的 owner。它不会另行调用群成员添加接口，因此不需要单独申请 `im:chat.members:write_only`。权限名称可能会随开放平台界面语言或应用类型显示不同；手动绑定群可关闭自动建群并省略建群权限。权限或事件订阅变更后，需要重新发布应用并确认测试租户已安装新版。

## 运行向导

```bash
codex-feishu setup
```

按提示输入：

- 服务区域 `feishu` 或 `lark`
- 同步范围 `conversation_status`、`with_tools` 或 `all_visible`
- 发送时机 `after_turn` 或 `streaming`
- 机器人所有者的飞书 Open ID，用于限制哪些成员可以向 Codex 发指令或回答问题
- marketplace 名称，默认 `codex-feishu-sync`
- 是否自动创建新会话私有群
- App ID 与 App Secret

向导会请求机器人身份完成连通性检查。通过后会保存配置、安装服务并启动。如果连通性检查失败，请先核对区域、凭据、机器人能力和租户安装状态。

## 排查

- **收不到群消息**：检查机器人在群内、`im.message.receive_v1` 已订阅、长连接/WebSocket 已启用；免 @ 消息还需要敏感权限 `im:message.group_msg`。
- **卡片按钮无响应**：确认 `card.action.trigger` 已订阅，权限/事件调整后应用已重新发布并安装。
- **发送消息失败**：检查机器人发送消息权限、机器人仍在目标群内，以及飞书/Lark 区域与租户匹配。
- **自动创建群失败**：检查 `im:chat:create` 权限，且应用机器人可用范围包含配置的所有者。
- **服务未启动**：运行 `codex-feishu status` 查看状态；服务日志位于用户配置目录的 `logs/service.log`。运行 `codex-feishu install-service` 可重新安装用户级服务。
- **会话没有自动绑定**：运行 `codex-feishu threads` 确认 thread 是否可见，再使用 `codex-feishu bind` 手动绑定已有群。

## 数据与权限边界

桥接服务仅接受配置的 owner Open ID 发来的群消息和卡片操作。其他成员的消息不会提交到 Codex。内部推理不会转发；敏感答案不会通过飞书同步，遇到敏感问题时会留空并提示用户回到本机 Codex 输入。飞书消息通过平台的 HTTPS/WebSocket 通道传输；尚未提交的消息会在本机用户配置目录中以明文排队保存，因此请限制该目录的本地访问权限。

参考：[飞书机器人概述](https://open.feishu.cn/document/client-docs/bot-v3/bot-overview)、[飞书群消息敏感权限说明](https://www.feishu.cn/content/article/7613711414611463386)、[Channel SDK 快速开始](https://github.com/larksuite/channel-sdk-go/blob/main/docs/zh-CN/quickstart.md)。
