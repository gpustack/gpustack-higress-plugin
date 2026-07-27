// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/c8b82797c51a97faca46e2ae12990453f5026802/plugins/wasm-go/extensions/ai-proxy/util/ptr.go
// Forked into gpustack/gpustack-higress-plugins at higress commit c8b82797c51a.
// Local modifications may diverge from upstream; keep this attribution when editing.

package util

func Ptr[T any](v T) *T {
	return &v
}
