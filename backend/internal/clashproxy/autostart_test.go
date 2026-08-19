package clashproxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	proxyprofile "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/profile"
	proxyruntime "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/runtime"
	"github.com/stretchr/testify/require"
)

type fakeProfileRuntimeManager struct {
	ensure func(proxyprofile.Profile) (*proxyruntime.Instance, error)
	starts []int64
	stops  []int64
}

func (m *fakeProfileRuntimeManager) EnsureRunning(_ context.Context, profile proxyprofile.Profile) (*proxyruntime.Instance, error) {
	m.starts = append(m.starts, profile.ID)
	if m.ensure != nil {
		return m.ensure(profile)
	}
	return &proxyruntime.Instance{ProfileID: profile.ID, MixedPort: 17000, Status: proxyruntime.StatusRunning}, nil
}

func (m *fakeProfileRuntimeManager) Stop(profileID int64) error {
	m.stops = append(m.stops, profileID)
	return nil
}

func (m *fakeProfileRuntimeManager) SetControllerHTTPClient(*http.Client) {}

type noopAccountProxyUpdater struct{}

func (noopAccountProxyUpdater) SetAccountProxy(context.Context, int64, *int64) error { return nil }

type recordedAccountProxyUpdate struct {
	accountID int64
	proxyID   *int64
}

type recordingAccountProxyUpdater struct {
	updates []recordedAccountProxyUpdate
}

func (u *recordingAccountProxyUpdater) SetAccountProxy(_ context.Context, accountID int64, proxyID *int64) error {
	var copied *int64
	if proxyID != nil {
		value := *proxyID
		copied = &value
	}
	u.updates = append(u.updates, recordedAccountProxyUpdate{accountID: accountID, proxyID: copied})
	return nil
}

func newAutoStartTestService(t *testing.T) (*Service, sqlmock.Sqlmock, *fakeProfileRuntimeManager) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	manager := &fakeProfileRuntimeManager{}
	return &Service{
		db:             db,
		options:        Options{Enabled: true},
		accountUpdater: noopAccountProxyUpdater{},
		runtimeManager: manager,
		runtimeStore:   proxyruntime.NewSQLStore(db),
	}, mock, manager
}

func expectProfileModel(mock sqlmock.Sqlmock, profileID int64) {
	mock.ExpectQuery("SELECT id, name, strategy, test_url, interval_seconds, status, auto_start, config_json").
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "strategy", "test_url", "interval_seconds", "status", "auto_start", "config_json"}).
			AddRow(profileID, "profile", "fallback", defaultTestURL, 300, "active", true, []byte(`{}`)))
}

func expectExistingManagedProxy(mock sqlmock.Sqlmock, profileID, proxyID int64) {
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT managed_proxy_id FROM clash_proxy_profiles").
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{"managed_proxy_id"}).AddRow(proxyID))
	mock.ExpectExec("UPDATE proxies").
		WithArgs(proxyID, "[Clash] profile", 17000).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT account_id FROM clash_proxy_account_bindings").
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}))
}

func expectStoppedRuntime(mock sqlmock.Sqlmock, profileID int64) {
	mock.ExpectQuery("SELECT id, profile_id, runtime_type").
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "profile_id", "runtime_type", "pid", "mixed_port", "controller_port", "controller_secret_ref", "config_path", "work_dir", "status", "last_error"}))
}

