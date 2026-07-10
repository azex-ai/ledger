//go:build e2e

package evm

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/stretchr/testify/require"
)

// End-to-end test against a local anvil chain: watcher (Reader.FetchDeposits)
// observes a real ERC-20 transfer to a CREATE2-derived deposit address, then
// Sweeper.BatchSweep collects it into the DepositFactory's treasury (design
// doc §7: "chains/evm: anvil 本地链，watcher→入账→归集端到端").
//
// Requires the `anvil` binary on PATH (Foundry). Run with:
//
//	go test -tags e2e ./...
//
// Skipped implicitly when the default (no `e2e` tag) `go test ./...` is run,
// per task instructions ("anvil 不可用时 e2e 用 build tag 跳过并说明").

// Anvil's well-known deterministic dev accounts (public, documented,
// test-only -- never used outside this local ephemeral chain).
const (
	anvilOwnerKey    = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilOwnerAddr   = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	anvilSweeperKey  = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	anvilSweeperAddr = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	anvilChainID     = 31337
)

func TestE2E_WatchThenSweep(t *testing.T) {
	rpcURL, stop := startAnvil(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := ethclient.DialContext(ctx, rpcURL)
	require.NoError(t, err)
	defer client.Close()

	ownerKey, err := crypto.HexToECDSA(anvilOwnerKey)
	require.NoError(t, err)
	ownerAuth, err := bind.NewKeyedTransactorWithChainID(ownerKey, big.NewInt(anvilChainID))
	require.NoError(t, err)

	// --- deploy MockUSDT + DepositFactory -------------------------------
	usdtArt := mockUSDTArtifact()
	usdtAddr, tx, usdtContract, err := bind.DeployContract(ownerAuth, usdtArt.ABI, usdtArt.Bytecode, client)
	require.NoError(t, err)
	_, err = bind.WaitMined(ctx, client, tx)
	require.NoError(t, err)

	factoryArt := depositFactoryArtifact()
	owner := common.HexToAddress(anvilOwnerAddr)
	// create2Prefix = 0xff (standard EVM CREATE2), maxBatchSize = 50.
	factoryAddr, tx, factoryContract, err := bind.DeployContract(ownerAuth, factoryArt.ABI, factoryArt.Bytecode, client,
		owner, owner, big.NewInt(50), [1]byte{0xff})
	require.NoError(t, err)
	_, err = bind.WaitMined(ctx, client, tx)
	require.NoError(t, err)

	// Read the deployed factory's actual init-code hash rather than
	// recomputing it -- this is the exact fingerprint DeriveDepositAddress
	// needs, and reading it removes any risk of this test silently drifting
	// from DepositProxy's real creation code.
	var initHashOut []interface{}
	initHashOut, err = callView(ctx, factoryContract, "proxyInitHash")
	require.NoError(t, err)
	initHash := initHashOut[0].([32]byte)
	initHashHex := "0x" + common.Bytes2Hex(initHash[:])

	const holder int64 = 42
	depositAddr, err := core.DeriveDepositAddress(factoryAddr.Hex(), initHashHex, holder)
	require.NoError(t, err)

	// --- allowlist the token + the sweeper account ----------------------
	tx, err = factoryContract.Transact(ownerAuth, "setAllowedToken", usdtAddr, true)
	require.NoError(t, err)
	_, err = bind.WaitMined(ctx, client, tx)
	require.NoError(t, err)

	sweeperAddr := common.HexToAddress(anvilSweeperAddr)
	tx, err = factoryContract.Transact(ownerAuth, "setSweeper", sweeperAddr, true)
	require.NoError(t, err)
	_, err = bind.WaitMined(ctx, client, tx)
	require.NoError(t, err)

	// --- simulate a deposit: mint + transfer to the derived address ------
	depositAmountRaw := big.NewInt(0).Mul(big.NewInt(1000), big.NewInt(1e18)) // MockUSDT has 18 decimals (OZ default, not overridden)
	tx, err = usdtContract.Transact(ownerAuth, "mint", owner, depositAmountRaw)
	require.NoError(t, err)
	_, err = bind.WaitMined(ctx, client, tx)
	require.NoError(t, err)

	tx, err = usdtContract.Transact(ownerAuth, "transfer", common.HexToAddress(depositAddr), depositAmountRaw)
	require.NoError(t, err)
	receipt, err := bind.WaitMined(ctx, client, tx)
	require.NoError(t, err)
	depositTxHash := tx.Hash().Hex()

	// --- watcher side: Reader.FetchDeposits must see the transfer --------
	chainSet := core.ChainSet{
		anvilChainID: core.ChainConfig{
			ChainID:       anvilChainID,
			Confirmations: 1,
			Factory:       factoryAddr.Hex(),
			InitHash:      initHashHex,
			CreditTokens: map[string]core.TokenConfig{
				normalizeTokenKey(usdtAddr.Hex()): {TokenAddress: normalizeTokenKey(usdtAddr.Hex()), CurrencyCode: "USDT", Decimals: 18},
			},
			SweepTokens: map[string]core.TokenConfig{
				normalizeTokenKey(usdtAddr.Hex()): {TokenAddress: normalizeTokenKey(usdtAddr.Hex()), CurrencyCode: "USDT", Decimals: 18},
			},
		},
	}
	clients, err := NewClientSet(ctx, chainSet, map[int64]string{anvilChainID: rpcURL})
	require.NoError(t, err)
	defer clients.Close()

	reader := NewReader(clients, 0)
	latest, err := reader.LatestBlock(ctx, anvilChainID)
	require.NoError(t, err)

	sightings, err := reader.FetchDeposits(ctx, anvilChainID, 0, latest, []string{depositAddr})
	require.NoError(t, err)
	require.Len(t, sightings, 1)
	got := sightings[0]
	require.Equal(t, depositTxHash, got.TxHash)
	require.Equal(t, int32(0), got.TxLogSeq)
	require.True(t, got.Amount.Equal(normalizeAmount(depositAmountRaw, 18)), "amount = %s", got.Amount)
	require.Equal(t, common.HexToAddress(depositAddr).Hex(), got.To)
	require.True(t, got.Confirmations >= 1)

	included, err := reader.TxIncluded(ctx, anvilChainID, depositTxHash)
	require.NoError(t, err)
	require.True(t, included)

	// --- sweeper side: BatchSweep must collect the deposit into treasury -
	sweeperSigner, err := NewLocalSigner(anvilSweeperKey)
	require.NoError(t, err)
	require.Equal(t, sweeperAddr.Hex(), sweeperSigner.Address())

	sweeper := NewSweeper(clients, sweeperSigner, sweeperSigner.Address())
	nonce, err := sweeper.NextNonce(ctx, anvilChainID)
	require.NoError(t, err)

	txHash, err := sweeper.BatchSweep(ctx, anvilChainID, usdtAddr.Hex(),
		[]core.SweepTarget{{Address: depositAddr, AccountHolder: holder}}, nonce)
	require.NoError(t, err)
	require.NotEmpty(t, txHash)

	require.Eventually(t, func() bool {
		_, err := client.TransactionReceipt(ctx, common.HexToHash(txHash))
		return err == nil
	}, 20*time.Second, 200*time.Millisecond, "sweep tx never mined")

	// treasury == owner in this test's factory constructor args.
	treasuryBalOut, err := callView(ctx, usdtContract, "balanceOf", owner)
	require.NoError(t, err)
	treasuryBal := treasuryBalOut[0].(*big.Int)
	require.True(t, treasuryBal.Cmp(depositAmountRaw) >= 0, "treasury did not receive the swept deposit")

	depositAddrBalOut, err := callView(ctx, usdtContract, "balanceOf", common.HexToAddress(depositAddr))
	require.NoError(t, err)
	depositAddrBal := depositAddrBalOut[0].(*big.Int)
	require.Equal(t, int64(0), depositAddrBal.Int64(), "deposit address should be swept to zero")

	// --- scanner side: sanity-check ScanBalances agrees post-sweep -------
	scanner := NewScanner(clients, 0)
	balances, err := scanner.ScanBalances(ctx, anvilChainID, usdtAddr.Hex(), []string{depositAddr})
	require.NoError(t, err)
	require.True(t, balances[depositAddr].IsZero())

	_ = receipt // receipt only needed to confirm WaitMined succeeded above
}

func callView(ctx context.Context, contract *bind.BoundContract, method string, args ...interface{}) ([]interface{}, error) {
	var out []interface{}
	err := contract.Call(&bind.CallOpts{Context: ctx}, &out, method, args...)
	return out, err
}

func startAnvil(t *testing.T) (rpcURL string, stop func()) {
	t.Helper()
	if _, err := exec.LookPath("anvil"); err != nil {
		t.Skip("anvil not found on PATH -- skipping e2e test (Foundry required, see chains/evm doc)")
	}
	port := freePort(t)
	cmd := exec.Command("anvil", "--port", strconv.Itoa(port), "--silent")
	require.NoError(t, cmd.Start())

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := ethclient.Dial(url)
		if err == nil {
			if _, err := c.BlockNumber(context.Background()); err == nil {
				c.Close()
				return url, func() {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}
			c.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	t.Fatal("anvil did not become ready in time")
	return "", func() {}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
