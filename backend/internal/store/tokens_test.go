package store_test

import (
	"bytes"
	"errors"
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

	var sealedFor uuid.UUID
	id, err := testStore.LinkConnection(ctx, u.ID, gh, []string{"scope-a"}, func(connID uuid.UUID) (store.SealedTokens, error) {
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
	again, err := testStore.LinkConnection(ctx, u.ID, gh, []string{"scope-a", "scope-b"}, func(uuid.UUID) (store.SealedTokens, error) {
		s := fakeSealed(t0.Add(time.Hour))
		s.Access = []byte("fake-sealed-access-2")
		return s, nil
	})
	if err != nil || again != id {
		t.Fatalf("relink: id %s (want %s), err %v", again, id, err)
	}
	info, err = testStore.GetConnection(ctx, u.ID, gh)
	if err != nil || info.Status != domain.ConnectionActive || len(info.Scopes) != 2 || info.NextSyncAt.After(time.Now()) {
		t.Fatalf("relinked connection: %v %+v", err, info)
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
	if _, err := testStore.LinkConnection(ctx, other.ID, gh, nil, func(uuid.UUID) (store.SealedTokens, error) {
		return store.SealedTokens{}, errors.New("seal failed")
	}); err == nil {
		t.Fatal("LinkConnection succeeded despite seal error")
	}
	if _, err := testStore.GetConnection(ctx, other.ID, gh); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed link left a connection behind: %v", err)
	}
}
