package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexTurnStateRequestModeForPlan(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	mode := codexTurnStateRequestModeForPlan(account, openAICodex292RequestPlan{
		enabled:   true,
		usedState: true,
	})
	require.NotNil(t, mode)
	require.Equal(t, CodexTurnStateRequestModeInjected, *mode)

	mode = codexTurnStateRequestModeForPlan(account, openAICodex292RequestPlan{enabled: true})
	require.NotNil(t, mode)
	require.Equal(t, CodexTurnStateRequestModeAcquire, *mode)

	mode = codexTurnStateRequestModeForPlan(account, openAICodex292RequestPlan{})
	require.NotNil(t, mode)
	require.Equal(t, CodexTurnStateRequestModeDisabled, *mode)

	require.Nil(t, codexTurnStateRequestModeForPlan(&Account{Platform: PlatformAnthropic}, openAICodex292RequestPlan{}))
}
