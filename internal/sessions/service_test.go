package sessions

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
)

// Runs against the local marketflow database (shared with marketflow-api); skips without it.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	db, err := sql.Open("pgx", "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable")
	if err != nil || db.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func twoTenants(t *testing.T, db *sql.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM tenants ORDER BY created_at LIMIT 2`)
	if err != nil {
		t.Skipf("tenants table: %v", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) < 2 {
		t.Skip("needs two local tenants")
	}
	return ids[0], ids[1]
}

func TestClientChosenSessionID(t *testing.T) {
	db := openDB(t)
	a, b := twoTenants(t, db)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()
	sid := uuid.New()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM chat_sessions WHERE id = $1`, sid) })

	created, err := s.CreateSessionWithID(ctx, a, sid, "web_widget")
	if err != nil || !created {
		t.Fatalf("create with the client's id: %v %v", created, err)
	}
	if _, err := s.LoadHistory(ctx, a, sid); err != nil {
		t.Fatalf("the session must load for its tenant: %v", err)
	}
	// Another tenant cannot claim the id, and cannot read the session.
	if created, err := s.CreateSessionWithID(ctx, b, sid, "web_widget"); err != nil || created {
		t.Fatalf("another tenant took the id: %v %v", created, err)
	}
	if _, err := s.LoadHistory(ctx, b, sid); err == nil {
		t.Fatal("another tenant read the session")
	}
}
