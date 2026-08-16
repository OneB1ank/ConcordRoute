package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type recordingInstanceStore struct {
	failedMessage string
	stopped       bool
}

func (s *recordingInstanceStore) GetRunningRuntime(context.Context, int64) (*Instance, error) {
	return nil, nil
}

func (s *recordingInstanceStore) GetRuntime(context.Context, int64) (*Instance, error) {
	return nil, nil
}

func (s *recordingInstanceStore) SaveRuntimeStarting(context.Context, Instance) error {
	return nil
}

func (s *recordingInstanceStore) MarkRuntimeRunning(context.Context, int64, int) error {
	return nil
}

func (s *recordingInstanceStore) MarkRuntimeFailed(_ context.Context, _ int64, message string) error {
	s.failedMessage = message
	return nil
}

func (s *recordingInstanceStore) MarkRuntimeStopped(context.Context, int64) error {
	s.stopped = true
	return nil
}

func TestRecordProcessExitRestoresBindingsBeforeMarkingFailure(t *testing.T) {
	store := &recordingInstanceStore{}
	cleanupCalled := false
	manager := &ProcessManager{
		Instances: store,
		OnUnexpectedExit: func(context.Context, int64, error) error {
			cleanupCalled = true
			return errors.New("cleanup failed")
		},
	}

	manager.recordProcessExit(7, false, errors.New("exit status 1"))

	require.True(t, cleanupCalled)
	require.Contains(t, store.failedMessage, "exit status 1")
	require.Contains(t, store.failedMessage, "cleanup failed")
	require.False(t, store.stopped)
}

func TestRecordProcessExitDuringStopSkipsUnexpectedCleanup(t *testing.T) {
	store := &recordingInstanceStore{}
	manager := &ProcessManager{
		Instances: store,
		OnUnexpectedExit: func(context.Context, int64, error) error {
			t.Fatal("unexpected cleanup callback")
			return nil
		},
	}

	manager.recordProcessExit(8, true, errors.New("signal: killed"))

	require.True(t, store.stopped)
	require.True(t, strings.TrimSpace(store.failedMessage) == "")
}
