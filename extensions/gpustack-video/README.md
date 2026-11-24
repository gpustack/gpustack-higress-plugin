# GPUStack video gateway adapter

The Video Playground uses four OpenAI-compatible operations through Higress:

| Method | Path |
| --- | --- |
| POST | `/v1/videos` |
| GET | `/v1/videos/{id}` |
| GET | `/v1/videos/{id}/content` |
| DELETE | `/v1/videos/{id}` |

Creation uses the model in the request body. Follow-up calls send the engine's
ID and `X-GPUStack-Model` with the same model route. The model router projects
the model into the existing routing header before authentication and load
balancing. The gateway forwards the engine's responses, including video bytes.

The adapter runs after model selection. Per-ingress `targets` are keyed by
Envoy cluster name. Each target can provide the engine's served model name
and MiniMax H3 request capabilities. For a remote cluster gateway the adapter
passes its model route on, and H3 conversion happens at the serving gateway.

## Implementation notes

The asynchronous video ingresses share the model route's authentication, mapping
and load balancing. A supporting engine can also serve `POST /v1/videos/sync`
through the ordinary model ingress and return video bytes directly.

The model router clears retry overrides for video requests. The adapter processes
asynchronous creation bodies and does not buffer response bodies. Engine-specific request
profiles handle JSON and multipart inputs, including the structured MiniMax
H3 `media_inputs` fields.
