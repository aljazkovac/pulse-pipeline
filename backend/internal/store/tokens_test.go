package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

// The store only moves opaque ciphertext; these byte strings stand in for it.
func fakeSealed(expires time.Time) store.SealedTokens {
	return store.SealedTokens{
		Access: []byte("fake-sealed-access"), Refresh: []byte("fake-sealed-refresh"),
		KeyVersion: 3, ExpiresAt: expires,
	}
}

func TestConnectionTokens_RoundTrip(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	conn, err := testStore.CreateConnection(ctx, u.ID, domain.ProviderGoogleHealth, nil)
	if err != nil {
		t.Fatal(err)
	}

	empty, err := testStore.GetConnectionTokens(ctx, u.ID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Access != nil || empty.Refresh != nil || empty.KeyVersion != 0 || !empty.ExpiresAt.IsZero() {
		t.Fatalf("new connection has tokens: %v", empty)
	}

	want := fakeSealed(t0.Add(time.Hour))
	if err := testStore.SetConnectionTokens(ctx, u.ID, conn.ID, want); err != nil {
		t.Fatal(err)
	}
	got, err := testStore.GetConnectionTokens(ctx, u.ID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Access, want.Access) || !bytes.Equal(got.Refresh, want.Refresh) ||
		got.KeyVersion != want.KeyVersion || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("round trip: got %v %q %q, want %v", got, got.Access, got.Refresh, want)
	}

	// Ciphertext without a key version is refused before it reaches SQL.
	if err := testStore.SetConnectionTokens(ctx, u.ID, conn.ID, store.SealedTokens{Access: []byte("x")}); err == nil {
		t.Fatal("SetConnectionTokens accepted ciphertext without a key version")
	}
}

func TestConnectionTokens_TenantIsolation(t *testing.T) {
	ctx := ctxT(t)
	alice, bob := newUser(t), newUser(t)
	conn, err := testStore.CreateConnection(ctx, alice.ID, domain.ProviderGoogleHealth, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := testStore.SetConnectionTokens(ctx, alice.ID, conn.ID, fakeSealed(t0)); err != nil {
		t.Fatal(err)
	}

	if _, err := testStore.GetConnectionTokens(ctx, bob.ID, conn.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob reading alice's tokens: want ErrNotFound, got %v", err)
	}
	overwrite := store.SealedTokens{Access: []byte("fake-bob"), KeyVersion: 1}
	if err := testStore.SetConnectionTokens(ctx, bob.ID, conn.ID, overwrite); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob overwriting alice's tokens: want ErrNotFound, got %v", err)
	}
	got, err := testStore.GetConnectionTokens(ctx, alice.ID, conn.ID)
	if err != nil || !bytes.Equal(got.Access, []byte("fake-sealed-access")) {
		t.Errorf("alice's tokens changed: %v %q", err, got.Access)
	}
	if _, err := testStore.GetConnectionTokens(ctx, alice.ID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown connection: want ErrNotFound, got %v", err)
	}
}

