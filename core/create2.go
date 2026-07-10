package core

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"
)

// DeriveDepositAddress computes the CREATE2 deposit address for holder under
// the given (factory, initHash) fingerprint (design doc §2):
//
//	address = keccak256(0xff ++ factory ++ salt ++ initHash)[12:]
//	salt    = bytes32(holder), big-endian, left-padded with zeros
//
// This is the same construction as azex-contracts' DepositFactory
// (EIP-1014) and the existing azex-server reference implementation
// (azex-server/internal/pkg/create2) -- same operand order, same salt
// encoding -- so a (factory, initHash, holder) triple derives the identical
// address everywhere it is computed: onchain, in chains/evm, and here.
// factory and initHash must each be 0x-prefixed hex of the right byte length
// (20 and 32 respectively). The same factory/initHash pair must be deployed
// at the same addresses on every EVM chain the caller targets for holder to
// get one address across all of them.
//
// holder must be a real (positive) account holder id -- deposit addresses
// are never derived for the negative system-account range
// (core.IsSystemAccount).
//
// The result is EIP-55 checksum-cased.
func DeriveDepositAddress(factory, initHash string, holder int64) (string, error) {
	if holder <= 0 {
		return "", fmt.Errorf("core: create2: holder must be positive, got %d: %w", holder, ErrInvalidInput)
	}
	factoryBytes, err := decodeFixedHex(factory, 20, "factory")
	if err != nil {
		return "", err
	}
	initHashBytes, err := decodeFixedHex(initHash, 32, "init_hash")
	if err != nil {
		return "", err
	}

	salt := make([]byte, 32)
	new(big.Int).SetInt64(holder).FillBytes(salt)

	data := make([]byte, 0, 1+len(factoryBytes)+len(salt)+len(initHashBytes))
	data = append(data, 0xff)
	data = append(data, factoryBytes...)
	data = append(data, salt...)
	data = append(data, initHashBytes...)

	hash := keccak256(data)
	return toChecksumAddress(hash[12:]), nil
}

func decodeFixedHex(s string, wantLen int, field string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return nil, fmt.Errorf("core: create2: %s: invalid hex %q: %w", field, s, err)
	}
	if len(b) != wantLen {
		return nil, fmt.Errorf("core: create2: %s must be %d bytes, got %d: %w", field, wantLen, len(b), ErrInvalidInput)
	}
	return b, nil
}

func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// toChecksumAddress applies EIP-55 mixed-case checksum encoding to a raw
// 20-byte address: hash the lowercase hex ASCII representation, then
// uppercase each a-f hex digit whose corresponding nibble of that hash is
// >= 8. Matches OpenZeppelin's Strings.toChecksumHexString byte-for-byte
// (azex-contracts' lib/openzeppelin-contracts/contracts/utils/Strings.sol).
func toChecksumAddress(addr []byte) string {
	hexAddr := hex.EncodeToString(addr) // lowercase, no 0x
	hash := keccak256([]byte(hexAddr))

	out := make([]byte, len(hexAddr))
	for i := 0; i < len(hexAddr); i++ {
		c := hexAddr[i]
		if c < 'a' || c > 'f' {
			out[i] = c // digit 0-9, unaffected by checksumming
			continue
		}
		nibble := hash[i/2]
		if i%2 == 0 {
			nibble >>= 4
		} else {
			nibble &= 0x0f
		}
		if nibble >= 8 {
			out[i] = c - 32 // 'a'..'f' -> 'A'..'F'
		} else {
			out[i] = c
		}
	}
	return "0x" + string(out)
}
