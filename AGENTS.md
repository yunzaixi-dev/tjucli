# TJUCLI Instructions

Autonomous CLI and internal tool-service repository (`cmd/tjucli`, `cmd/tjucli-server`).
Standard-library Go tools covering the verified public course sharing provider.
Remote grants and file output boundaries are described in TOOL_SERVER.md; never add credentials to source.

## Rules & Boundaries
- Module: `github.com/yunzaixi-dev/tjucli`
- Go version: `1.26.0`
- No campus credentials or unverified features.
- Primary tasks: `task check`, `task test`, `task build`, `task cli:build`.
- Skill docs: `skills/tjucli/SKILL.md`.

## Branches

Use only `dev` (default, direct development) and `release` (verified production source). Both branches run CI. Backend deployment is controlled by the integration repository pinned component SHAs; component pushes do not independently restart production services.
