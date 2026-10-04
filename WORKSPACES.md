# TJUClaw CLI 与系统工作空间

> 开发中：本文记录系统工作空间 CLI 的使用约定，不是上线公告。
> 已完成首批本地集成，服务端与 CLI 必须使用匹配版本。
> 本机构建和协议测试不代表真实模型验收、线上服务或桌面分发已交付。

## 三个不同的程序

- **`tjuclaw`**：产品 CLI，负责系统工作空间的本地配置、执行与账号内连接。
- **`tjucli`**：校园工具 CLI，继续提供自己的命令与 JSON 协议，
  不因新增产品 CLI 而改名或被替换。见 [README.md](README.md)、
  [TJUCLI.md](TJUCLI.md)。
- **`tjucli-server`**：校园工具 HTTP 服务，使用独立的 grants 授权机制。
  见 [TOOL_SERVER.md](TOOL_SERVER.md)。它的 grants token 不是系统工作空间连接 token。

## 安装

macOS 与 Linux（x64、arm64）：

```bash
curl -fsSL https://tjuclaw-release.zaixi.dev/cli/install.sh | sh
```

Windows（x64、arm64），在 PowerShell 中：

```powershell
irm https://tjuclaw-release.zaixi.dev/cli/install.ps1 | iex
```

脚本下载对应平台的 `tjuclaw`，先按发布的 `SHA256SUMS` 校验，再安装到
`~/.local/bin`（Windows 为 `%LOCALAPPDATA%\Programs\tjuclaw`，并加入用户 PATH）。
可用环境变量 `TJUCLAW_VERSION` 指定版本、`TJUCLAW_INSTALL_DIR` 指定目录。
重新运行同一命令即可升级。Windows 与 Linux 桌面客户端 0.1.1 起也内置了 `tjuclaw`。

## 从源码构建

从本 CLI 仓库目录执行，需要 `go.mod` 指定的 Go 1.27.0、
Node.js（读取 `package.json` 版本）和 Task：

```bash
rtk task build
# 兼容入口：rtk task cli:build
```

构建输出为 `bin/tjuclaw`、`bin/tjucli`、`bin/tjucli-server`。
`tjuclaw` 与 `tjucli` 使用 `package.json` 的现有版本，通过
`-ldflags="-X main.version=..."` 注入，不单独增加产品版本。
只构建产品 CLI 时也可执行：

```bash
go build -trimpath \
  -ldflags="-X main.version=$(node -p 'require("./package.json").version')" \
  -o bin/tjuclaw ./cmd/tjuclaw
```

以下示例假定你已把自己构建的 `tjuclaw` 放入 `PATH`；
也可把命令中的 `tjuclaw` 替换成该二进制的绝对路径。
发布的下载由 CI 在 `release` 分支版本号变化时构建（`scripts/build-downloads.sh`），
发布前扫描二进制，确保不含私有仓库、内网地址、本机路径或密钥。

```bash
tjuclaw version
```

产品 CLI 默认输出 JSON 信封，不需要 `--json`：成功是
`{"ok":true,"data":...}`，失败是 `{"ok":false,"error":{"id":"..."}}`
并返回非零退出码。`version` 不需要初始化环境。

**桌面开发包已实现随包携带 CLI，公开分发尚未发布。** 桌面连接使用固定的
随包程序，缺少程序时显示不可用；打开桌面页面不等于已连接或已运行环境。
Pi、Claude Code、Codex 和 MCP 服务也不因构建 CLI 而自动安装。
当前宿主机 prompt 执行要求 Unix 的逐调用进程组监督；Windows 等非 Unix 平台
会在读取凭据或创建会话前拒绝 prompt，Windows MCP 也保持拒绝。
非 Unix 的 `workspace allow` 不接受非空能力列表，但允许空列表撤销；
配置、状态、会话元数据和仅作为调用来源的连接仍可使用。

## 环境身份不是工作目录

工作空间是一整个可执行系统环境，而不是一个知识库、项目文件夹或会话。
环境有自己的身份、配置、能力许可、插件、MCP 服务、模型端点与运行状态。
完整 Pi 集成和独立会话属于这一环境模型，但不表示所有执行适配已经完成。

