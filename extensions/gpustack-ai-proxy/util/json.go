// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/aae6fbce36a2d1dd7afff007a265ecbebdd8a6f1/plugins/wasm-go/extensions/ai-proxy/util/json.go
// Forked into gpustack/gpustack-higress-plugins at higress commit aae6fbce36a2.
// Local modifications may diverge from upstream; keep this attribution when editing.

package util

import (
	"strconv"
	"strings"
)

func EscapeStringForJson(s string) string {
	var builder strings.Builder
	for _, c := range s { //iterate through rune
		switch c {
		case '"':
			builder.WriteRune('\\')
			builder.WriteRune(c)
			break
		default:
			quoted := strconv.QuoteRune(c)
			builder.WriteString(quoted[1 : len(quoted)-1])
		}
	}
	return builder.String()
}
