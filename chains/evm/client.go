package evm

import (
	"context"
	"fmt"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ClientSet holds one ethclient.Client per chain in a core.ChainSet, keyed
// by chain ID. RPC endpoints are injected by the caller's composition root
// as a plain map -- this package never reads the environment directly
// (abstractions.md Environment Parity; task instructions §"Client 管理").
type ClientSet struct {
	chains  core.ChainSet
	clients map[int64]*ethclient.Client
}

// NewClientSet dials one RPC endpoint per chain present in chains. rpcURLs
// must have an entry for every chain ID in chains, or NewClientSet fails --
// partial configuration is rejected up front rather than surfacing as a
// runtime nil-map lookup later.
func NewClientSet(ctx context.Context, chains core.ChainSet, rpcURLs map[int64]string) (*ClientSet, error) {
	clients := make(map[int64]*ethclient.Client, len(chains))
	for chainID := range chains {
		url, ok := rpcURLs[chainID]
		if !ok || url == "" {
			closeAll(clients)
			return nil, fmt.Errorf("evm: new client set: no rpc url configured for chain %d", chainID)
		}
		client, err := ethclient.DialContext(ctx, url)
		if err != nil {
			closeAll(clients)
			return nil, fmt.Errorf("evm: new client set: dial chain %d: %w", chainID, err)
		}
		clients[chainID] = client
	}
	return &ClientSet{chains: chains, clients: clients}, nil
}

func closeAll(clients map[int64]*ethclient.Client) {
	for _, c := range clients {
		c.Close()
	}
}

// Close closes every underlying RPC connection. Safe to call once at
// process shutdown.
func (cs *ClientSet) Close() {
	closeAll(cs.clients)
}

func (cs *ClientSet) client(chainID int64) (*ethclient.Client, error) {
	c, ok := cs.clients[chainID]
	if !ok {
		return nil, fmt.Errorf("evm: chain %d: %w", chainID, ErrChainNotConfigured)
	}
	return c, nil
}

func (cs *ClientSet) chainConfig(chainID int64) (core.ChainConfig, error) {
	cfg, ok := cs.chains[chainID]
	if !ok {
		return core.ChainConfig{}, fmt.Errorf("evm: chain %d: %w", chainID, ErrChainNotConfigured)
	}
	return cfg, nil
}