`--config-dir DIR` 选择环境实例，须在子命令前提供，也接受
`--config-dir=DIR`。不指定时使用操作系统用户配置目录下的 `tjuclaw`；
不会按当前项目目录自动区分环境。因此每个环境应显式使用独立的绝对
配置目录；不要让两台主机、两个账号或两个环境共用、同步或复制同一份连接配置。
配置包含私密连接凭据，应放在源码仓库之外。私密目录和文件分别使用
`0700`、`0600` 权限；这不是加密，仍需保护本机账号与备份。

`workspace init --root ROOT` 选择该环境 prompt 执行时的工作目录。
`ROOT` 是执行上下文，**不是身份凭据，也不是宿主机访问的安全沙箱**。
切换 shell 的 cwd 不会创建新环境，远程调用也不能通过参数覆盖配置中的 cwd。
当前 MCP helper 使用该环境配置目录中的独立服务状态目录作为 cwd，
而非 `ROOT`；需要项目路径的 MCP 服务应由本机配置显式传入绝对路径，
不要假定它启动在项目根目录。
经本机许可运行的 Agent 或 MCP 程序可能访问宿主机上其他资源，
实际范围还取决于操作系统权限和该程序自己的审批、隔离设置。

例如，两个环境必须选不同配置目录，即使它们偶尔处理同一个项目：

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" \
  workspace init --root "$HOME/study" --name "学习电脑"
tjuclaw --config-dir "$HOME/.config/tjuclaw/lab" \
  workspace init --root "$HOME/lab" --name "实验环境"
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" workspace status
```

先准备好相应工作目录；初始化不会覆盖已有环境配置。
`workspace status` 是本地脱敏配置检查，
成功输出形如 `{"ok":true,"data":...}`；它不证明云端在线或真实 Agent 执行成功。

## 连接账号：token 只粘贴到 stdin

先通过已认证账号的环境注册流程取得环境 ID 与一次性显示的
`connection_token`。首次绑定需要兼容的服务端；只有本地 CLI 不足以完成注册。
此 token 是绑定该环境的专属连接凭据，不是登录 cookie，也不是模型 API key。
“一次性”指创建时仅显示一次，不表示连接只允许使用一次。

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" \
  workspace link --api https://app.tjuclaw.cloud/api --id WORKSPACE_ID
```

将 `WORKSPACE_ID` 换成注册结果中的 ID。启动命令后，仅向这个进程的**标准输入**
粘贴 token，再结束输入；Unix 终端通常是输入换行后按 `Ctrl-D`。
注意终端可能回显，不要投屏、录屏或记录这次输入。
不要把真实 token 写入命令文本、`--token` 参数、URL、脚本、JSON 示例或仓库文件；
不要用把 token 当外部进程 argv 的方式传递。避免 shell tracing 和 CI 日志。

`--api` 是客户端 API Base URL，产品入口包含 `/api`，且只包含一次。
上例只是 URL 格式示例，不保证当前线上服务已支持本协议。
如接入自建入口，请替换为可信的服务地址，不把凭据拼进 URL。
连接 token 会保存在该环境的私密本地配置中，不应在状态输出或诊断中出现。
它还能列出同账号环境、提交已获许可的调用和读取同账号调用的参数与结果；
不能把它当成只有心跳权限的低敏凭据。
服务端删除环境会撤销其连接和队列；丢失 token 时走注册/撤销流程，
不要从日志、他人的配置或登录 cookie 中寻找替代凭据。

## 默认不允许远程执行

新环境的能力许可列表默认为空。初始化、绑定账号、配置模型或运行 connector
都不等于允许别人触发本机 Agent。已连接也不等于发布了能力。

首版能力名限于：

- `pi.prompt`
- `claude.prompt`
- `codex.prompt`
- `mcp.call`
- `terminal.open`（远程终端，见下文）

确认工具已安装、配置正确并了解宿主机风险后，只显式开放需要的能力：

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" workspace allow pi.prompt
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" workspace status
```

`workspace allow CAP...` **替换**本机许可列表，不是追加。
不带能力名可清空全部许可：

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" workspace allow
```

