# Host workspace runtimes

Implementation, not deployment or live model acceptance. Only the local owner's
explicit `AllowedCapabilities` grants permit execution; unknown capabilities
remain denied even if accidentally listed. The connector must reload that local
config before each call. Remote JSON cannot select cwd, executable, argv, API
keys, endpoint URLs, plugins, or MCP commands.
Host Pi/Claude/Codex prompt execution currently requires Unix per-invocation
process-group supervision. All non-Unix prompts, including Windows, fail closed
before credential lookup or runtime/session state creation.

## Supported behavior

- `pi.prompt`: installed Pi JSON/print subprocess, full built-in host
  filesystem/shell tools under explicit `pi.prompt` consent. **Pi has no built-in
  tool approval gate or sandbox.** `--no-approve` means ignore project resources;
  it is not a tool permission restriction. Only explicit absolute-path plugins
  load; no package installation or global/project extension discovery. A private
  extension activates all configured built-ins (including grep/find/ls), without
  force-activating plugin tools or overriding plugin permission hooks.
- Linked Pi configs additionally load a privately generated extension with
  `workspace_list`, `workspace_invoke`, and `workspace_invocation`. They call the
  current CLI executable with fixed config dir/cwd, structured argv and bounded
  JSON stdin. An invoke only submits a queued call; it does not wait for success
  or retry. The server and target executor still enforce same-account ownership,
  online status and target capabilities. Unlinked configs expose no
  cross-workspace tools. The extension embeds no token, key or API URL and strips model
  credentials from its CLI child's environment.
- Pi also gets `workspace_mcp_call({server, tool, args})` when local `mcp.call`
  is explicitly allowed and MCP servers are configured, including standalone
  unlinked environments. Its server schema contains only configured names;
  it invokes the fixed current CLI `run mcp.call` using bounded stdin JSON.
  The CLI reloads local permissions/config and the runtime forwards its helper.
  No automatic tool discovery or caller-selected executable is involved.
  Only the selected server's approved host-env references reach that CLI child;
  values use a separate platform-private ephemeral file, never extension source,
  argv or Pi's ambient environment. Missing refs fail closed. Helper and bridge
  suppress echoed values, and final Pi output also redacts approved MCP values.
- `claude.prompt`: print/JSON, isolated bare startup, manual permissions, and
  `--permission-prompts none` by default. With an owner-local approval hook it
  uses stream-json SDK `can_use_tool` requests and `--permission-prompts host`.
  Built-in tools remain available; requests needing approval are denied unless
  the local owner explicitly accepts that individual request. No inherited
  account, project settings, MCP or plugin discovery.
- `codex.prompt`: app-server **stdio**, initialize/initialized, fresh ephemeral
  transport, durable thread/start or thread/resume and turn/start, final
  agentMessage items and turn/completed. Read-only sandbox and untrusted approval
  policy. Command/file approvals can be accepted once through an owner-local
  hook; no automatic approval, session-wide grants or policy amendments.
  Permission expansion, elicitation, auth-refresh callbacks and unknown host
  requests remain denied. Native UI wiring belongs to the parent.
- `mcp.call`: local capability/server checks then `workspacemcp.Call`. The
  helper owns initialization, configured argv/env and its isolated MCP cwd.
  Remote callers cannot override the command or provide a URL. This is separate
  from Claude/Codex model tool integration. Pi accesses it through the typed
  `workspace_mcp_call` bridge, not unrestricted native MCP configuration.

Prompt arguments are exactly `{prompt, endpoint?, session_id?}`; MCP arguments are exactly
`{server, tool, arguments: object}`. Duplicate/unknown/null fields are rejected.
An omitted endpoint selects the sole configured endpoint, otherwise a name is
required (except explicit native-account attachment below). No account/provider
fallback. Success returns `{runtime, output, session_id}`.

## Durable conversation contract

Omit `session_id` to create a conversation; supply the returned UUID to resume
with exactly one new prompt. Empty IDs, native IDs, partial UUIDs, paths and
implicit "latest" selection are rejected. Conversation metadata and native
history live under `StateDir/runtime/conversations/<runtime>/<opaque UUID>`.
They are bound to workspace ID, runtime, fixed Config.Root and endpoint/auth
source configuration; account tokens themselves are not part of metadata.

- Pi uses a fixed privately precreated session JSONL via `--session`; Claude
  uses persistent independent `CLAUDE_CONFIG_DIR` with `--session-id`/`--resume`;
  Codex uses persistent independent `CODEX_HOME` and stores its native thread ID
  privately before starting a turn. No transcript prompt reconstruction.
- Native files and metadata are synced, metadata replacement is atomic, and an
  OS lock serializes turns across processes. Different sessions are independent.
  A busy conversation returns `workspace_session_busy`. Crash/cancel/failure
  leaves an interrupted conversation; explicit resume never replays the
  previously delivered prompt. A native partial turn may remain in history.
