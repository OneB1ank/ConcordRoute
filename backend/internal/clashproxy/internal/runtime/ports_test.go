package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPortAllocatorUsesConfiguredRanges(t *testing.T) {
	allocator := &PortAllocator{
		mixedStart:      17000,
		mixedEnd:        17001,
		controllerStart: 18000,
		controllerEnd:   18001,
		isFree: func(port int) bool {
			return port != 17000
		},
	}
	mixed, controller, err := allocator.AllocatePair()
	require.NoError(t, err)
	require.Equal(t, 17001, mixed)
	require.Equal(t, 18000, controller)
}