这是本机配置操作，不是让远程参数扩大权限的通道。
服务端检查目标发布的能力，本地执行器还会再次检查本机许可；
本地 `run` 也不绕过能力检查。许可不自动同意 Claude/Codex 的审批，
不关闭它们的 sandbox，也不保证某能力已经具备可用的执行适配。

## 远程终端

开放 `terminal.open` 后，在 TJUClaw 网页或客户端“工作”侧栏点主机行的终端按钮，
或在项目菜单选“在终端中打开”，就会在这台电脑上启动你的登录 shell
（`$SHELL -l`，环境变量 `TERM=xterm-256color`），终端停靠在页面下方。

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" workspace allow pi.prompt terminal.open
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" connect
```

- shell 只在 `workspace init --root` 选定的根目录内启动；项目路径（解析符号链接后）
  不在根目录内时拒绝。shell 启动后拥有你本机账号的全部权限，可以 `cd` 到任何地方，
  所以只在信任的同账号环境开放。
- 每次打开都会重新读取本机许可；撤销 `terminal.open` 后新终端立即被拒绝。
  已运行的 `connect` 在开放后无需重启。
- 每台电脑最多同时 4 个终端；闲置 30 分钟自动关闭；关闭标签页会挂断 shell。
- 终端经 API 中转（HTTPS 长轮询，无需开放入站端口）。中转只在内存中保留最近
  1 MB 输出，不写入磁盘。API 重启会结束所有终端。
- Linux 与 macOS 使用伪终端，支持 vim、htop 等全屏程序与窗口大小同步。
  Windows 暂为简化模式（PowerShell 经管道运行）：命令可用，但没有输入回显、
  行编辑和全屏程序。
修改许可会影响后续执行，不保证中断已经启动的调用，也不能收回已发送的内容；
需要停止当前 connector 时应明确终止该进程。
当前 Pi 在明确授权后启用完整宿主机文件与 shell 工具，没有内置逐次审批或
操作系统沙箱；配置目录隔离不限制它读取其他宿主机文件。Claude 拒绝需要人工
审批的操作，Codex 使用只读 sandbox 并拒绝审批请求，尚无交互式审批入口。

停止 connector 后，可以清除本机连接凭据：

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" workspace unlink
```

`unlink` 只清除本地 API 地址和 token，不删除服务端环境，也不撤销仍在其他
地方保存的 token；彻底撤销需要通过已认证账号删除服务端环境。

连接器会在向服务端报告结果前，先把完成结果写入本机私密待发送目录。
网络故障后重新连接，只重发该结果，不重新执行已经交付的主机操作；如果服务端
已完成，会核对相同结果再清除待发送记录。待发送记录按连接凭据和地址隔离，
不会在重新绑定另一个账号或地址后发给新连接。冲突或撤销会保留本机结果并报告
连接失败，不会悄悄丢弃它；这不是断点续跑，也不能恢复已中断的主机进程。

## 独立端点、插件与 MCP 配置