- Local APIs are `ListSessions(ctx, runtime)` (empty runtime lists all allowed
  runtimes) and `Session(ctx, id)`. They return only
  `{id,runtime,endpoint,state,turns,created_at,updated_at}`; states are
  `ready|running|interrupted`. Stale running metadata is reported interrupted
  when no process holds the lock. No prompts, native IDs or filesystem paths.
- Local CLI commands select the same workspace configuration directory:

  ```sh
  tjuclaw --config-dir /absolute/workspace-config sessions
  tjuclaw --config-dir /absolute/workspace-config sessions --runtime pi
  tjuclaw --config-dir /absolute/workspace-config session "$SESSION_ID"
  ```

  `sessions [--runtime X]` accepts `pi`, `claude` or `codex`; omission lists
  locally allowed runtimes. `session ID` returns metadata, not a transcript.
  These are local commands, not remote capabilities or Pi bridge tools.
- Limits: 1024 conversations per runtime, 32 MiB total native state per
  conversation and 4096 files. Symlinks/special files are rejected. Native files
  and directories use the shared private helpers: 0600/0700 on Unix, protected
  owner-only DACLs on Windows. Oversize native state fails closed
  and requires local owner cleanup; no automatic deletion/compaction/replay.
- Cross-process locking uses Unix flock or Windows nonblocking LockFileEx;
  other platforms fail closed. Unix also fsyncs directories; Windows follows
  the shared config writer's file-sync/atomic-rename semantics, without claiming
  Unix-equivalent directory crash durability. Windows and macOS were only
  cross-compiled, not runtime-accepted on native hosts.

## Explicit native host-auth attachment

Only owner-authored `Config.RuntimeAuth` selects a native account:
`runtime_auth: {claude|codex: {mode,source,model?}}`. Absence continues to require
the independently configured endpoint, never ambient/global login.

- `host-file`: an explicitly selected absolute canonical private regular JSON
  credential file **in a private parent directory**. Shared privacy checks
  reject symlinks/reparse points and do not repair the selected source's
  permissions or ACL. Claude accepts `claudeAiOauth.accessToken` and passes only
  that token as `CLAUDE_CODE_OAUTH_TOKEN` with bare/safe startup; it does not
  copy refresh tokens, read the native keychain, or refresh/write the source.
  Codex validates the selected `auth.json`, copies only that file temporarily
  into the conversation's own CODEX_HOME, forces file auth storage and the
  built-in `openai` provider, and removes the snapshot after the invocation.
- `oauth-env`: Claude only, an explicit owner-selected host environment NAME;
  only its value becomes `CLAUDE_CODE_OAUTH_TOKEN`. Missing/invalid sources fail
  closed. No host HOME, settings, sessions, hooks, plugins or full env is copied.
- Native account mode rejects remote endpoint selection and never routes those
  credentials to a custom model URL. Optional model is owner-configured; omitted
  model uses the native provider default. Credential values are not in argv,
  generated source, session metadata, status APIs or public completion output.
- Source files are never modified. Claude token refresh and Codex source-login
  freshness remain owner-managed; this is not a keychain import/login/refresh UI.
  Pi native-account attachment is unsupported and explicitly fails closed.

## Owner-local approval transport contract

Set `Executor.Approvals ApprovalHandler`; nil denies. The hook is
`RequestApproval(ctx, ApprovalRequest) (ApprovalDecision,error)`.
Request JSON is `{id,runtime,session_id,kind,tool?,input,expires_at}`; response
JSON is `{request_id,decision:"allow"|"deny"}`. Inputs are bounded and selected
credential/endpoint values are redacted for the local display. An allow reply
uses the original native request, never modified tool input or persistent rules.
Hooks must honor context; late/error/unknown responses deny.

- `FileApprovals{Dir,Timeout}` uses
  `<Dir>/<random request ID>/{request,response}.json` with platform-private
  directories/files. Empty temporary files are secured before secrets are
  written, synced and atomically published; no partially written response.
  Responses must be atomic and match the ID exactly. Consumed, canceled and
  expired requests are removed. Native methods: `ApprovalDirectory()`,
  `PendingApprovals(ctx)` and `RespondApproval(ctx,id,ApprovalAllow|ApprovalDeny)`.
  `PendingApprovals` limits its serialized array to 1 MiB minus 4 KiB reserved
  for the native/CLI envelope. Overflow returns `workspace_runtime_output_limit`
  with no partial list; a caller must not present it as an empty queue.
- `StdioApprovals{Stream,Timeout}` uses the same newline-JSON on a dedicated
  local `io.ReadWriteCloser`, not model stdin. Timeout/cancel closes the stream;
  the owner transport must reconnect. Requests are serialized.
