package runtime

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSQLStoreSaveRuntimeStarting(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec("INSERT INTO clash_proxy_runtime_instances").
		WithArgs(int64(100), "mihomo", 17000, 18000, "secret-ref", "/tmp/config.yaml", "/tmp").
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = NewSQLStore(db).SaveRuntimeStarting(context.Background(), Instance{
		ProfileID:           100,
		RuntimeType:         "mihomo",
		MixedPort:           17000,
		ControllerPort:      18000,
		ControllerSecretRef: "secret-ref",
		ConfigPath:          "/tmp/config.yaml",
		WorkDir:             "/tmp",
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSQLStoreMarkRuntimeRunning(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec("UPDATE clash_proxy_runtime_instances").
		WithArgs(int64(100), 1234).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, NewSQLStore(db).MarkRuntimeRunning(context.Background(), 100, 1234))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSQLStoreMarkRuntimeStopped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec("UPDATE clash_proxy_runtime_instances").
		WithArgs(int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, NewSQLStore(db).MarkRuntimeStopped(context.Background(), 100))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSQLStoreMarkAllRunningStopped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec("UPDATE clash_proxy_runtime_instances").
		WillReturnResult(sqlmock.NewResult(0, 2))

	require.NoError(t, NewSQLStore(db).MarkAllRunningStopped(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())
}