`workspace configure` 从 stdin 接收一个 JSON 对象，字段包括
`endpoints`、`plugins`、`mcp_servers`。配置对象不用于设置账号归属、
连接 token 或远程能力许可；它们分别由绑定与本机授权流程管理。
省略的配置字段保持原值，提供的数组/映射整体替换；
用 `[]` 或 `{}` 清空对应集合，不用 `null`。
桌面添加界面使用固定的 `workspace configure --merge`：端点按名称更新或新增，
插件引用去重追加，未提供的当前项不删除；空数组在此模式下不清空。
此模式只接受 `endpoints` 和 `plugins`。需要明确删除或修改高级配置时，
使用普通替换配置或桌面的可信原生文件导入流程。
以下为配置格式示例，需要将地址、模型名和插件路径换成你自己的可信配置：

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" workspace configure <<'JSON'
{
  "endpoints": [
    {
      "name": "study-model",
      "base_url": "https://models.example.com/v1",
      "model": "your-model",
      "api_key_env": "STUDY_MODEL_API_KEY"
    }
  ],
  "plugins": ["/absolute/path/to/trusted-plugin.ts"],
  "mcp_servers": {}
}
JSON
```

`api_key_env` 保存的是本机环境变量的**名字**，不是 key 的值。
在启动本环境的 CLI/connector 前，用你自己的安全凭据方式设置该变量；
不要把真实 key 写进配置、stdin 示例或 shell 历史。
多个环境应使用各自的变量名与启动环境，不借用开发者的全局模型账户。
配置端点名称只选择已配置的端点，不允许调用参数临时传入任意模型 URL 或 key。
编辑端点时省略 `api_key_env`，仅在名称和 Base URL 都未改变时保留当前引用；
显式设置为 `""` 会撤销引用。修改 Base URL 不会把已有凭据自动转发给新地址，
必须重新明确配置。引用保留基于事务内的最新状态，不会恢复刚刚撤销的凭据。

插件路径只应指向可信、已安装的插件；声明路径不表示已下载或已验证插件。
当前 Pi 适配要求绝对插件路径，不会自动安装包引用。
每个环境的 Pi 配置、会话和插件状态应独立，不继承另一个环境的配置与凭据。

`mcp_servers` 按服务名保存本机 stdio 服务定义，
每项包含 `command`、`args`、`env`。例如：

```json
{
  "mcp_servers": {
    "local-tools": {
      "command": "/absolute/path/to/trusted-mcp-server",
      "args": [],
      "env": {"SERVICE_API_KEY": "STUDY_MCP_API_KEY"}
    }
  }
}
```

服务路径是示例，CLI 不提供这个程序。`env` 是“子进程变量名 → 本机变量名”
映射：上例从本机 `STUDY_MCP_API_KEY` 取值，交给子进程的 `SERVICE_API_KEY`。
两边都写裸变量名，不写 `${NAME}` 插值表达式，更不写秘密字面值；
无需变量时用 `{}`。MCP 执行限于配置中的服务和工具，
调用参数不能覆盖服务的 command、argv 或地址。配置成功不等于 MCP 握手、
工具调用或 Pi 插件加载已经通过真实验证。

## 本机执行、保持连接与账号内互调

所有执行参数都从 stdin 读取 JSON，不从位置参数传入 prompt 或工具参数。
以下示例会在能力获许可且执行适配可用时触发真实工具，
可能向你选择的模型服务发送内容并产生费用，不是免费的自检命令。

```bash
printf '%s\n' '{"prompt":"概述当前项目","endpoint":"study-model"}' |
  tjuclaw --config-dir "$HOME/.config/tjuclaw/study" run pi.prompt

printf '%s\n' '{"server":"local-tools","tool":"inspect","arguments":{}}' |
  tjuclaw --config-dir "$HOME/.config/tjuclaw/study" run mcp.call
```

prompt 能力接受 `prompt` 与可选的已配置 `endpoint` 名称；
MCP 接受 `server`、`tool`、`arguments` 对象。
省略 `endpoint` 时必须恰好配置一个端点，否则需要显式选择。
端点协议必须兼容所选运行时：Pi 使用 OpenAI Chat Completions，
Codex 使用 Responses，Claude 使用自身的模型服务协议；
不能假定同一个服务地址兼容三个运行时。
工具名须真实存在；这些示例不是成功运行的证据。

显式启动 connector 才会保持连接、同步已许可能力并领取调用：

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" connect
# 单轮连接/领取处理，不常驻：
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" connect --once
```

保持终端进程运行；用 `Ctrl-C` 停止。桌面常驻连接也需要明确的本机操作，
不能因为加载远程网页就自动启动。
`--once` 不是等待任意数量远程任务完成的自动化服务。

先从已经绑定的源环境列出同一账号拥有的环境：

```bash
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" workspaces
```

