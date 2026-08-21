package clashproxy

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	proxyruntime "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/runtime"
	"github.com/stretchr/testify/require"
)

type selectiveAccountProxyUpdater struct {
	failAccountID int64
	updates       []recordedAccountProxyUpdate
}

func (u *selectiveAccountProxyUpdater) SetAccountProxy(_ context.Context, accountID int64, proxyID *int64) error {
	if accountID == u.failAccountID {
		return errors.New("test update failure")
	}
	var copied *int64
	if proxyID != nil {
		value := *proxyID
		copied = &value
	}
	u.updates = append(u.updates, recordedAccountProxyUpdate{accountID: accountID, proxyID: copied})
	return nil
}

func TestBindUnboundOpenAIOAuthAccountsBindsEligibleAccountsAndReportsPartialFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	updater := &selectiveAccountProxyUpdater{failAccountID: 78}
	service := &Service{
		db:             db,
		accountUpdater: updater,
		runtimeStore:   proxyruntime.NewSQLStore(db),
	}

	mock.ExpectQuery("SELECT id, profile_id, runtime_type").
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "profile_id", "runtime_type", "pid", "mixed_port", "controller_port",
			"controller_secret_ref", "config_path", "work_dir", "status", "last_error",
		}).AddRow(int64(1), int64(11), "mihomo", 1234, 17000, 17001, "secret", "config", "work", "running", ""))
	mock.ExpectQuery("SELECT managed_proxy_id FROM clash_proxy_profiles").
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"managed_proxy_id"}).AddRow(int64(91)))
	mock.ExpectQuery("SELECT id FROM accounts WHERE deleted_at IS NULL AND parent_account_id IS NULL.+proxy_id IS NULL").
		WithArgs(bulkBindingOpenAIPlatform, bulkBindingOAuthType).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(77)).AddRow(int64(78)))

	expectBulkBindingAccount(mock, 77, 11, 91, 1001, true)
	expectBulkBindingAccount(mock, 78, 11, 91, 1002, false)

	result, err := service.BindUnboundOpenAIOAuthAccounts(context.Background(), 11)
	require.NoError(t, err)
	require.Equal(t, &BulkBindOpenAIOAuthResult{
		ProfileID: 11,
		Eligible:  2,
		Bound:     1,
		Failed:    1,
		Failures: []BulkBindOpenAIOAuthFailure{{
			AccountID: 78,
			Reason:    "test update failure",
		}},
	}, result)
	require.Equal(t, []recordedAccountProxyUpdate{{accountID: 77, proxyID: int64Pointer(91)}}, updater.updates)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBindUnboundOpenAIOAuthAccountsRequiresRunningProfile(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	service := &Service{
		db:             db,
		accountUpdater: noopAccountProxyUpdater{},
		runtimeStore:   proxyruntime.NewSQLStore(db),
	}
	mock.ExpectQuery("SELECT id, profile_id, runtime_type").
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "profile_id", "runtime_type", "pid", "mixed_port", "controller_port",
			"controller_secret_ref", "config_path", "work_dir", "status", "last_error",
		}).AddRow(int64(1), int64(11), "mihomo", 0, 17000, 17001, "secret", "config", "work", "stopped", ""))

	result, err := service.BindUnboundOpenAIOAuthAccounts(context.Background(), 11)
	require.Nil(t, result)
	require.ErrorContains(t, err, "start the proxy profile")
	require.NoError(t, mock.ExpectationsWereMet())
}

func expectBulkBindingAccount(mock sqlmock.Sqlmock, accountID, profileID, proxyID, bindingID int64, success bool) {
	mock.ExpectQuery("SELECT proxy_id FROM accounts").
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{"proxy_id"}).AddRow(nil))
	mock.ExpectQuery("SELECT b.id, b.account_id").
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "account_name", "account_platform", "profile_id", "profile_name", "previous_proxy_id", "enabled",
		}))
	mock.ExpectQuery("INSERT INTO clash_proxy_account_bindings").
		WithArgs(accountID, profileID, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(bindingID))
	if !success {
		mock.ExpectExec("DELETE FROM clash_proxy_account_bindings WHERE account_id").
			WithArgs(accountID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		return
	}
	mock.ExpectQuery("SELECT b.id, b.account_id").
		WithArgs(bindingID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "account_name", "account_platform", "profile_id", "profile_name", "previous_proxy_id", "enabled",
		}).AddRow(bindingID, accountID, "account", bulkBindingOpenAIPlatform, profileID, "profile", nil, true))
}
