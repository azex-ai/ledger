package core

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ground truth for CREATE2 vectors below: computed independently via
// Foundry's `cast create2` (mainnet-grade EVM tooling; same
// keccak256(0xff++factory++salt++initHash) construction as
// azex-contracts/src/DepositFactory.sol). The holder=1 vector additionally
// matches azex-server's proven reference implementation
// (azex-server/internal/pkg/create2/create2_test.go's "nonce=1" case), which
// is asserted against a real mainnet DepositFactory deployment -- so that
// one vector is cross-validated against two independent sources.
//
// These vectors are also the pin downstream chains/evm and azex-contracts
// forge tests are expected to reproduce (design doc §7).
const (
	testFactory  = "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323"
	testInitHash = "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ece"
)

func TestDeriveDepositAddress_Vectors(t *testing.T) {
	cases := []struct {
		name   string
		holder int64
		want   string // EIP-55 checksummed, via `cast create2`
	}{
		{
			name:   "holder=1 (cross-validated against azex-server mainnet vector)",
			holder: 1,
			want:   "0xB3e7eA5de7C24b4e89b1AC454f02a42DBAE0BFc0",
		},
		{
			name:   "holder=2",
			holder: 2,
			want:   "0x550427E1afB2EbD4889031DCE4801D993017d7eC",
		},
		{
			name:   "holder=1001",
			holder: 1001,
			want:   "0x8eaA0E854b1ff86188774AA71AeCfDFFc0f93F67",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveDepositAddress(testFactory, testInitHash, tc.holder)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDeriveDepositAddress_Deterministic(t *testing.T) {
	a1, err := DeriveDepositAddress(testFactory, testInitHash, 42)
	require.NoError(t, err)
	a2, err := DeriveDepositAddress(testFactory, testInitHash, 42)
	require.NoError(t, err)
	assert.Equal(t, a1, a2)
}

func TestDeriveDepositAddress_UniquePerHolder(t *testing.T) {
	a1, err := DeriveDepositAddress(testFactory, testInitHash, 1)
	require.NoError(t, err)
	a2, err := DeriveDepositAddress(testFactory, testInitHash, 2)
	require.NoError(t, err)
	assert.NotEqual(t, a1, a2)
}

func TestDeriveDepositAddress_RejectsNonPositiveHolder(t *testing.T) {
	for _, holder := range []int64{0, -1, -1001} {
		_, err := DeriveDepositAddress(testFactory, testInitHash, holder)
		assert.ErrorIs(t, err, ErrInvalidInput)
	}
}

func TestDeriveDepositAddress_RejectsMalformedFactoryOrInitHash(t *testing.T) {
	_, err := DeriveDepositAddress("0xnothex", testInitHash, 1)
	assert.Error(t, err)

	_, err = DeriveDepositAddress("0x1234", testInitHash, 1) // wrong length
	assert.ErrorIs(t, err, ErrInvalidInput)

	_, err = DeriveDepositAddress(testFactory, "0x1234", 1) // wrong length
	assert.ErrorIs(t, err, ErrInvalidInput)
}

// EIP-55 checksum vectors: the canonical examples from the EIP-55
// specification (https://eips.ethereum.org/EIPS/eip-55 "Test Cases"),
// independently reproduced here via `cast to-check-sum-address`.
func TestToChecksumAddress_EIP55Vectors(t *testing.T) {
	cases := []struct {
		lower string
		want  string
	}{
		// All caps
		{"0x52908400098527886e0f7030069857d2e4169ee7", "0x52908400098527886E0F7030069857D2E4169EE7"},
		{"0x8617e340b3d01fa5f11f306f4090fd50e238070d", "0x8617E340B3D01FA5F11F306F4090FD50E238070D"},
		// All lower
		{"0xde709f2102306220921060314715629080e2fb77", "0xde709f2102306220921060314715629080e2fb77"},
		{"0x27b1fdb04752bbc536007a920d24acb045561c26", "0x27b1fdb04752bbc536007a920d24acb045561c26"},
		// Normal
		{"0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"},
		{"0xfb6916095ca1df60bb79ce92ce3ea74c37c5d359", "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"},
		{"0xdbf03b407c01e7cd3cbea99509d93f8dddc8c6fb", "0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB"},
		{"0xd1220a0cf47c7b9be7a2e6ba89f429762e7b9adb", "0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb"},
	}
	for _, tc := range cases {
		t.Run(tc.lower, func(t *testing.T) {
			bytesAddr, err := hex.DecodeString(strings.TrimPrefix(tc.lower, "0x"))
			require.NoError(t, err)
			got := toChecksumAddress(bytesAddr)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ChecksumAddress is the exported entry point store adapters use to
// normalize an observed/looked-up address before touching deposit_addresses
// (design doc §7-2). It must accept any input casing and always return the
// same canonical EIP-55 form.
func TestChecksumAddress_NormalizesAnyCasing(t *testing.T) {
	want := "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
	inputs := []string{
		"0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", // all lower (the common wire format: RPC/viem/ethers emit lowercase "0x")
		"0x5AAEB6053F3E94C9B9A09F33669435E7EF1BEAED", // all upper body
		want, // already canonical
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			got, err := ChecksumAddress(in)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestChecksumAddress_Deterministic(t *testing.T) {
	addr, err := DeriveDepositAddress(testFactory, testInitHash, 4242)
	require.NoError(t, err)

	// Round-tripping an already-checksummed address through ChecksumAddress
	// must be a no-op (idempotent normalization).
	got, err := ChecksumAddress(addr)
	require.NoError(t, err)
	assert.Equal(t, addr, got)
}

func TestChecksumAddress_RejectsWrongLengthOrInvalidHex(t *testing.T) {
	_, err := ChecksumAddress("0x1234")
	assert.ErrorIs(t, err, ErrInvalidInput)

	_, err = ChecksumAddress("0xnothex000000000000000000000000000000000")
	assert.Error(t, err)
}
