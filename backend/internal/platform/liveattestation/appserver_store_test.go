package liveattestation

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAppServerAttestationStoreNegotiatesAndBindsToken(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-a", SessionID: "session-a", ThreadID: "thread-a"}

	capability, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	require.True(t, capability)

	request, requestID, err := store.BeginGenerate(key)
	require.NoError(t, err)
	require.NotEmpty(t, request)
	require.NotZero(t, requestID)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"jsonrpc":"2.0","id":1,"result":{"token":"v1.client-token"}}`)))

	header, ok := store.HeaderForRequest(key)
	require.True(t, ok)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-token"}`, header)
}

func TestAppServerAttestationStoreRejectsCrossAccountAndUnnegotiatedUse(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-a"}
	_, _, err := store.BeginGenerate(key)
	require.ErrorIs(t, err, ErrAttestationNotNegotiated)

	_, err = store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)

	other := SessionKey{AccountID: 8, ConnectionID: "conn-a"}
	err = store.AcceptGenerateResponse(other, []byte(`{"jsonrpc":"2.0","id":1,"result":{"token":"v1.client-token"}}`))
	require.ErrorIs(t, err, ErrAttestationRequestExpired)
	require.False(t, func() bool { _, ok := store.HeaderForRequest(other); return ok }())
}

func TestAppServerAttestationStoreExpiresPendingRequests(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	store.timeout = time.Millisecond
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	key := SessionKey{AccountID: 7, ConnectionID: "conn-a"}
	_, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	now = now.Add(2 * time.Millisecond)
	err = store.AcceptGenerateResponse(key, []byte(`{"jsonrpc":"2.0","id":1,"result":{"token":"v1.client-token"}}`))
	require.ErrorIs(t, err, ErrAttestationRequestExpired)
}

