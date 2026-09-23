# Internal course and knowledge tool HTTP service

The `tjucli-server` binary exposes the existing public course provider for remote CLI calls. It does not execute arbitrary commands, access personal campus accounts, or implement the product task scheduler.

Build both binaries with `rtk task build` in this repository. From the integration checkout use `rtk task cli:build`. `task test` covers all Go packages, including HTTP transport.

## Local integration configuration

Server environment:

- `HTTP_ADDR`: listener, default `127.0.0.1:18090`. Use a trusted TLS ingress for non-loopback clients.
- `TJUCLI_GRANTS_FILE`: private regular JSON file, at most 64 KiB. Only its owner may access it.

Grant format:

```json
{"grants":[{"token_sha256":"<sha256 of a randomly generated opaque token>","run_id":"<32 lowercase hex characters>","expires_at":"<RFC3339 expiry>","scopes":["course:read","knowledge:read"]}]}
```

Generate a random token with at least 32 bytes of entropy outside the CLI. Keep the raw token in a separate private file for its one execution; the server stores only the SHA-256 digest. Do not put tokens in command arguments, source files or logs. Removing a grant blocks subsequent requests; expiry also bounds in-flight work. Grant-file issuance is a local integration adapter; the product backend issuer and sandbox lifecycle are not implemented by this service.

Client environment:

- `TJUCLI_MODE=remote`
- `TJUCLI_SERVER_URL`: HTTPS origin, or loopback HTTP for local integration. No URL credentials, query or fragment.
- `TJUCLI_TOKEN_FILE`: private regular file containing the opaque token.

Existing commands and JSON envelopes remain available. Standalone mode defaults to local for compatibility; the product sandbox must explicitly select remote. Remote errors never trigger direct-provider fallback. Version and help are local metadata and remain usable without service credentials.

## Protocol

Course routes require `Authorization: Bearer <token>` with a currently valid `course:read` grant. The knowledge route requires the separate `knowledge:read` scope. `/healthz` is an unauthenticated health endpoint. Grants contain no WeKnora administrator key.

| Route | Request JSON | Success |
| --- | --- | --- |
| `POST /v1/course/list` | `{"path":"/","cursor":""}` | Existing ListResult / ListMeta envelope |
| `POST /v1/course/search` | `{"query":"电路","max_pages":20,"limit":50}` | Existing SearchResult / SearchMeta envelope |
| `POST /v1/course/download` | `{"path":"/provider/file","max_bytes":1048576}` | Binary stream with exact Content-Length and X-Content-SHA256 |
| `POST /v1/knowledge/search` | `{"query":"电路","limit":50,"source":"course"}` | Citation-bearing `knowledge.SearchResult` envelope |

Requests require JSON, are bounded to 16 KiB and reject unknown fields. No request accepts an owner identity or local output path. Invalid/missing grants fail closed; concurrency is bounded and excess work receives HTTP 429. The server download ceiling is 64 MiB, even though standalone local mode accepts up to 1 GiB.

Knowledge search accepts a non-empty query of at most 8192 bytes, a limit from 1 to 100, and an optional source. The server uses the configured WeKnora retrieval provider with the request timeout and concurrency limits above; malformed provider results, including hits without complete citation provenance, return `protocol_error`, while provider failures return `upstream_error`.

For downloads the server chooses a private temporary path, completes and checks the upstream download, streams the file, then cleans it up. The CLI saves within its current working directory, verifies the declared size and SHA-256, and publishes without replacing an existing file. Failures remove partial output. File paths outside the workspace or escaping through symlinks are rejected.

The service does not establish that a caller is physically inside a particular cloud sandbox. That requires trusted issuance and deployment controls as well as these per-run grants. TLS, real backend issuance, cloud networking and sandbox integration need separate acceptance.