- Default/maximum approval wait is 30 seconds; a transport may shorten it.
  Codex hooks handle only matching active-thread/turn command/file requests,
  and Claude handles only SDK `can_use_tool`. Duplicate/unknown requests deny.
  This is a native owner interaction hook, not an auto-approval policy.
- Parent CLI/native must never expose response commands in the Pi model bridge,
  connector capability list or remote API. These files are not an OS security
  boundary against another same-user process/full-host Pi.

The CLI's `run` and `connect` executors now use `FileApprovals` for the selected
workspace. In another local terminal, the owner can inspect and respond:

```sh
tjuclaw --config-dir /absolute/workspace-config approvals
tjuclaw --config-dir /absolute/workspace-config approval "$APPROVAL_ID" allow
tjuclaw --config-dir /absolute/workspace-config approval "$APPROVAL_ID" deny
```

Choose **one** response after reviewing the request; the examples are
alternatives, not a sequence. Each reply applies only to that pending ID.
Missing, expired or denied replies do not authorize execution. The CLI provides
no remote automatic approval, session-wide grant or persistent allow policy;
reply commands are not exposed through remote capabilities or Pi bridge tools.
These commands do not add a per-tool approval gate
to Pi: its full host-tool consent remains the explicit local `pi.prompt` grant.

Endpoint transports are **OpenAI Chat Completions for Pi**, **Anthropic Messages
for Claude**, and **OpenAI Responses for Codex**. Config has no transport field;
an endpoint must serve the chosen adapter's protocol. Keys are read from the
named local environment variable, not stored in generated config or argv.
Empty `APIKeyEnv` allows keyless providers: Pi/Claude require a public dummy auth
marker (`workspace-keyless`); Codex omits `env_key`. A server rejecting any dummy
authorization header is not proven compatible with Pi/Claude.

Each process has a fresh private HOME/config/cache under StateDir, not a home
derived from cwd. Native conversation history persists independently; generated
invocation state and credential snapshots are removed afterward. Plugin files
are trusted local host code.
**These config-loading boundaries are not OS isolation:** with full Pi host
tools, the model can ask to read arbitrary host files or process environment.
Neither this package nor a local grant makes malicious prompts safe.

## Bounds and remaining limits

- Arguments: 64 KiB; prompt: 32 KiB; stdout/RPC and public result: 1 MiB;
  stderr: 64 KiB, never returned. Deadline: 30 minutes by default; owner-only
  `Executor.RunTimeout` can shorten/extend it up to 24 hours. A shorter upstream
  deadline always wins; prompt JSON cannot set duration. `RunTimeout` is
  currently a **library field only**, not a CLI flag or config field.
  CLI `run` and `connect` use the 30-minute local ceiling; requesting a longer
  remote invocation deadline does not extend that local ceiling. The connector
  also honors the invocation's bounded upstream deadline.
  MCP helper retains its separate shorter deadline. Selected model keys/gateway URLs are redacted from final
  prompt results; echoed configured MCP environment values fail closed.
- Unix cancellation kills the subprocess group and closes RPC pipes.
  **Pi/Claude/Codex prompt execution is disabled on all non-Unix platforms**:
  `Execute` returns `workspace_runtime_unavailable` before credential lookup,
  state/session creation or process startup. A safe per-invocation descendant
  supervisor is still missing. Killing only the immediate process, or enclosing
  the whole connector in a Windows Job, cannot satisfy individual invocation
  cancellation/deadline guarantees.
- Windows ACL and session-lock support remains available for local session
  metadata and owner approval APIs; it does **not** enable host prompt execution.
  Private helpers fail closed on foreign-owned objects or volumes without
  enforceable ACLs. Windows prompt execution is an unfinished release gate,
  not merely unverified.
  The MCP helper refuses to start on Windows and other platforms lacking its
  process-tree supervisor. Platforms without Unix or Windows session locking
  fail closed for conversation execution. macOS was cross-compiled only.
- No arbitrary shell command construction by the adapter/bridge. Pi's documented
  model-facing shell tool is intentionally enabled by local consent.
- No bundled native GUI approval UI, model protocol fallback, automatic plugin
  installation, Claude/Codex MCP-to-model
  injection, delivery replay, or background turn recovery.
- Cross-workspace extension registration and fake CLI handlers were verified;
  real model-driven environment-to-environment execution is **not** verified.
  Parent also reports the optional `rtk task workspace:system:test -- --pi`
  passing with compiled CLI, actual installed Pi, synthetic loopback OpenAI SSE,
  `workspace_list`, scoped CLI/API discovery, tool result and final assistant
  text. That verifies discovery and `$ENV` header resolution without a real
  model or developer account; it is not live mutual-agent invocation acceptance.
  The new Pi MCP bridge is verified with fake Pi registration, a fixed fake CLI
  and actual helper protocol against a fake MCP process. Parent additionally
  reports the actual installed-Pi synthetic endpoint smoke passing with both
  `workspace_list` and `workspace_mcp_call` (exit 0). This verifies synthetic
  model-directed tool routing through the CLI/MCP helper, not a real paid model,
  production Pi 1.0, live mutual-agent execution or deployment.

