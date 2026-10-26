// GPUStack-local test (no upstream counterpart): guards the path-segment
// boundary of the base-path prefixing decisions.
//
// Upstream decided "already prefixed" with a byte prefix, so with basePath
// "/api" a normal "/api2/v1/models" was mistaken for an already-prefixed path,
// failed ValidateWithin and turned into a 400 instead of getting "/api"
// prepended (flagged by Copilot on PR #53; the same byte-prefix test in
// applyProviderBasePath silently skipped prefixing "/v2/models" under
// providerBasePath "/v").

package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrependBasePathDecidesAtSegmentBoundary(t *testing.T) {
	c := &ProviderConfig{basePath: "/api"}

	// A sibling sharing only the prefix spelling must be prepended, not 400'd.
	joined, err := c.prependBasePath("/api2/v1/models")
	require.NoError(t, err)
	assert.Equal(t, "/api/api2/v1/models", joined)

	// Already-prefixed paths pass through (query included).
	joined, err = c.prependBasePath("/api/v1/models?x=1")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/models?x=1", joined)

	// The path portion equal to the base itself is already prefixed.
	joined, err = c.prependBasePath("/api?x=1")
	require.NoError(t, err)
	assert.Equal(t, "/api?x=1", joined)

	// Dot segments climbing out of the base are still rejected.
	_, err = c.prependBasePath("/api/../v2")
	assert.Error(t, err)

	// Already under the base with dot segments resolving inside it stays valid.
	joined, err = c.prependBasePath("/api/a/../b")
	require.NoError(t, err)
	assert.Equal(t, "/api/a/../b", joined)

	// A single trailing slash on basePath names the same location, so an
	// already-prefixed path must survive the comparison against the trimmed
	// base instead of being prefixed twice.
	trailing := &ProviderConfig{basePath: "/api/"}
	joined, err = trailing.prependBasePath("/api/v1/models")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/models", joined)
	joined, err = trailing.prependBasePath("/api2/v1/models")
	require.NoError(t, err)
	assert.Equal(t, "/api/api2/v1/models", joined)
}

func TestApplyProviderBasePathDecidesAtSegmentBoundary(t *testing.T) {
	c := &ProviderConfig{providerBasePath: "/v"}

	assert.Equal(t, "/v/v2/models", c.applyProviderBasePath("/v2/models"))
	assert.Equal(t, "/v/chat/completions", c.applyProviderBasePath("/chat/completions"))
	assert.Equal(t, "/v/chat/completions", c.applyProviderBasePath("/v/chat/completions"))
	assert.Equal(t, "/v/plain", c.applyProviderBasePath("/plain"))

	// A trailing slash on providerBasePath must behave like its trimmed form.
	trailing := &ProviderConfig{providerBasePath: "/v/"}
	assert.Equal(t, "/v/chat/completions", trailing.applyProviderBasePath("/chat/completions"))
	assert.Equal(t, "/v/chat/completions", trailing.applyProviderBasePath("/v/chat/completions"))
	assert.Equal(t, "/v/v2/models", trailing.applyProviderBasePath("/v2/models"))
}