func TestStartProfileKeepsConfiguredAutoStartSetting(t *testing.T) {
	service, mock, manager := newAutoStartTestService(t)
	expectProfileModel(mock, 1)
	expectExistingManagedProxy(mock, 1, 91)

	runtime, err := service.StartProfile(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, "running", runtime.Status)
	require.Equal(t, []int64{1}, manager.starts)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStopProfileKeepsConfiguredAutoStartSetting(t *testing.T) {
	service, mock, manager := newAutoStartTestService(t)
	mock.ExpectExec("UPDATE proxies").
		WithArgs(int64(1), "disabled").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectStoppedRuntime(mock, 1)

	runtime, err := service.StopProfile(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, "stopped", runtime.Status)
	require.Equal(t, []int64{1}, manager.stops)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateProfilePersistsAutoStartSetting(t *testing.T) {
	service, mock, _ := newAutoStartTestService(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("INSERT INTO clash_proxy_profiles").
		WithArgs("profile", "fallback", defaultTestURL, 300, true).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(11)))
	mock.ExpectExec("DELETE FROM clash_proxy_profile_nodes").
		WithArgs(int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO clash_proxy_profile_nodes").
		WithArgs(int64(11), int64(7), 0, 1).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id, name, strategy, test_url, interval_seconds, status, auto_start").
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "strategy", "test_url", "interval_seconds", "status", "auto_start", "config_json", "managed_proxy_id"}).
			AddRow(int64(11), "profile", "fallback", defaultTestURL, 300, "active", true, []byte(`{}`), nil))
	mock.ExpectQuery("SELECT node_id FROM clash_proxy_profile_nodes").
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}).AddRow(int64(7)))

	profile, err := service.CreateProfile(context.Background(), CreateProfileInput{
		Name:      "profile",
		Strategy:  "fallback",
		AutoStart: true,
		NodeIDs:   []int64{7},
	})
	require.NoError(t, err)
	require.True(t, profile.AutoStart)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateProfilePersistsDisabledAutoStartSetting(t *testing.T) {
	service, mock, _ := newAutoStartTestService(t)
	expectStoppedRuntime(mock, 11)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE clash_proxy_profiles").
		WithArgs(int64(11), "profile", "fallback", defaultTestURL, 300, false).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM clash_proxy_profile_nodes").
		WithArgs(int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO clash_proxy_profile_nodes").
		WithArgs(int64(11), int64(7), 0, 1).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id, name, strategy, test_url, interval_seconds, status, auto_start").
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "strategy", "test_url", "interval_seconds", "status", "auto_start", "config_json", "managed_proxy_id"}).
			AddRow(int64(11), "profile", "fallback", defaultTestURL, 300, "active", false, []byte(`{}`), nil))
	mock.ExpectQuery("SELECT node_id FROM clash_proxy_profile_nodes").
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}).AddRow(int64(7)))

	profile, err := service.UpdateProfile(context.Background(), 11, UpdateProfileInput{
		Name:      "profile",
		Strategy:  "fallback",
		AutoStart: false,
		NodeIDs:   []int64{7},
	})
	require.NoError(t, err)
	require.False(t, profile.AutoStart)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClosePreservesAutoStartIntent(t *testing.T) {
	service, mock, manager := newAutoStartTestService(t)
	mock.ExpectQuery("SELECT id FROM clash_proxy_profiles WHERE deleted_at IS NULL").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectExec("UPDATE proxies").
		WithArgs(int64(1), "disabled").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectStoppedRuntime(mock, 1)

	require.NoError(t, service.Close())
	require.Equal(t, []int64{1}, manager.stops)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReconcileStartupContinuesAfterOneProfileFails(t *testing.T) {
	service, mock, manager := newAutoStartTestService(t)
	manager.ensure = func(profile proxyprofile.Profile) (*proxyruntime.Instance, error) {
		if profile.ID == 1 {
			return nil, errors.New("test start failure")
		}
		return &proxyruntime.Instance{ProfileID: profile.ID, MixedPort: 17000, Status: proxyruntime.StatusRunning}, nil
	}
	mock.ExpectExec("UPDATE clash_proxy_runtime_instances").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery("SELECT id FROM clash_proxy_profiles WHERE managed_proxy_id IS NOT NULL").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("SELECT id.+FROM clash_proxy_profiles.+WHERE auto_start = TRUE").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
	expectProfileModel(mock, 1)
	expectProfileModel(mock, 2)
	expectExistingManagedProxy(mock, 2, 92)

	err := service.ReconcileStartup(context.Background())
	require.ErrorContains(t, err, "auto-start clash proxy profile 1")
	require.Equal(t, []int64{1, 2}, manager.starts)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReconcileStartupKeepsBoundAccountOnManagedProxy(t *testing.T) {
	service, mock, _ := newAutoStartTestService(t)
	updater := &recordingAccountProxyUpdater{}
	service.accountUpdater = updater

	mock.ExpectExec("UPDATE clash_proxy_runtime_instances").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id FROM clash_proxy_profiles WHERE managed_proxy_id IS NOT NULL").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectExec("UPDATE proxies").
		WithArgs(int64(1), "disabled").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT managed_proxy_id FROM clash_proxy_profiles").
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"managed_proxy_id"}).AddRow(int64(91)))
	mock.ExpectQuery("SELECT account_id FROM clash_proxy_account_bindings").
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(int64(77)))
	mock.ExpectQuery("SELECT id.+FROM clash_proxy_profiles.+WHERE auto_start = TRUE").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	require.NoError(t, service.ReconcileStartup(context.Background()))
	require.Equal(t, []recordedAccountProxyUpdate{{accountID: 77, proxyID: int64Pointer(91)}}, updater.updates)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReconcileStartupDoesNotAssignProxyWithoutBinding(t *testing.T) {
	service, mock, _ := newAutoStartTestService(t)
	updater := &recordingAccountProxyUpdater{}
	service.accountUpdater = updater

	mock.ExpectExec("UPDATE clash_proxy_runtime_instances").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id FROM clash_proxy_profiles WHERE managed_proxy_id IS NOT NULL").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectExec("UPDATE proxies").
		WithArgs(int64(1), "disabled").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT managed_proxy_id FROM clash_proxy_profiles").
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"managed_proxy_id"}).AddRow(int64(91)))
	mock.ExpectQuery("SELECT account_id FROM clash_proxy_account_bindings").
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}))
	mock.ExpectQuery("SELECT id.+FROM clash_proxy_profiles.+WHERE auto_start = TRUE").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	require.NoError(t, service.ReconcileStartup(context.Background()))
	require.Empty(t, updater.updates)
	require.NoError(t, mock.ExpectationsWereMet())
}

func int64Pointer(value int64) *int64 {
	return &value
}