func TestLinkConnection_CreatesThenReactivates(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	gh := domain.ProviderGoogleHealth
	const accountID = "health-user"

	var sealedFor uuid.UUID
	id, err := testStore.LinkConnection(ctx, u.ID, gh, accountID, []string{"scope-a"}, func(connID uuid.UUID) (store.SealedTokens, error) {
		sealedFor = connID
		return fakeSealed(t0), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sealedFor != id {
		t.Fatalf("seal got connection %s, link returned %s", sealedFor, id)
	}
	info, err := testStore.GetConnection(ctx, u.ID, gh)
	if err != nil || info.ID != id || info.Status != domain.ConnectionActive || len(info.Scopes) != 1 {
		t.Fatalf("linked connection: %v %+v", err, info)
	}

	// A connection in error with a cursor is re-linked: same row, active, due
	// now, cursor kept.
	cursor := []byte(`{"synced_through": "2026-11-19"}`)
	future := time.Now().Add(48 * time.Hour)
	if err := testStore.MarkConnectionSynced(ctx, u.ID, id, t0, future, cursor); err != nil {
		t.Fatal(err)
	}
	if err := testStore.SetConnectionStatus(ctx, u.ID, id, domain.ConnectionError); err != nil {
		t.Fatal(err)
	}
	again, err := testStore.LinkConnection(ctx, u.ID, gh, accountID, []string{"scope-a", "scope-b"}, func(uuid.UUID) (store.SealedTokens, error) {
		s := fakeSealed(t0.Add(time.Hour))
		s.Access = []byte("fake-sealed-access-2")
		return s, nil
	})
	if err != nil || again != id {
		t.Fatalf("relink: id %s (want %s), err %v", again, id, err)
	}
	info, err = testStore.GetConnection(ctx, u.ID, gh)
	if err != nil || info.Status != domain.ConnectionActive || len(info.Scopes) != 2 {
		t.Fatalf("relinked connection: %v %+v", err, info)
	}
	// LinkConnection schedules the sync using PostgreSQL's clock.
	var databaseNow time.Time
	if err := testPool.QueryRow(ctx, "SELECT now()").Scan(&databaseNow); err != nil {
		t.Fatalf("reading database time: %v", err)
	}
	if info.NextSyncAt.After(databaseNow) {
		t.Fatalf("relinked connection due at %v, after database time %v", info.NextSyncAt, databaseNow)
	}
	raw, err := testStore.GetConnectionSyncCursor(ctx, u.ID, id)
	if err != nil || !bytes.Contains(raw, []byte("2026-11-19")) {
		t.Fatalf("cursor lost on relink: %v %s", err, raw)
	}
	toks, err := testStore.GetConnectionTokens(ctx, u.ID, id)
	if err != nil || string(toks.Access) != "fake-sealed-access-2" {
		t.Fatalf("tokens not replaced: %v %q", err, toks.Access)
	}

	// A failing seal rolls the whole link back.
	other := newUser(t)
	if _, err := testStore.LinkConnection(ctx, other.ID, gh, accountID, nil, func(uuid.UUID) (store.SealedTokens, error) {
		return store.SealedTokens{}, errors.New("seal failed")
	}); err == nil {
		t.Fatal("LinkConnection succeeded despite seal error")
	}
	if _, err := testStore.GetConnection(ctx, other.ID, gh); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed link left a connection behind: %v", err)
	}
}

func TestLinkConnection_RejectsUnprovenAccount(t *testing.T) {
	ctx := ctxT(t)
	u, legacyUser := newUser(t), newUser(t)
	gh := domain.ProviderGoogleHealth
	id, err := testStore.LinkConnection(ctx, u.ID, gh, "health-user", []string{"original"}, func(uuid.UUID) (store.SealedTokens, error) {
		return fakeSealed(t0), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if err := testStore.RescheduleConnection(ctx, u.ID, id, future); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.CreateConnection(ctx, legacyUser.ID, gh, nil); err != nil {
		t.Fatal(err)
	}
	seal := func(uuid.UUID) (store.SealedTokens, error) {
		t.Error("sealer called without a verified account match")
		return fakeSealed(t0.Add(time.Hour)), nil
	}
	if _, err := testStore.LinkConnection(ctx, u.ID, gh, "other-account", nil, seal); !errors.Is(err, store.ErrProviderAccountMismatch) {
		t.Fatalf("different account: %v, want ErrProviderAccountMismatch", err)
	}
	if _, err := testStore.LinkConnection(ctx, legacyUser.ID, gh, "health-user", nil, seal); !errors.Is(err, store.ErrProviderAccountUnverified) {
		t.Fatalf("unverified legacy account: %v, want ErrProviderAccountUnverified", err)
	}
	conn, err := testStore.GetConnection(ctx, u.ID, gh)
	if err != nil || !conn.NextSyncAt.Equal(future) || len(conn.Scopes) != 1 || conn.Scopes[0] != "original" {
		t.Fatalf("rejected re-link changed the connection: %+v, %v", conn, err)
	}
	tokens, err := testStore.GetConnectionTokens(ctx, u.ID, id)
	if err != nil || !tokens.ExpiresAt.Equal(t0) {
		t.Fatalf("rejected re-link changed the tokens: %v, %v", tokens, err)
	}
}

func TestLinkConnection_BlocksActiveRunAndFencesAbandonedRefresh(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	gh := domain.ProviderGoogleHealth
	seal := func(uuid.UUID) (store.SealedTokens, error) { return fakeSealed(t0), nil }
	id, err := testStore.LinkConnection(ctx, u.ID, gh, "health-user", nil, seal)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := testStore.ClaimSyncRun(ctx, u.ID, gh, domain.SyncTriggerManual, t0, time.Hour, func([]byte) (domain.DateRange, error) {
		return domain.DateRange{Start: domain.DateOf(t0), End: domain.DateOf(t0)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.LinkConnection(ctx, u.ID, gh, "health-user", nil, seal); !errors.Is(err, store.ErrSyncInProgress) {
		t.Fatalf("re-link during active sync: %v, want ErrSyncInProgress", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE sync_runs SET started_at = now() - interval '2 hours' WHERE user_id = $1 AND id = $2`, u.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := testStore.AbandonStaleSyncRuns(ctx, u.ID, gh, time.Hour); n != 1 || err != nil {
		t.Fatalf("abandoned %d runs, err = %v", n, err)
	}

	linked := fakeSealed(t0.Add(time.Hour))
	if _, err := testStore.LinkConnection(ctx, u.ID, gh, "health-user", nil, func(uuid.UUID) (store.SealedTokens, error) {
		return linked, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := testStore.SetSyncRunTokens(ctx, run, id, fakeSealed(t0)); !errors.Is(err, store.ErrSyncRunAbandoned) {
		t.Fatalf("abandoned run saving tokens: %v, want ErrSyncRunAbandoned", err)
	}
	got, err := testStore.GetConnectionTokens(ctx, u.ID, id)
	if err != nil || !got.ExpiresAt.Equal(linked.ExpiresAt) {
		t.Fatalf("newly linked tokens were overwritten: %v, %v", got, err)
	}
}

func TestSealedTokens_AreRedactedWhenFormatted(t *testing.T) {
	tok := fakeSealed(t0)
	var jsonLog, textLog bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonLog, nil)).Info("link", "tokens", tok)
	slog.New(slog.NewTextHandler(&textLog, nil)).Info("link", "tokens", tok)
	for _, s := range []string{
		fmt.Sprint(tok), fmt.Sprintf("%v", tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok),
		fmt.Sprintf("%v", &tok), jsonLog.String(), textLog.String(),
	} {
		if strings.Contains(s, "fake-sealed") || strings.Contains(s, "ZmFrZS1zZWFsZWQ") { // raw or base64
			t.Errorf("ciphertext leaked in %q", s)
		}
		if !strings.Contains(s, "key_version: 3") {
			t.Errorf("redacted form should keep the key version: %q", s)
		}
	}
}
