# TJUCLI Instructions

Autonomous CLI and internal tool-service repository (`cmd/tjucli`, `cmd/tjucli-server`).
Standard-library Go tools covering the verified public course sharing provider.
Remote grants and file output boundaries are described in TOOL_SERVER.md; never add credentials to source.

The repository is public and released under GPL-3.0-only; `package.json` remains private only to prevent accidental npm publication of this Go component.

## Rules & Boundaries
- Module: `github.com/yunzaixi-dev/tjucli`
- Go version: `1.27.0` (source of truth: `go.mod`)
- No campus credentials or unverified features.
- Primary tasks: `task check`, `task test`, `task build`, `task cli:build`.
- Skill docs: `skills/tjucli/SKILL.md`.

## Branches

Use `release` as the primary branch for rapid iteration and production source. `dev` is retained without deleting it. Both branches run CI. Backend deployment is controlled by the integration repository pinned component SHAs; component pushes do not independently restart production services.