## Evidence and verification

Installed commands were checked with `--version` and `--help` in temporary HOME,
an empty environment except execution essentials, isolated agent config dirs,
and no account credentials. No model prompt was sent.

- Installed **Pi 0.85.1**, not a verified Pi 1.0 installation. Primary bundled
  `docs/usage.md`, `docs/security.md`, `docs/models.md`, `docs/extensions.md`,
  `docs/environment-variables.md`, and `docs/json.md` establish the emitted
  flags, full host permission boundary and extension `registerTool/execute`
  API. `models.md` explicitly requires `$VAR` (plain `VAR` is a literal).
  Installed `sessions.md` and `session-manager.js` establish explicit-path
  creation/resume, including initialization of a privately precreated empty file.
  Offline `--list-models` accepted both environment interpolation and the
  keyless placeholder. The installed SDK extension loader registered all three
  generated bridge tools without creating an agent or calling a model.
- Installed **Codex 0.160.0**. Official
  [app-server docs](https://developers.openai.com/codex/app-server),
  [CLI reference](https://developers.openai.com/codex/cli/reference) and
  [config reference](https://developers.openai.com/codex/config-reference)
  informed the protocol/provider setup. Installed generated JSON schemas
  establish actual wire enum spellings `untrusted` and `read-only`; some prose
  examples use older spellings. Schema generation and help do not send prompts.
- Installed **Claude Code 2.1.287**. Official
  [CLI reference](https://code.claude.com/docs/en/cli-reference) and
  [environment reference](https://code.claude.com/docs/en/env-vars) establish
  bare print/manual permissions, denial of unattended approval requests and
  custom gateway environment variables. `manual` is listed in installed help.
  Official Anthropic `claude-agent-sdk-python` `_internal/query.py` and
  `_internal/transport/subprocess_cli.py` establish stream-json initialization,
  `can_use_tool`, `updatedInput` and control-response envelopes. New session,
  safe-mode and stream flags were rechecked using installed help with isolated
  HOME and no auth. No live native account or charged prompt was tested.

Tests use the test executable as fake Pi/Claude/Codex/MCP/bridge CLI processes,
isolated temp dirs and synthetic keys. The generated extension handler harness
uses a fake registration API/schema stub and fake CLI, not a charged model.
Node-dependent harness is skipped when Node is absent; Unix-specific process
tree checks are not evidence of native Windows support.
MCP bridge tests additionally cover standalone/linked registration gates,
configured-name schemas, fixed argv/cwd/stdin, selected-only environment
transport, private file mode, missing refs, local permission revocation,
helper/bridge secret suppression and final Pi redaction.
Session/auth/approval tests additionally cover native history across executor
restarts, workspace/runtime/endpoint/account-source isolation, concurrent turns,
interrupted status/no replay, symlinks/history bounds, source nonmutation,
explicit OAuth refs, scoped owner approvals, queue/stdio ID matching, denial,
timeout/cancel and upstream/local task ceilings. Long wall-clock execution and
real native-account refresh/approval UI are not proved by these fake tests.
Private-state tests check shared platform privacy, atomic replacement, source
checks without repair, file/ancestor symlink refusal, nonblocking session locks,
and approval aggregate/envelope limits. Linux ordinary and race suites each
passed 101 tests after the non-Unix execution gate. The focused
conversation/session/deadline subset previously passed 8 tests normally and
8 with race detection (included in the full suite, not extra).
Runtime vet and Windows/macOS test cross-compilation also passed.
These are not native Windows/macOS execution evidence.
Platform regressions cover all three denied non-Unix prompts before endpoint
or native-account lookup, no state creation, retained local default-deny, and
Windows metadata/session/approval API availability. The denied-platform branch
and metadata regression were also exercised on Linux using a temporary Go
overlay with supervision disabled: 5 tests passed normally and 5 with race
detection. The overlay changed no repository source and does not test Windows
ACLs, locking or native process execution.
Parent separately reports local CLI session/approval command tests and an
embedded-CLI `.deb` smoke passing. Those do not establish real Claude/Codex
account acceptance, charged-model execution or a production deployment.

```sh
rtk go test ./internal/workspaceruntime
rtk go test -race ./internal/workspaceruntime
rtk go vet ./internal/workspaceruntime
```

Release note and integration wiring are parent-owned; no commits or deployment
are part of this package's work.
