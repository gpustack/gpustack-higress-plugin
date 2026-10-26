// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/36f2f1ac5c7305c5a46c296c617b45d1fbacb7d3/plugins/wasm-go/extensions/ai-proxy/util/ptr.go
// Forked into gpustack/gpustack-higress-plugins at higress commit 36f2f1ac5c73.
// Local modifications may diverge from upstream; keep this attribution when editing.

package util

func Ptr[T any](v T) *T {
	return &v
}
