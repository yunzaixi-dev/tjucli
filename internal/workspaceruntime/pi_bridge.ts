// Generated privately by workspaceruntime; not a standalone app entry.
// API: installed Pi 0.85.1 docs/extensions.md, Custom Tools / Tool Definition.
import { Type } from "typebox";
import { spawn } from "node:child_process";
import { readFile } from "node:fs/promises";

const executable = __TJUCLAW_EXECUTABLE__;
const configDir = __TJUCLAW_CONFIG_DIR__;
const cwd = __TJUCLAW_CWD__;
const linked = __TJUCLAW_LINKED__;
const mcpRefs = __TJUCLAW_MCP_REFS__;
const mcpEnvFile = __TJUCLAW_MCP_ENV_FILE__;
const maxBytes = 1024 * 1024;
const capabilities = ["pi.prompt", "claude.prompt", "codex.prompt", "mcp.call"];
const safeID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;

function object(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function exact(value, keys) {
  if (!object(value) || Object.keys(value).some((key) => !keys.includes(key))) {
    throw new Error("workspace_invalid_arguments");
  }
}

function select(value, keys) {
  const result = {};
  for (const key of keys) if (Object.hasOwn(value, key)) result[key] = value[key];
  return result;
}

function publicData(command, data) {
  if (!object(data)) throw new Error("workspace_response_invalid");
  // MCP helper already suppresses secrets; retain only MCP tool result fields,
  // never arbitrary top-level CLI/config diagnostics.
  if (command === "run") return select(data, ["content", "structuredContent", "isError"]);
  if (command === "workspaces") {
    if (!Array.isArray(data.workspaces) || data.workspaces.length > 1024) {
      throw new Error("workspace_response_invalid");
    }
    return { workspaces: data.workspaces.map((item) => {
      if (!object(item)) throw new Error("workspace_response_invalid");
      return select(item, ["id", "name", "kind", "capabilities", "online", "last_seen_at", "created_at"]);
    }) };
  }
  // Never return ownership, connection tokens, caller input arguments, or
  // accidental extra server fields. Results/errors are the public protocol.
  return select(data, ["id", "source_workspace_id", "target_workspace_id",
    "capability", "status", "result", "error", "created_at", "updated_at"]);
}

function run(command, args, input, signal, approvedEnv = {}) {
  if (signal?.aborted) return Promise.reject(new Error("workspace_request_canceled"));
  const body = input === undefined ? "" : JSON.stringify(input);
  if (Buffer.byteLength(body) > 64 * 1024) {
    return Promise.reject(new Error("workspace_invalid_arguments"));
  }
  // The CLI reads its own linked config locally. No account/provider keys,
  // connection token, or ambient startup hooks are passed as argv or env.
  const env = Object.create(null);
  for (const key of ["PATH", "SystemRoot", "HOME", "USERPROFILE", "TMPDIR",
    "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "LANG"]) {
    if (process.env[key] !== undefined) env[key] = process.env[key];
  }
  Object.assign(env, approvedEnv);
  return new Promise((resolve, reject) => {
    let child;
    let settled = false;
    let bytes = 0;
    let stderrBytes = 0;
    const chunks = [];
    const kill = () => {
      if (!child?.pid) return;
      try {
        if (process.platform === "win32") child.kill("SIGKILL");
        else process.kill(-child.pid, "SIGKILL");
      } catch {}
    };
    const finish = (error, result) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", abort);
      if (error) {
        kill();
        child?.stdin?.destroy();
        child?.stdout?.destroy();
        child?.stderr?.destroy();
        reject(new Error(error));
      } else resolve(result);
    };
    const abort = () => finish("workspace_request_canceled");
    const timer = setTimeout(() => finish("workspace_request_timeout"), 30000);
    signal?.addEventListener("abort", abort, { once: true });
    if (signal?.aborted) { abort(); return; }
    try {
      child = spawn(executable, ["--config-dir", configDir, command, ...args], {
        cwd, env, shell: false, detached: process.platform !== "win32",
        stdio: ["pipe", "pipe", "pipe"],
      });
    } catch { finish("workspace_request_failed"); return; }
    child.on("error", () => finish("workspace_request_failed"));
    child.stdin.on("error", () => finish("workspace_request_failed"));
    child.stdout.on("data", (chunk) => {
      bytes += chunk.length;
      if (bytes > maxBytes) { finish("workspace_response_limit"); return; }
      chunks.push(chunk);
    });
    child.stderr.on("data", (chunk) => {
      stderrBytes += chunk.length;
      if (stderrBytes > 64 * 1024) finish("workspace_response_limit");
    });
    child.on("close", (code) => {
      if (settled) return;
      try {
        const envelope = JSON.parse(Buffer.concat(chunks).toString("utf8"));
        if (code !== 0 || !object(envelope) || envelope.ok !== true) {
          finish("workspace_request_failed");
          return;
        }
        const data = publicData(command, envelope.data);
        const encoded = JSON.stringify(data);
        // Defense in depth if a helper/version accidentally returns a secret.
        // Compare after JSON decoding/re-encoding so escape forms cannot hide it.
        for (const value of Object.values(approvedEnv)) {
          if (value && encoded.includes(JSON.stringify(value).slice(1, -1))) {
            finish("workspace_request_failed");
            return;
          }
        }
        finish(null, { ok: true, data });
      } catch { finish("workspace_response_invalid"); }
    });
    child.stdin.end(body);
  });
}

function toolResult(envelope) {
  return { content: [{ type: "text", text: JSON.stringify(envelope) }], details: {} };
}

export default function workspaceBridge(pi) {
  const servers = Object.keys(mcpRefs).sort();
  if (servers.length !== 0) pi.registerTool({
    name: "workspace_mcp_call", label: "调用本环境 MCP 工具",
    description: "调用本机明确配置且授权的 stdio MCP 服务器工具。只能按已配置名称选择服务器，不能指定命令、环境变量或地址。",
    parameters: Type.Object({
      server: Type.String({ enum: servers, maxLength: 128 }),
      tool: Type.String({ minLength: 1, maxLength: 256 }),
      args: Type.Record(Type.String(), Type.Unknown()),
    }, { additionalProperties: false }),
    async execute(_id, params, signal) {
      exact(params, ["server", "tool", "args"]);
      if (typeof params.server !== "string" || !Object.hasOwn(mcpRefs, params.server) ||
        typeof params.tool !== "string" || params.tool.trim() === "" ||
        params.tool.length > 256 || /[\x00\r\n]/.test(params.tool) || !object(params.args)) {
        throw new Error("workspace_invalid_arguments");
      }
      if (signal?.aborted) throw new Error("workspace_request_canceled");
      let approvedEnv;
      try {
        // Read only privately generated data. Never discover scripts/config,
        // interpolate caller text, or copy the developer's full environment.
        const file = await readFile(mcpEnvFile);
        if (file.length > maxBytes) throw new Error();
        const values = JSON.parse(file.toString("utf8"));
        if (!object(values) || !object(values[params.server])) throw new Error();
        approvedEnv = Object.create(null);
        for (const host of mcpRefs[params.server]) {
          const value = values[params.server][host];
          if (typeof value !== "string" || value.length > 8192 || value.includes("\x00")) throw new Error();
          Object.defineProperty(approvedEnv, host, { value, enumerable: true });
        }
      } catch { throw new Error("workspace_request_failed"); }
      return toolResult(await run("run", ["mcp.call"], {
        server: params.server, tool: params.tool, arguments: params.args,
      }, signal, approvedEnv));
    },
  });
  if (!linked) return;
  pi.registerTool({
    name: "workspace_list", label: "列出同账号工作空间",
    description: "发现当前已连接账号下的系统环境、在线状态和明确开放的能力。离线或未授权环境不可调用。",
    parameters: Type.Object({}, { additionalProperties: false }),
    async execute(_id, params, signal) {
      exact(params, []);
      return toolResult(await run("workspaces", [], undefined, signal));
    },
  });
  pi.registerTool({
    name: "workspace_invoke", label: "调用同账号工作空间",
    description: "提交一次已授权的目标环境能力调用，返回调用 ID 与状态，不重试也不等于执行完成。用 workspace_invocation 查询结果。",
    parameters: Type.Object({
      target: Type.String({ pattern: safeID.source, maxLength: 128 }),
      capability: Type.String({ enum: capabilities }),
      arguments: Type.Record(Type.String(), Type.Unknown()),
    }, { additionalProperties: false }),
    async execute(_id, params, signal) {
      exact(params, ["target", "capability", "arguments"]);
      if (typeof params.target !== "string" || !safeID.test(params.target) ||
        !capabilities.includes(params.capability) || !object(params.arguments)) {
        throw new Error("workspace_invalid_arguments");
      }
      return toolResult(await run("invoke", [params.target, params.capability], params.arguments, signal));
    },
  });
  pi.registerTool({
    name: "workspace_invocation", label: "查询工作空间调用",
    description: "按调用 ID 查询同账号调用状态及公开结果。running/queued 不是成功，failed 不自动重放。",
    parameters: Type.Object({
      id: Type.String({ pattern: safeID.source, maxLength: 128 }),
    }, { additionalProperties: false }),
    async execute(_id, params, signal) {
      exact(params, ["id"]);
      if (typeof params.id !== "string" || !safeID.test(params.id)) {
        throw new Error("workspace_invalid_arguments");
      }
      return toolResult(await run("invocation", [params.id], undefined, signal));
    },
  });
}
