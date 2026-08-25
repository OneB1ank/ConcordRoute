//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMapUpstreamStatusCodeMapsTransient429To503(t *testing.T) {
	t.Parallel()

	require.Equal(t, http.StatusServiceUnavailable, mapUpstreamStatusCode(http.StatusTooManyRequests))
	require.Equal(t, http.StatusBadGateway, mapUpstreamStatusCode(http.StatusBadGateway))
	require.Equal(t, http.StatusBadGateway, mapUpstreamStatusCode(http.StatusInternalServerError))
	require.Equal(t, http.StatusUnauthorized, mapUpstreamStatusCode(http.StatusUnauthorized))
}
