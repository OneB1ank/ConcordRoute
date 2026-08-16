package profile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProfileValidateActive(t *testing.T) {
	require.NoError(t, Profile{ID: 1, Strategy: StrategyURLTest, Status: StatusActive}.ValidateActive())
	require.ErrorContains(t, Profile{ID: 0, Strategy: StrategySelect, Status: StatusActive}.ValidateActive(), "id is required")
	require.ErrorContains(t, Profile{ID: 1, Strategy: Strategy("bad"), Status: StatusActive}.ValidateActive(), "strategy")
	require.ErrorContains(t, Profile{ID: 1, Strategy: StrategySelect, Status: StatusDisabled}.ValidateActive(), "not active")
}
