package node

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSQLStoreListProfileNodes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var _ = db

	mock.ExpectQuery("SELECT n.id, n.name, n.node_type, n.source_type").
		WithArgs(int64(100)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "node_type", "source_type", "config_json", "secret_json"}).
			AddRow(int64(1), "Proxy", "http", "manual", []byte(`{"server":"proxy.example.com","port":8080}`), []byte(`{"password":"secret"}`)))

	nodes, err := NewSQLStore(db).ListProfileNodes(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, TypeHTTP, nodes[0].Type)
	require.Equal(t, "proxy.example.com", nodes[0].Config["server"])
	require.Equal(t, "secret", nodes[0].Secret["password"])
	require.NoError(t, mock.ExpectationsWereMet())
}
