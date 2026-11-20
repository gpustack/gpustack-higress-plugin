# Async video jobs

`gpustack-video` keeps OpenAI video jobs on the inference instance that created
them. The public ID is an opaque database identity. Uploads and video content
travel through Higress. GPUStack's API server receives task metadata, including
submitted parameters and upload descriptors, without uploaded media bytes.

The module runs twice in a filter chain:

| Mode | Phase / priority | Responsibility |
| --- | --- | --- |
| `resolve` | AUTHN / 805 | Capture caller credentials before upstream authentication rewrites; authorize the task owner and resolve its persisted target. |
| `transport` | UNSPECIFIED_PHASE / 50 | Confirm the selected endpoint, adapt MiniMax H3 inputs, rewrite native/public IDs, and confirm acceptance before sending response headers. |

The model router runs at AUTHN / 900, the header transformer at 810, and normal
route authorization, IP ACL and rate limits remain in the chain. The resolver
removes its bootstrap marker before those policies run. Task-read routes use
`cluster_header: x-gpustack-video-cluster` independently of the optional custom
LB plugin. Merely setting a target header does not establish pinning.

## Configuration

```yaml
enabled: false               # enabled only by dedicated ingress rules
mode: resolve                 # or transport
role: edge                    # or cluster
serviceName: gpustack.static  # registered central metadata service
servicePort: 80
gatewayToken: <derived gateway credential>
timeoutMs: 10000
```

Keep the WasmPlugin default configuration published (`defaultConfigDisable: false`)
with `enabled: false`, and set `enabled: true` on dedicated ingress rules. This
retains the shared control configuration without intercepting unrelated routes.
Per-ingress rules only enable this configuration. They cannot override its
credential, service or role. Limit activation to the dedicated create, task-read
and resolver-bootstrap ingresses. Normal inference APIs, including synchronous
video, retain their existing filters.

The Python integration can register a central control service for a cluster
gateway from server configuration:

```yaml
gateway_plugin:
  gpustack-video:
    config:
      role: cluster
      controlEndpoint: https://central-server.example
```

The downstream gateway must already route the deployed model names to the
corresponding GPUStack model-instance registries and authenticate the cluster's
registration credential. It uses the central control plane's Worker, model and
instance identities; this is not an adapter for an arbitrary external Provider.
Installing the updated modules and configuring this role is required at both
levels; an unmodified downstream gateway is not assumed to support video jobs.

## One gateway

1. The normal route chooses an endpoint, using custom LB or Envoy's default
   forwarding.
2. `POST /v2/video-tasks/gateway/prepare` authorizes the original user, tenant,
   API key and route, then records the actual model-instance registry, Worker,
   API port, runtime generation and submitted request parameters. The public
   model name is retained before native alias rewriting. File uploads retain
   filename, media type and size only; inline Base64 and credential fields are
   omitted. This snapshot does not infer unreported engine defaults or archive
   the referenced files.
3. Only the selected gateway converts `media_inputs` and H3 parameters and
   forwards the engine POST.
4. The response's native ID is recorded with `/accepted`. The response is held
   until that metadata write succeeds and its ID is replaced by the public ID.
5. Status/content reads resolve that public ID and validate the original runtime
   while generation is active. Completed managed outputs can use the original
   Worker's independent file service. The actual Envoy cluster must match the
   resolved binding before dispatch.
6. `DELETE /v1/videos/{id}` uses the same original-runtime binding. The final
   gateway records an uncertain deletion before dispatch, validates the engine's
   deletion response, and confirms a durable soft deletion before replying.

## Two gateways

The edge gateway initially selects the cluster gateway. It records a submission
intent with no claimed instance and issues a short-lived, signed internal grant.
The same public ID is used at both levels. A destination that cannot uniquely
identify the eligible cluster/model is rejected rather than guessed.

The cluster gateway captures the grant before removing its request header, then
performs its normal instance selection. `/v2/video-tasks/relay/bind` requires both
the grant and the matching cluster SYSTEM credential. It atomically claims the
intent, validates the selected model/Worker/runtime, and returns capabilities.
A second bind is rejected; it cannot release a second engine POST. The inner
gateway performs parameter adaptation and records `/relay/accepted`. The edge
confirms that durable acceptance before returning the response.

A task read resolves to the original cluster gateway first, then to the original
instance. The edge rechecks the original user's current access before issuing a
read grant; the cluster gateway requires that grant and its cluster credential.
Deletion uses a separate `delete` grant, rechecks current access before dispatch,
and confirms the same database tombstone at the edge. Read grants cannot authorize
deletion.
It never chooses a replacement instance for an existing task. The model router
clears caller-supplied pinning/relay headers at the edge. Only `role: cluster`
accepts a relay header for verification by the control plane.

Internal grants expire after five minutes. This lifetime does not expire the
public task ID or authorize subsequent requests without a new access check.

## Transfer and retry boundaries

- Disable Envoy/Ingress retries and fallback on video creation routes at both
  levels. Strip client retry/hedge override headers. The AI proxy must also skip
  provider retries for async video creation, status and content APIs.
- A lost acceptance response means the submission is unconfirmed. Do not retry
  the engine POST on the same or a different instance. Metadata confirmation is
  idempotent; submission is not.
- Only small create/status/delete responses and content errors are buffered.
  Successful `/content` responses remain a byte stream. The AI proxy also bypasses JSON/SSE body handling for content.
- Do not transparently restart a download after bytes have been sent. Broken
  transfers must remain detectable by the client.
- The native engine's collection endpoint is not exposed as a GPUStack history
  list. File archival and independent Worker file cleanup are separate capabilities.

