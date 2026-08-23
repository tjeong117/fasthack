# Potential Add-ons for Reducing Compute

These are valid extensions to Hindsight, but they are deliberately outside the four-hour MVP. The current product has one narrow correctness claim: replay a real, previously observed shell result only when the command, repository tree, and environment match. Each add-on below needs its own state model before it can make the same claim.

## Recommended order

| Priority | Add-on | Verdict | Why it can reduce compute | Required correctness boundary |
|---|---|---|---|---|
| 1 | Cross-machine command cache | **Valid; strongest next step** | Agents on different laptops, runners, or worktrees can reuse the same observed command result. | Remote action index plus content-addressed stdout/stderr blobs; repository, platform, toolchain, and environment fingerprints; authentication, tenant/repository namespaces, digest verification, poisoning controls, and global leases. |
| 2 | In-flight API/MCP request coalescing | **Valid now if session-scoped and allowlisted** | If parallel agents make the same read-only request simultaneously, one request runs and the others await that response. | Exact normalized request, auth/tenant scope, trusted read-only classification, short lifetime, and fail-open behavior. Do not infer safety from the HTTP method or an untrusted tool annotation alone. |
| 3 | Persistent API/MCP response cache | **Valid, but conditional** | Repeated searches, repository reads, and other expensive read-only calls can avoid network work and provider cost. | The provider's cache/freshness rules or an immutable version/snapshot ID; URI and relevant request headers; authorization scope; `Vary`; TTL/revalidation (`ETag` or equivalent); response status; and an explicit per-tool allowlist. A plain request hash is not enough. |
| 4 | Build-output/artifact cache | **Valid; proven pattern** | A matching compile, test-build, or container step can restore outputs instead of rebuilding them. | Declared inputs and outputs, command/toolchain/platform identity, a content-addressed artifact store, verified downloads, and safe restoration. Hindsight currently serves stdout/stderr only, so this is a new product surface. |
| 5 | Dependency-download cache | **Valid** | Package archives and module downloads can be shared when lockfiles and platform constraints match. | Package-manager-native cache or immutable artifact digests keyed by lockfile, runtime, OS, architecture, registry/source, and relevant build flags. |
| 6 | Restored dependency environment | **Conditional; defer** | Restoring a complete installed environment can save more time than caching downloads. | Recreate it from locked artifacts, or restore only into an identical container/path/runtime. Do not copy arbitrary virtual environments between machines: installed scripts can contain absolute interpreter paths. |
| 7 | Native file-read result cache | **Technically valid, low priority** | Repeated reads can be served by content hash. | File/content identity, permissions, byte ranges, encoding, and output formatting. Local filesystem reads are usually too cheap for this layer to beat the operating system's page cache. |
| 8 | Team-wide shared agent cache service | **Valid end state** | Any authorized agent harness can share command, artifact, and selected read-only tool results across a team. | This is not one universal key-value cache. It is a gateway over separate cache types, each with its own key and policy, plus auth, repository/tenant isolation, encryption, quotas, audit logs, retention, deletion, and trust levels. |

## Why the remote command cache is real

Established remote build systems separate an **action cache** (an action key to result metadata) from a **content-addressable store** (output blobs). Bazel's remote-cache protocol stores action results, output files, and stdout/stderr this way, and its action identity includes inputs such as the command and environment. That maps cleanly to Hindsight's observed-result design. See [Bazel remote caching](https://bazel.build/remote/caching) and the [Remote Execution API action schema](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).

For Hindsight, a remote hit cannot keep returning `cat ~/.hindsight/.../blob` paths from the machine that produced the result. The receiving machine must fetch blobs by digest, verify them, store them locally, then replay stdout, stderr, and the exit code separately. A global lease is also needed so two machines do not both execute the same miss.

## Why network/API caching needs a different key

HTTP caching is valid, including in shared caches, but freshness and representation selection are part of correctness. A cache key normally includes at least the method and target URI; `Vary`, authorization, freshness, and validation can change whether a stored response may be reused. See [RFC 9111: HTTP Caching](https://www.rfc-editor.org/rfc/rfc9111.html).

Therefore a Hindsight network layer should have two modes:

1. **Single-flight:** coalesce identical, explicitly read-only calls that are already in flight within one short-lived run.
2. **Persistent:** reuse only responses governed by provider cache headers or immutable snapshot/version identifiers, with authentication and tenant identity included in the scope.

MCP tool annotations can help discover candidates, but the protocol calls annotations hints and says clients must not trust them unless the server is trusted. `readOnlyHint` is not sufficient authorization to cache a tool. See the [MCP tools specification](https://modelcontextprotocol.io/specification/2025-06-18/server/tools).

## Build and dependency caching

Build artifacts are a legitimate larger target. Docker BuildKit supports importing and exporting external caches for CI and multi-machine reuse, while warning that secrets must not be put into cache layers. See [Docker cache backends](https://docs.docker.com/build/cache/backends/) and [Docker cache optimization](https://docs.docker.com/build/cache/optimize/).

Dependency caches are also established practice: keys commonly combine the runner platform with lockfile hashes, and fallback keys may intentionally restore a less exact cache that the package manager then updates. See [GitHub Actions dependency caching](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching). That fallback behavior is acceptable for dependency acceleration, but it is **not** an exact Hindsight replay and must be labeled separately.

Whole environment images are riskier. Python's documentation says virtual environments are generally non-portable because installed scripts contain absolute interpreter paths and recommends recreating them at the destination. See [Python `venv` documentation](https://docs.python.org/3/library/venv.html). Cache immutable downloads or container layers first; treat copying an environment as a constrained optimization, not a general feature.

## Explicit non-goals

Do not add any of these as exact servable records:

- mutating API calls such as deploys, messages, payments, writes, or deletes;
- unversioned live API responses with no freshness/revalidation contract;
- model answers, generated patches, simulator rollouts, or judgments;
- results classified as safe only because an untrusted server supplied a hint;
- blobs shared across users, repositories, or organizations without authorization and isolation;
- secrets, tokens, or private response data in globally addressable cache entries.

## Hackathon scope decision

For the current demo, finish and prove the local exact shell-result cache. If time remains, prototype **session-scoped read-only request coalescing** because it adds useful savings without claiming durable freshness. The next serious product milestone should be the cross-machine action cache/content-addressable store. Persistent API caching and a team-wide harness come after auth, namespacing, retention, and poisoning defenses are designed.
