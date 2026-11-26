package addresses

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// sortBy reaches raw SQL, so the whitelist and its fallback are the only
// thing standing between an admin query string and injection.
func TestOrderByClauseWhitelistsColumns(t *testing.T) {
	tests := []struct {
		sortBy, sortDir, want string
	}{
		{"credited_total", "desc", " ORDER BY COALESCE(d.credited, 0) DESC, a.id DESC"},
		{"deposit_count", "asc", " ORDER BY COALESCE(d.cnt, 0) ASC, a.id DESC"},
		// Injections and unknowns collapse to the stable default.
		{"id; DROP TABLE deposit_addresses", "asc", " ORDER BY a.id ASC, a.id DESC"},
		{"", "", " ORDER BY a.id DESC, a.id DESC"},
		{"1=1 UNION SELECT", "asc", " ORDER BY a.id ASC, a.id DESC"},
	}
	for _, tt := range tests {
		if got := orderByClause(tt.sortBy, tt.sortDir); got != tt.want {
			t.Errorf("orderByClause(%q, %q) = %q, want %q", tt.sortBy, tt.sortDir, got, tt.want)
		}
	}
}

// q builds a regexp that matches the whitespace-normalised SQL sqlmock sees.
func q(sql string) string { return regexp.QuoteMeta(sql) }

// After a rotation the user has one disabled history row and one active row.
// The lookup must hand the active one to GetOrAssign, never the retired one.
func TestExistingAssignmentPrefersActiveRowAfterRotation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(q(`SELECT id, user_id, chain, address, derivation_index, status
		FROM deposit_addresses WHERE user_id = $1 AND chain = $2
		ORDER BY (status = 'active') DESC, id DESC
		LIMIT 1`)).
		WithArgs(int64(8624), FamilyEVM).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "chain", "address", "derivation_index", "status"}).
			AddRow(9001, 8624, "evm", "0xnewaddress", 400, "active"))

	got, err := NewRepo(db).ExistingAssignment(context.Background(), 8624, FamilyEVM)
	require.NoError(t, err)
	require.Equal(t, "0xnewaddress", got.Address)
	require.Equal(t, "active", got.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Guard against silently reintroducing a lookup that can pick the retired row.
func TestExistingAssignmentQueryOrdersActiveFirst(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`ORDER BY \(status = 'active'\) DESC, id DESC LIMIT 1`).
		WithArgs(int64(1), FamilyEVM).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "chain", "address", "derivation_index", "status"}).
			AddRow(1, 1, "evm", "0xactive", 1, "active"))
	_, err = NewRepo(db).ExistingAssignment(context.Background(), 1, FamilyEVM)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScannerStillWatchesDisabledAssignments(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`status IN \('active', 'disabled'\)`).WithArgs(FamilyEVM).
		WillReturnRows(sqlmock.NewRows([]string{"address"}).AddRow("0xactive").AddRow("0xdisabled"))
	got, err := NewRepo(db).ActiveAddresses(t.Context(), FamilyEVM)
	require.NoError(t, err)
	require.Equal(t, []string{"0xactive", "0xdisabled"}, got)
	require.NoError(t, mock.ExpectationsWereMet())
}