func TestAppServerAttestationStoreTreatsExactPendingDeadlineAsExpired(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	store.timeout = time.Millisecond
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	key := SessionKey{AccountID: 7, ConnectionID: "conn-exact-deadline"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	now = now.Add(time.Millisecond)
	err = store.AcceptGenerateResponse(key, []byte(`{"id":1,"result":{"token":"v1.client-token"}}`))
	require.ErrorIs(t, err, ErrAttestationRequestExpired)
}

func TestAppServerAttestationStoreRejectsNonIntegerResponseID(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-fractional-id"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	err = store.AcceptGenerateResponse(key, []byte(`{"id":1.0,"result":{"token":"v1.client-token"}}`))
	require.Error(t, err)
}

func TestAppServerAttestationStoreRejectsMismatchedResponseIDWithoutTreatingItAsExpiry(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-id-mismatch"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	err = store.AcceptGenerateResponse(key, []byte(`{"id":999,"result":{"token":"v1.client-token"}}`))
	require.ErrorIs(t, err, ErrAttestationResponseIDMismatch)
	// The pending request remains available for the matching response.
	err = store.AcceptGenerateResponse(key, []byte(`{"id":1,"result":{"token":"v1.client-token"}}`))
	require.NoError(t, err)
}

func TestAppServerAttestationStoreRejectsMismatchedResponseIDOnClientError(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-error-id"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, requestID, err := store.BeginGenerate(key)
	require.NoError(t, err)
	_, err = store.AcceptGenerateResponseWithHeader(key, []byte(`{"id":999,"error":{"code":-32000,"message":"client unavailable"}}`))
	require.ErrorIs(t, err, ErrAttestationResponseIDMismatch)

	// The original pending request remains consumable by the caller's failure
	// path, which must emit malformed-response status for the mismatched frame.
	header, err := store.RecordFailureWithHeaderForRequest(key, requestID, AttestationStatusMalformedResponse)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":4}`, header)
}

func TestAppServerAttestationStoreRejectsInvalidIDOnClientErrorResponse(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-error-invalid-id"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	err = store.AcceptGenerateResponse(key, []byte(`{"id":{},"error":{"code":-32000,"message":"client unavailable"}}`))
	require.Error(t, err)
	// The valid pending request is still available after rejecting the invalid
	// response frame and can be completed by the matching response.
	err = store.AcceptGenerateResponse(key, []byte(`{"id":1,"result":{"token":"v1.client-token"}}`))
	require.NoError(t, err)
}

func TestAppServerAttestationStoreDoesNotRetainUnnegotiatedCapabilityState(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-no-attestation"}
	capability, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":false}}}`))
	require.NoError(t, err)
	require.False(t, capability)
	_, _, err = store.BeginGenerate(key)
	require.ErrorIs(t, err, ErrAttestationNotNegotiated)
	// Re-initializing the same connection with the capability must still start
	// from a clean state and be allowed to negotiate normally.
	capability, err = store.ObserveInitialize(key, []byte(`{"id":2,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	require.True(t, capability)
}

func TestAppServerAttestationStorePurgesAtExactTTL(t *testing.T) {
	store := NewAppServerAttestationStore(time.Millisecond)
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	key := SessionKey{AccountID: 7, ConnectionID: "conn-exact-ttl"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	now = now.Add(time.Millisecond)
	_, _, err = store.BeginGenerate(key)
	require.ErrorIs(t, err, ErrAttestationNotNegotiated)
}

func TestAppServerAttestationStoreRecordsTimeoutAfterPendingDeadline(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	store.timeout = time.Millisecond
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	key := SessionKey{AccountID: 7, ConnectionID: "conn-timeout"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, requestID, err := store.BeginGenerate(key)
	require.NoError(t, err)
	now = now.Add(2 * time.Millisecond)
	header, err := store.RecordFailureWithHeaderForRequest(key, requestID, AttestationStatusTimeout)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":1}`, header)
}

func TestAppServerAttestationStoreLateFailureCannotOverwriteNewPendingRequest(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	store.timeout = time.Millisecond
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	key := SessionKey{AccountID: 7, ConnectionID: "conn-timeout-race"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, firstID, err := store.BeginGenerate(key)
	require.NoError(t, err)
	now = now.Add(2 * time.Millisecond)
	_, secondID, err := store.BeginGenerateWithTimeout(key, time.Minute)
	require.NoError(t, err)
	_, err = store.RecordFailureWithHeaderForRequest(key, firstID, AttestationStatusTimeout)
	require.ErrorIs(t, err, ErrAttestationRequestExpired)
	_, err = store.RecordFailureWithHeaderForRequest(key, secondID, AttestationStatusRequestFailed)
	require.NoError(t, err)
}

func TestAppServerAttestationStoreCustomPendingTimeout(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	key := SessionKey{AccountID: 7, ConnectionID: "remote-bridge"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerateWithTimeout(key, 500*time.Millisecond)
	require.NoError(t, err)
	now = now.Add(150 * time.Millisecond)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"id":1,"result":{"token":"v1.remote-token"}}`)))
	header, ok := store.HeaderForRequest(key)
	require.True(t, ok)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.remote-token"}`, header)
}

func TestAppServerAttestationStoreReturnsCommittedEnvelopeAtomically(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-atomic"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	envelope, err := store.AcceptGenerateResponseWithHeader(key, []byte(`{"id":1,"result":{"token":"opaque-token"}}`))
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":0,"t":"opaque-token"}`, envelope)
}

func TestAppServerAttestationStoreEscapesOpaqueTokenAsValidJSON(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 7, ConnectionID: "conn-json-escape"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	envelope, err := store.AcceptGenerateResponseWithHeader(key, []byte(`{"id":1,"result":{"token":"opaque\\\"token\\nline"}}`))
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":0,"t":"opaque\\\"token\\nline"}`, envelope)
}

func TestAppServerAttestationStoreRecordsOfficialFailureStatuses(t *testing.T) {
	for _, status := range []AttestationStatus{
		AttestationStatusTimeout,
		AttestationStatusRequestFailed,
		AttestationStatusRequestCanceled,
		AttestationStatusMalformedResponse,
	} {
		store := NewAppServerAttestationStore(time.Minute)
		key := SessionKey{AccountID: 7, ConnectionID: "conn-failure"}
		_, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
		require.NoError(t, err)
		_, _, err = store.BeginGenerate(key)
		require.NoError(t, err)
		require.NoError(t, store.RecordFailure(key, status))
		header, ok := store.HeaderForRequest(key)
		require.True(t, ok)
		require.Equal(t, fmt.Sprintf(`{"v":1,"s":%d}`, status), header)
	}
}

func TestSessionKeyRejectsOversizedComponents(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 1, ConnectionID: strings.Repeat("x", maxAttestationKeyComponentBytes+1)}
	_, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.ErrorIs(t, err, ErrInvalidSessionKey)
}

func TestSessionKeyRejectsControlCharacters(t *testing.T) {
	store := NewAppServerAttestationStore(time.Minute)
	key := SessionKey{AccountID: 1, ConnectionID: "conn\x00a"}
	_, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.ErrorIs(t, err, ErrInvalidSessionKey)
}
