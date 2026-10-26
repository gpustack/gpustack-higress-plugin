// GPUStack-local test (no upstream counterpart): guards the object-start gate
// of DuplicateTopLevelMember.
//
// Upstream started counting at the first '{' anywhere in the body, so a
// multipart image-edit/variation form whose prompt field mentions JSON-looking
// text was scanned as if it were a JSON object and could be rejected as a
// duplicate model member (flagged by Copilot on PR #53).

package jsonbody

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDuplicateTopLevelMemberIgnoresNonObjectBodies(t *testing.T) {
	// A multipart form with one model field and a prompt quoting JSON twice:
	// not a JSON object at all, so no verdict may be produced.
	multipart := []byte("--boundary\r\n" +
		"Content-Disposition: form-data; name=\"model\"\r\n\r\n" +
		"gpt-image-1\r\n" +
		"--boundary\r\n" +
		"Content-Disposition: form-data; name=\"prompt\"\r\n\r\n" +
		"Compare {\"model\":\"a\"} and {\"model\":\"b\"}\r\n" +
		"--boundary--\r\n")
	duplicate, err := DuplicateTopLevelMember(multipart, "model")
	require.NoError(t, err)
	assert.False(t, duplicate, "a multipart body must never be read as a JSON object")

	// A JSON array has no top-level members either.
	duplicate, err = DuplicateTopLevelMember([]byte(`[{"model":"a"},{"model":"b"}]`), "model")
	require.NoError(t, err)
	assert.False(t, duplicate)

	// Leading whitespace before the object is still an object.
	duplicate, err = DuplicateTopLevelMember([]byte("  \n{\"model\":\"a\",\"model\":\"b\"}"), "model")
	require.NoError(t, err)
	assert.True(t, duplicate)

	// An empty body carries no verdict.
	duplicate, err = DuplicateTopLevelMember(nil, "model")
	require.NoError(t, err)
	assert.False(t, duplicate)
}
