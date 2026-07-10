package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

const (
	testFactory  = "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323"
	testInitHash = "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ece"
)

func TestDepositAddressStore_EnsureAddress_Idempotent(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewDepositAddressStore(pool)

	addr, err := core.DeriveDepositAddress(testFactory, testInitHash, 5001)
	require.NoError(t, err)

	first, err := store.EnsureAddress(ctx, core.AddressRegistrationInput{
		AccountHolder: 5001, Address: addr, Factory: testFactory, InitHash: testInitHash,
	})
	require.NoError(t, err)
	assert.Equal(t, addr, first.Address)
	assert.Equal(t, int64(5001), first.AccountHolder)

	// Second call for the same holder is a no-op returning the existing row --
	// account_holder is UNIQUE (design doc §2: one address per holder).
	second, err := store.EnsureAddress(ctx, core.AddressRegistrationInput{
		AccountHolder: 5001, Address: addr, Factory: testFactory, InitHash: testInitHash,
	})
	require.NoError(t, err)
	assert.Equal(t, first.UID, second.UID)
	assert.Equal(t, first.CreatedAt, second.CreatedAt)
}

func TestDepositAddressStore_GetByAddress_ChecksumNormalizes(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewDepositAddressStore(pool)

	addr, err := core.DeriveDepositAddress(testFactory, testInitHash, 5002)
	require.NoError(t, err)

	_, err = store.EnsureAddress(ctx, core.AddressRegistrationInput{
		AccountHolder: 5002, Address: addr, Factory: testFactory, InitHash: testInitHash,
	})
	require.NoError(t, err)

	// A caller passing the observed `to` address in a DIFFERENT casing (e.g.
	// straight off a JSON-RPC response, lowercase by convention) must still
	// resolve to the same row -- this is the whole point of normalizing
	// through core.ChecksumAddress before every lookup (Foundation contract
	// §7-2's silent-miss warning).
	lower := strings.ToLower(addr)
	require.NotEqual(t, addr, lower, "test address must actually contain mixed-case letters to be meaningful")

	got, err := store.GetByAddress(ctx, lower)
	require.NoError(t, err)
	assert.Equal(t, int64(5002), got.AccountHolder)
	assert.Equal(t, addr, got.Address) // stored value is canonical EIP-55 casing regardless of lookup casing

	upper := strings.ToUpper(strings.TrimPrefix(addr, "0x"))
	got2, err := store.GetByAddress(ctx, "0x"+upper)
	require.NoError(t, err)
	assert.Equal(t, int64(5002), got2.AccountHolder)
}

func TestDepositAddressStore_GetByAddress_NotFound(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewDepositAddressStore(pool)

	addr, err := core.DeriveDepositAddress(testFactory, testInitHash, 999999)
	require.NoError(t, err)

	_, err = store.GetByAddress(ctx, addr)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestDepositAddressStore_ListAddresses(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewDepositAddressStore(pool)

	before, err := store.ListAddresses(ctx)
	require.NoError(t, err)

	addr1, err := core.DeriveDepositAddress(testFactory, testInitHash, 6001)
	require.NoError(t, err)
	addr2, err := core.DeriveDepositAddress(testFactory, testInitHash, 6002)
	require.NoError(t, err)

	_, err = store.EnsureAddress(ctx, core.AddressRegistrationInput{AccountHolder: 6001, Address: addr1, Factory: testFactory, InitHash: testInitHash})
	require.NoError(t, err)
	_, err = store.EnsureAddress(ctx, core.AddressRegistrationInput{AccountHolder: 6002, Address: addr2, Factory: testFactory, InitHash: testInitHash})
	require.NoError(t, err)

	after, err := store.ListAddresses(ctx)
	require.NoError(t, err)
	assert.Len(t, after, len(before)+2)
}

func TestDepositAddressStore_EnsureAddress_InvalidInput(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewDepositAddressStore(pool)

	_, err := store.EnsureAddress(ctx, core.AddressRegistrationInput{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, core.ErrInvalidInput))
}
