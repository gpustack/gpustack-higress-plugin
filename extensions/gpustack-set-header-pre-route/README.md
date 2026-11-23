# gpustack-set-header-pre-route Plugin


## Introduction

This plugin automatically injects the route name (route_name) and model name (cluster_name) into HTTP request headers before routing, based on the request path.
You can configure which path prefixes or suffixes should be processed, and customize the header names to be injected.
It is commonly used in model services and API gateway scenarios to help downstream services identify the request source and target model.

## Configuration

The plugin supports the following configuration options (YAML/JSON):

```yaml
routeNameHeader: "X-GPUStack-Route-Name" # Optional, specify which header to inject the route name
clusterNameHeader: "X-GPUStack-Model" # Optional, specify which header to inject the cluster name
enableOnPathSuffix: # Optional, list of URI path suffixes to process
  - "/completions",
  - "/embeddings",
  - "/images/generations",
  - "/audio/speech",
  - "/fine_tuning/jobs",
  - "/moderations",
  - "/image-synthesis",
  - "/video-synthesis",
  - "/rerank",
  - "/messages",
  - "/responses",
enableOnPathPrefix:
  - "/model/proxy"
```


## Implementation Notes

Design decisions and hazards (agent guidance, migrated from the
repository-level AGENTS.md — see that file for repo-wide policies).

Injects route/cluster name into request headers **before** routing (pre-route phase).

Config fields: `routeNameHeader`, `clusterNameHeader`, `enableOnPathSuffix`, `enableOnPathPrefix`
