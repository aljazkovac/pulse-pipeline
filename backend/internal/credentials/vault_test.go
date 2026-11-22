package credentials_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/credentials"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/sync"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/testdb"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/tokencrypt"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.Store
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, func(_ string, pool *pgxpool.Pool) {
		testPool, testStore = pool, store.New(pool)
	}))
}

var _ sync.CredentialStore = (*credentials.Vault)(nil)

var (
	key1 = bytes.Repeat([]byte{0x31}, tokencrypt.KeySize)
	key2 = bytes.Repeat([]byte{0x32}, tokencrypt.KeySize)
	t0   = time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)
)

func keyring(t *testing.T, current int16, keys map[int16][]byte) *tokencrypt.Keyring {
	t.Helper()
	k, err := tokencrypt.NewKeyring(current, keys)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func setup(t *testing.T) (context.Context, domain.User) {
	t.Helper()
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	u, err := testStore.CreateUser(ctx, fmt.Sprintf("vault-%s@example.com", uuid.NewString()), "Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, u
}

func fakeCreds(suffix string) domain.Credentials {
	return domain.Credentials{
		AccessToken: "fake-access-" + suffix, RefreshToken: "fake-refresh-" + suffix,
		ExpiresAt: t0.Add(time.Hour),
	}
}

func startRun(t *testing.T, ctx context.Context, u domain.User) domain.SyncRun {
	t.Helper()
	day := domain.DateOf(t0)
	run, err := testStore.StartSyncRun(ctx, u.ID, domain.ProviderGoogleHealth, domain.SyncTriggerManual, domain.DateRange{Start: day, End: day})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestVault_LinkLoadSave(t *testing.T) {
	ctx, u := setup(t)
	v := credentials.NewVault(testStore, keyring(t, 1, map[int16][]byte{1: key1}))

	id, err := v.Link(ctx, u.ID, domain.ProviderGoogleHealth, "health-user", []string{"fake-scope"}, fakeCreds("1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.LoadCredentials(ctx, u.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != fakeCreds("1") {
		t.Fatalf("LoadCredentials = %+v, want the linked tokens", got.AccessToken == "fake-access-1")
	}

	// Plaintext never reaches the database.
	var access, refresh []byte
	if err := testPool.QueryRow(ctx, `SELECT access_token_ciphertext, refresh_token_ciphertext FROM provider_connections WHERE id = $1`, id).Scan(&access, &refresh); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(access, []byte("fake-access-1")) || bytes.Contains(refresh, []byte("fake-refresh-1")) {
		t.Fatal("token plaintext stored in provider_connections")
	}

	if err := v.SaveCredentials(ctx, startRun(t, ctx, u), id, fakeCreds("2")); err != nil {
		t.Fatal(err)
	}
	if got, err := v.LoadCredentials(ctx, u.ID, id); err != nil || got != fakeCreds("2") {
		t.Fatalf("after save: ok=%v err=%v", got == fakeCreds("2"), err)
	}

	if _, err := v.Link(ctx, u.ID, domain.ProviderGoogleHealth, "health-user", nil, domain.Credentials{AccessToken: "fake-access-only"}); err == nil {
		t.Fatal("Link accepted credentials without a refresh token")
	}
}

func TestVault_ConnectionWithoutTokensLoadsEmpty(t *testing.T) {
	ctx, u := setup(t)
	conn, err := testStore.CreateConnection(ctx, u.ID, domain.ProviderMock, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := credentials.NewVault(testStore, keyring(t, 1, map[int16][]byte{1: key1}))
	got, err := v.LoadCredentials(ctx, u.ID, conn.ID)
	if err != nil || got != (domain.Credentials{}) {
		t.Fatalf("LoadCredentials = empty? %v, err %v", got == domain.Credentials{}, err)
	}
}

func TestVault_TenantIsolation(t *testing.T) {
	ctx, alice := setup(t)
	_, bob := setup(t)
	v := credentials.NewVault(testStore, keyring(t, 1, map[int16][]byte{1: key1}))
	aliceConn, err := v.Link(ctx, alice.ID, domain.ProviderGoogleHealth, "health-alice", nil, fakeCreds("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.LoadCredentials(ctx, bob.ID, aliceConn); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob loading alice's credentials: want ErrNotFound, got %v", err)
	}
	bobConn, err := v.Link(ctx, bob.ID, domain.ProviderGoogleHealth, "health-bob", nil, fakeCreds("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SaveCredentials(ctx, startRun(t, ctx, bob), aliceConn, fakeCreds("bob")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob saving over alice's credentials: want ErrNotFound, got %v", err)
	}
	if got, err := v.LoadCredentials(ctx, alice.ID, aliceConn); err != nil || got != fakeCreds("alice") {
		t.Errorf("alice's credentials changed (err %v)", err)
	}

	// Ciphertext copied from Alice's row into Bob's fails authentication:
	// it is bound to the row it was sealed for.
	if _, err := testPool.Exec(ctx, `
		UPDATE provider_connections b
		SET access_token_ciphertext = a.access_token_ciphertext,
		    refresh_token_ciphertext = a.refresh_token_ciphertext
		FROM provider_connections a
		WHERE a.id = $1 AND b.id = $2`, aliceConn, bobConn); err != nil {
		t.Fatal(err)
	}
	if _, err := v.LoadCredentials(ctx, bob.ID, bobConn); !errors.Is(err, tokencrypt.ErrDecrypt) {
		t.Fatalf("copied ciphertext: want ErrDecrypt, got %v", err)
	}

	// Swapping the access and refresh columns within a row fails too.
	if _, err := testPool.Exec(ctx, `
		UPDATE provider_connections
		SET access_token_ciphertext = refresh_token_ciphertext,
		    refresh_token_ciphertext = access_token_ciphertext
		WHERE id = $1`, aliceConn); err != nil {
		t.Fatal(err)
	}
	if _, err := v.LoadCredentials(ctx, alice.ID, aliceConn); !errors.Is(err, tokencrypt.ErrDecrypt) {
		t.Fatalf("swapped columns: want ErrDecrypt, got %v", err)
	}
}

func TestVault_KeyRotation(t *testing.T) {
	ctx, u := setup(t)
	v1 := credentials.NewVault(testStore, keyring(t, 1, map[int16][]byte{1: key1}))
	id, err := v1.Link(ctx, u.ID, domain.ProviderGoogleHealth, "health-user", nil, fakeCreds("old"))
	if err != nil {
		t.Fatal(err)
	}

	rotated := credentials.NewVault(testStore, keyring(t, 2, map[int16][]byte{1: key1, 2: key2}))
	if got, err := rotated.LoadCredentials(ctx, u.ID, id); err != nil || got != fakeCreds("old") {
		t.Fatalf("reading v1 row after rotation failed: %v", err)
	}
	// A refresh re-saves the tokens, which re-seals them under the new key.
	if err := rotated.SaveCredentials(ctx, startRun(t, ctx, u), id, fakeCreds("new")); err != nil {
		t.Fatal(err)
	}
	sealed, err := testStore.GetConnectionTokens(ctx, u.ID, id)
	if err != nil || sealed.KeyVersion != 2 {
		t.Fatalf("key version after re-save = %d (err %v), want 2", sealed.KeyVersion, err)
	}
	retired := credentials.NewVault(testStore, keyring(t, 2, map[int16][]byte{2: key2}))
	if got, err := retired.LoadCredentials(ctx, u.ID, id); err != nil || got != fakeCreds("new") {
		t.Fatalf("reading v2 row after retiring v1 failed: %v", err)
	}
	if _, err := v1.LoadCredentials(ctx, u.ID, id); !errors.Is(err, tokencrypt.ErrUnknownKeyVersion) {
		t.Fatalf("old keyring reading v2 row: want ErrUnknownKeyVersion, got %v", err)
	}
}
