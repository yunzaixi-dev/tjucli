# tjucli

Standalone Go CLI for Tianjin University data tools and campus services.
The current implemented provider covers the public course-sharing catalog at `cs.tjuse.com`.
No campus credentials or private student authentication tokens are required.

## Installation & Build

Requires Go 1.26+.

```bash
rtk task check
rtk task build
```

Binary outputs are `bin/tjucli` and `bin/tjucli-server`.
For remote tool calls and scoped grant configuration, see [TOOL_SERVER.md](TOOL_SERVER.md).
The HTTP service is an integration building block; product Pi/cloud sandbox execution remains pending.
You can also build directly using Go:

```bash
go build -trimpath -ldflags="-X main.version=$(node -p 'require("./package.json").version')" -o bin/tjucli ./cmd/tjucli
```

## Usage & Commands

```text
tjucli version [--json]
tjucli capabilities [--json]
tjucli course ls [PATH] [--cursor CURSOR] [--json]
tjucli course search QUERY [--max-pages N] [--limit N] [--json]
tjucli course download PATH --output FILE [--max-bytes N] [--json]
tjucli knowledge search QUERY [--limit N] [--source SOURCE] [--json]
```

See `TJUCLI.md` for full command specifications and safety boundaries.
For Pi agent skills, see `skills/tjucli/SKILL.md`.

## 许可证

`tjucli` 是公开开源仓库：[yunzaixi-dev/tjucli](https://github.com/yunzaixi-dev/tjucli)。源码采用 **GPL-3.0-only**。再发布 CLI、Tool Server 或其修改版本时，须遵守 GPLv3 的源代码、版权声明和许可证保留要求；完整文本见仓库根目录的 `LICENSE`。第三方依赖继续遵循各自许可证。

该许可证不授权 TJUClaw 私有 API 或 crawler 的源码。

## Development & Testing

```bash
rtk task test       # runs go test -race ./...
rtk task check      # runs fmt:check, lint, and race tests
```