`workspaces` 是顶层只读命令，不是 `workspace status` 的别名。
它使用该配置目录保存的专属 Bearer token 请求服务端，
返回 `{"ok":true,"data":{"workspaces":[...]}}`；
每项包含 `id`、`name`、`kind`、`capabilities`、`online`。
本地 `workspace status` 则只读取当前环境的脱敏配置。
列表限于 token 所属账号，不返回 owner 或 token，也不允许注册、删除环境
或扩大目标能力许可。列出目标不等于获得执行权限；在线状态只是查询时的状态，
实际提交与执行仍须再次检查。

从已经绑定的源环境请求另一个环境执行：

```bash
printf '%s\n' '{"prompt":"概述当前项目"}' |
  tjuclaw --config-dir "$HOME/.config/tjuclaw/study" invoke TARGET_WORKSPACE_ID pi.prompt
tjuclaw --config-dir "$HOME/.config/tjuclaw/study" invocation INVOCATION_ID
```

`TARGET_WORKSPACE_ID` 是**同一账号拥有的目标环境 ID**，不是目录、
任意 URL、主机名或 shell 命令。源和目标的归属由服务端认证身份确定，
不接受调用者提供 `owner` 来冒充账号。知道目标 ID 不构成授权。
目标必须在线、显式发布该能力，并在实际执行时仍满足本机许可。
跨账号访问与不存在的私密记录使用相同的 404。

`invoke` 创建调用后，用返回的调用 ID 执行 `invocation ID` 查询。
调用状态为 `queued`、`running`、`succeeded` 或 `failed`；
请求获接收不等于执行完成。离线或能力未许可时不能发起执行。
交付的调用不会自动重放，超时/失败也不承诺自动重试；
先查调用状态，避免盲目重复提交引发重复操作。

## Agent 自动调用的边界

现有云端沙箱 Pi 有通用的产品工具入口：在兼容服务端已注册提供方、
本轮工具授权有效且工具链可用时，可通过 `tjucli tools list --json`
发现 `workspace_list`、`workspace_invoke`、`workspace_invocation_status`，
再通过 `tjucli tools call` 调用。工具仍按该会话的已认证账号路由，
提交调用只返回异步调用 ID，不等待本机执行完成。
这是已有入口的代码接线，不代表线上已升级或真实模型链已验收。

本 CLI 的本地 `pi.prompt` 适配与上述云端运行时不是同一条执行链。
本机明确授权 `pi.prompt` 后，Pi 可以使用完整的宿主机文件与 shell 工具；
这不是沙箱隔离，也没有逐条命令审批。未授权时不会启动执行。
已绑定账号的 Pi 通过内置扩展获得 `workspace_list`、`workspace_invoke`、
`workspace_invocation`，固定调用本环境 CLI，不把令牌或模型钥匙写入扩展或模型参数。
异步调用成功提交不等于目标已经执行；不会自动重试。

本环境额外允许 `mcp.call` 并配置 MCP 服务后，Pi 可使用 `workspace_mcp_call`。
它只接受已配置的服务名、工具名和结构化参数，不自动安装或发现所有 MCP 工具；
没有账号连接的环境也可以独立使用本地 MCP。

配置更新会在跨进程事务内读取当前配置，仅修改本次指定字段。
等待 stdin 不会持有锁或把旧授权保存回去，避免刚撤销的权限被旧命令恢复。
已经启动的进程不会因清空能力列表而立即停止。

可在集成仓库运行以下隔离验收，不使用开发者登录或真实模型：

```bash
rtk task workspace:system:test
# 可选：实际安装的 Pi + 合成的回环模型端点；不是线上模型验收
rtk task workspace:system:test -- --pi
```

## 当前不承诺的能力

- 完整 Pi/cloud 自动化与所有 Agent 的真实模型执行验收。
- 桌面安装包公开分发、全平台宿主执行或后台服务自动安装。
- 现有云端会话自动迁移、长期任务恢复或跨账号互调。
- 云端 Pi 作为可被其他环境调用的目标，或完整双向互调。
- 把工作目录当安全隔离、把配置文件权限当加密，或自动开放宿主机权限。

请分别检查本机 CLI、工具安装、环境配置、能力许可、服务端协议与目标在线状态。
本地构建、模拟进程测试、CI 产物和脱敏状态输出，都不能单独证明线上执行链可用。
