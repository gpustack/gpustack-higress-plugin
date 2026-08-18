// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/aae6fbce36a2d1dd7afff007a265ecbebdd8a6f1/plugins/wasm-go/extensions/ai-proxy/util/json_test.go
// Forked into gpustack/gpustack-higress-plugins at higress commit aae6fbce36a2.
// Local modifications may diverge from upstream; keep this attribution when editing.

package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEscapeForJsonString(t *testing.T) {
	var tests = []struct {
		input, output string
	}{
		{"hello", "hello"},
		{"hello\"world", "hello\\\"world"},
		{"h\be\vl\tlo\rworld\n", "h\\be\\vl\\tlo\\rworld\\n"},
	}

	for _, tt := range tests {
		// t.Run enables running "subtests", one for each
		// table entry. These are shown separately
		// when executing `go test -v`.
		testName := tt.input
		t.Run(testName, func(t *testing.T) {
			output := EscapeStringForJson(tt.input)
			assert.Equal(t, tt.output, output)
		})
	}
}