## Deletion semantics

Both vLLM-Omni's `deleted: true` response and SGLang's `status: deleted` response
require durable confirmation from the control plane. The public result reports
`record_deleted` and `file_cleanup` separately. Managed vLLM files must be confirmed
missing before claiming `file_cleanup: deleted`; unverified cleanup is
`unconfirmed`. SGLang output cleanup remains unsupported and reports `retained`.
Engine acknowledgement does not prove that GPU execution stopped immediately.

`DELETE /v1/videos/{id}?keep_task=true` retains history and returns its committed
final status. A completion verified before cancellation confirmation remains
`completed`. `DELETE /v2/video-tasks/{id}` independently hides owned history,
returns `file_cleanup: not_requested`, and does not require a reachable Worker,
engine or file. Removing active history does not stop generation.

Failed or uncertain native deletion retains its binding until the caller explicitly
removes history. The UI may independently remove that history and report cleanup
as unconfirmed. No automatic native deletion replay or replica failover is enabled.
Tombstone retention and independent Worker file cleanup remain separate policies.

## Tests

`go test ./extensions/gpustack-video/...` covers request adaptation, ownership
protocol boundaries, ID handling, endpoint checks and binary pass-through.
The opt-in Envoy integration test exercises real modules with local fake engines
and a mocked metadata service, without a GPU or real database. It covers custom
LB on/off at both gateway levels, one submission after a dropped response,
status/content pinning, uploads and streamed content. GPUStack's Python tests
separately exercise tenant/key checks, grants, durable state and portable schema
migration.

Run the opt-in integration check with Go 1.24.4, Python 3 and a Higress-compatible
Envoy binary (including the Proxy-Wasm and WASI host extensions):

```sh
export VIDEO_GATEWAY_TEST_DIR="$(mktemp -d)"
mkdir "$VIDEO_GATEWAY_TEST_DIR/wasm"
export GOOS=wasip1 GOARCH=wasm
go build -buildmode=c-shared -o "$VIDEO_GATEWAY_TEST_DIR/wasm/video.wasm" ./extensions/gpustack-video
go build -buildmode=c-shared -o "$VIDEO_GATEWAY_TEST_DIR/wasm/router.wasm" ./extensions/gpustack-generic-proxy-router
go build -buildmode=c-shared -o "$VIDEO_GATEWAY_TEST_DIR/wasm/pre-route.wasm" ./extensions/gpustack-set-header-pre-route
go build -buildmode=c-shared -o "$VIDEO_GATEWAY_TEST_DIR/wasm/lb.wasm" ./extensions/gpustack-lb
go build -buildmode=c-shared -o "$VIDEO_GATEWAY_TEST_DIR/wasm/ai-proxy.wasm" ./extensions/gpustack-ai-proxy
ENVOY_BINARY=/path/to/envoy python3 scripts/test_video_gateway.py
```

The script binds loopback ports 17800–17805. Two listener/filter chains model the
edge and cluster gateways in one Envoy process. Its metadata service is mocked;
it does not replace backend authorization tests or a deployment acceptance test.
Configuration and Envoy logs remain in `VIDEO_GATEWAY_TEST_DIR` for inspection.


## Managed outputs and task cards

The control plane persists status observations and serves the caller-scoped
`GET /v2/video-tasks` history. Engine status observations carry only trusted
native/public identity and generation metadata; public responses omit native
storage paths. The Playground synchronizes on entry, refresh and selection,
with task cards on the left and a prompt/Submit composer on the right. Cards own Stop/Delete actions.
There is no background Server polling loop.

New deployments with managed output directories can read completed MP4 files
from a stable Worker service even after the inference runtime is removed.
The resolver returns either the original runtime, or a scoped Worker file
binding. Cached completed status is emitted in transport mode, after the
original route policies have run. File bytes remain a stream through Higress;
only the selected task is downloaded by the Playground.

The Worker registry is independent of instance registries and uses the Worker
API port. Tunnel-mode origins use the Worker's tunnel-visible IP and an HTTP
proxy registry. File grants are short-lived and scoped to task, instance,
native storage key and backend. The Worker only reads its managed output
root, rejects path traversal and symlink outputs, checks the MP4 structure,
and supports Range requests.

Native DELETE semantics remain unchanged. SGLang's retained output cleanup is
not implemented. Engine TTL stays opt-in; GPUStack does not impose a 24-hour
retention period. Custom output directories retain native access semantics;
old unmanaged files are not automatically imported.

## Retained cancellation records

`DELETE /v1/videos/{id}?keep_task=true` reuses native DELETE through the original binding. The final gateway strips the GPUStack query flag before forwarding, records the intent, and confirms cancellation metadata before returning `status: cancelled` and `record_only_delete: true`. The record remains visible. The edge validates the same retained confirmation on two-gateway requests. Uncertain outcomes are not labelled cancelled and are not retried automatically.

`DELETE /v2/video-tasks/{id}` removes a retained cancelled record only after native deletion is confirmed and the current owner, tenant and model access checks pass. It does not send another engine DELETE. Ordinary native task deletion retains its existing semantics. Cancellation acknowledgement does not guarantee immediate GPU termination.

The Playground uses one five-second list scheduler for queued/in-progress rows, bounded per-task status requests, and manual retry after a failed row query. Completed rows are not polled and only the selected completed video downloads. Cursor pagination loads near the scroll boundary; card columns use the same core-ui ResizeContainer as Catalog. The parameter form always renders aspect ratio and a Select for resolution, including one-option defaults.
