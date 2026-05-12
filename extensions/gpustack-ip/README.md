# gpustack-ip Plugin

## Overview

`gpustack-ip` is a Higress Proxy-Wasm plugin that — for traffic destined to **trusted GPUStack upstreams** — injects:

1. The real client IP into a configurable request header (`realIPHeader`)
2. Any number of static headers via `header_add` (e.g. a pre-shared bearer token the backend uses to authenticate the gateway)

For non-matching clusters (e.g. third-party LLM providers reached via the proxy) the plugin is a no-op, so the trust headers and pre-shared tokens never leak to upstreams that are not part of the GPUStack control plane.

### Threat model

Within a Kubernetes cluster the gpustack-server HTTP port is typically reachable from any pod in the cluster. A malicious in-cluster workload could therefore bypass Higress and send forged `x-gpustack-real-ip` headers directly to gpustack-server. By pairing the IP header with a pre-shared token (`header_add: x-gpustack-internal-token: <secret>`) and validating the token on the server side, gpustack-server can distinguish requests that actually came through Higress from forged direct hits.

The pre-shared token is a long-lived static secret — anyone with read access to *both* the WasmPlugin config *and* the server-side validator config can forge requests. The token still raises the bar relative to plain network reachability and is sufficient when access to request-level traces / logs is kept narrower than access to configuration.

## Configuration

```yaml
# Optional: header that receives the resolved client IP.
realIPHeader: x-gpustack-real-ip

# Optional: static headers added to the request. Use this to ship a pre-shared
# token the backend can validate before trusting realIPHeader.
header_add:
  x-gpustack-internal-token: <shared-secret>

# Optional: extra regular expressions matched against the cluster_name FQDN.
# Added to the built-in defaults; does not replace them.
additionalClusterNameRegexps:
  - "^my-internal-svc(\\.|$)"
```

At least one of `realIPHeader` or `header_add` must be configured.

### Trusted cluster matching

The plugin reads the `cluster_name` property (Envoy form `outbound|<port>|<subset>|<fqdn>`), extracts the FQDN, and matches it against:

| Pattern                       | Matches                                                              |
| ----------------------------- | -------------------------------------------------------------------- |
| `^gpustack(-\|\.\|$)`         | `gpustack`, `gpustack-server`, `gpustack-*.<ns>.svc.cluster.local`…  |
| `^model-\d+-\d+(\.\|$)`       | GPUStack model instances (`model-<id>-<instance>[.suffix]`)          |
| `^provider-\d+(\.\|$)`        | GPUStack providers (`provider-<id>[.suffix]`)                        |

Headers are injected **only** when the FQDN matches one of these patterns or one of the `additionalClusterNameRegexps`. Every other upstream is passed through untouched.

## Phase ordering

Recommended phase: `UNSPECIFIED_PHASE`
Recommended priority: `400`

Constraints:

- **Must run after `model-mapper`** — the FQDN we match against reflects the post-mapping cluster selection, so model-mapper has to have finalised the upstream first.
- **Must run before `ext-auth`** (and any other plugin that consumes the injected headers) — ext-auth typically forwards request headers to an external auth service, which is one of the trust-header consumers.

`400` is a reasonable default for a Higress AI plugin chain, but adjust it to fit your actual plugin lineup (model-mapper's priority and ext-auth's phase/priority vary between deployments).

## Example

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: gpustack-ip
  namespace: higress-system
spec:
  defaultConfig:
    realIPHeader: x-gpustack-real-ip
    header_add:
      x-gpustack-internal-token: change-me
  defaultConfigDisable: false
  failStrategy: FAIL_OPEN
  phase: UNSPECIFIED_PHASE
  priority: 400
  url: http://localhost:8080/wasm-plugins/gpustack-ip/1.0.0/plugin.wasm
```

## Secret rotation

Static-token rotation requires a brief overlap window:

1. Update gpustack-server's validator to accept `<old-secret>` **and** `<new-secret>`.
2. Update this plugin's `header_add` to emit `<new-secret>`.
3. After all in-flight requests have drained (usually seconds), remove `<old-secret>` from the validator.

Both ends must be able to be reconfigured without restart for this to be non-disruptive.
