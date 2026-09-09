package main

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/zenon-network/go-zenon/chain/nom"
	"github.com/zenon-network/go-zenon/common/types"
	"github.com/zenon-network/go-zenon/pow"
	"github.com/zenon-network/go-zenon/wallet"
)

const (
	blockVersion         = 1
	blockTypeUserSend    = 2
	blockTypeUserReceive = 3
)

// Sender builds, signs and publishes one send at a time from a single
// account.
//
// The hard part of a sequential airdrop is not the signing, it is the account
// chain. Every block names its predecessor's hash, so block N+1 cannot be
// built until the node has accepted block N and reports it as the frontier.
// Publishing returns before that is necessarily visible, so the sender keeps
// the hash it just published and waits for the node's frontier to catch up to
// it before building the next one. Skipping that wait produces a block whose
// previousHash points at a block the node still thinks is not the tip, and
// the node rejects it with an error that says nothing about why.
type Sender struct {
	client *Client
	key    *wallet.KeyPair
	addr   types.Address
	chain  uint64

	// FrontierWait bounds how long to wait for the node to report a block
	// just published as the account frontier.
	FrontierWait time.Duration

	// lastHash is the hash published by the previous Send, or zero before the
	// first one.
	lastHash types.Hash
}

func NewSender(client *Client, key *wallet.KeyPair, chainID uint64) *Sender {
	return &Sender{
		client:       client,
		key:          key,
		addr:         types.PubKeyToAddress(key.Public),
		chain:        chainID,
		FrontierWait: 90 * time.Second,
	}
}

func (s *Sender) Address() types.Address { return s.addr }

// Send publishes one transfer and returns its hash.
func (s *Sender) Send(ctx context.Context, to types.Address, amount *big.Int, zts types.ZenonTokenStandard) (types.Hash, error) {
	if amount == nil {
		amount = big.NewInt(0)
	}

	height, previous, err := s.frontier(ctx)
	if err != nil {
		return types.ZeroHash, err
	}

	momentum, err := s.client.FrontierMomentum(ctx)
	if err != nil {
		return types.ZeroHash, fmt.Errorf("frontier momentum: %w", err)
	}

	block := &nom.AccountBlock{
		Version:              blockVersion,
		ChainIdentifier:      s.chain,
		BlockType:            blockTypeUserSend,
		PreviousHash:         previous,
		Height:               height,
		MomentumAcknowledged: types.HashHeight{Hash: momentum.Hash, Height: momentum.Height},
		Address:              s.addr,
		ToAddress:            to,
		Amount:               amount,
		TokenStandard:        zts,
		FromBlockHash:        types.ZeroHash,
		Data:                 nil,
		PublicKey:            s.key.Public,
	}

	if err := s.payPlasma(ctx, block); err != nil {
		return types.ZeroHash, err
	}

	block.Hash = block.ComputeHash()
	block.Signature = s.key.Sign(block.Hash.Bytes())

	if err := s.client.PublishRawTransaction(ctx, block); err != nil {
		return types.ZeroHash, fmt.Errorf("publish: %w", err)
	}
	s.lastHash = block.Hash
	return block.Hash, nil
}

// Receive takes delivery of one send addressed to this account.
//
// A receive carries no value fields of its own: the amount, token and sender
// all come from the block it names, so filling them in here would either be
// ignored or rejected.
func (s *Sender) Receive(ctx context.Context, from types.Hash) (types.Hash, error) {
	height, previous, err := s.frontier(ctx)
	if err != nil {
		return types.ZeroHash, err
	}
	momentum, err := s.client.FrontierMomentum(ctx)
	if err != nil {
		return types.ZeroHash, fmt.Errorf("frontier momentum: %w", err)
	}

	block := &nom.AccountBlock{
		Version:              blockVersion,
		ChainIdentifier:      s.chain,
		BlockType:            blockTypeUserReceive,
		PreviousHash:         previous,
		Height:               height,
		MomentumAcknowledged: types.HashHeight{Hash: momentum.Hash, Height: momentum.Height},
		Address:              s.addr,
		ToAddress:            types.ZeroAddress,
		Amount:               big.NewInt(0),
		TokenStandard:        types.ZeroTokenStandard,
		FromBlockHash:        from,
		Data:                 nil,
		PublicKey:            s.key.Public,
	}
	if err := s.payPlasma(ctx, block); err != nil {
		return types.ZeroHash, err
	}
	block.Hash = block.ComputeHash()
	block.Signature = s.key.Sign(block.Hash.Bytes())
	if err := s.client.PublishRawTransaction(ctx, block); err != nil {
		return types.ZeroHash, fmt.Errorf("publish receive: %w", err)
	}
	s.lastHash = block.Hash
	return block.Hash, nil
}

// ReceiveAll collects every pending send addressed to this account, returning
// how many it took delivery of.
//
// The list is re-fetched each round rather than paged through in one pass:
// receiving changes the list, and a cursor into a list that is being consumed
// walks off the end of it.
func (s *Sender) ReceiveAll(ctx context.Context, out io.Writer) (int, error) {
	received := 0
	for {
		pending, err := s.client.UnreceivedBlocks(ctx, s.addr, 0, 50)
		if err != nil {
			return received, fmt.Errorf("unreceived blocks: %w", err)
		}
		if len(pending) == 0 {
			return received, nil
		}
		for _, b := range pending {
			select {
			case <-ctx.Done():
				return received, ctx.Err()
			default:
			}
			hash, err := s.Receive(ctx, b.Hash)
			if err != nil {
				return received, fmt.Errorf("receive %s: %w", b.Hash, err)
			}
			received++
			if out != nil {
				fmt.Fprintf(out, "receive %s of %s in %s\n",
					b.Amount.Int, b.TokenStandard, hash.String()[:12])
			}
		}
	}
}

// frontier returns the height and previous hash for the next block, waiting
// for the node to acknowledge the block published just before it.
//
// An account that has never published starts at height 1 with a zero previous
// hash, which is also what a node with no record of the account reports.
func (s *Sender) frontier(ctx context.Context) (uint64, types.Hash, error) {
	deadline := time.Now().Add(s.FrontierWait)
	for {
		b, err := s.client.FrontierAccountBlock(ctx, s.addr)
		if err != nil {
			return 0, types.ZeroHash, fmt.Errorf("frontier account block: %w", err)
		}
		switch {
		case b == nil:
			if s.lastHash.IsZero() {
				return 1, types.ZeroHash, nil
			}
		case s.lastHash.IsZero() || b.Hash == s.lastHash:
			return b.Height + 1, b.Hash, nil
		}

		// The node has not caught up to what was just published. Waiting is
		// the only correct response: building on a stale frontier forks the
		// account chain.
		if time.Now().After(deadline) {
			return 0, types.ZeroHash, fmt.Errorf(
				"node did not report block %s as the account frontier within %s",
				s.lastHash, s.FrontierWait)
		}
		select {
		case <-ctx.Done():
			return 0, types.ZeroHash, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// payPlasma fills in fusedPlasma, difficulty and nonce.
//
// The split between fused plasma and proof of work is the node's call, not
// ours: under dynamic plasma the price moves per momentum, so asking is the
// only way to be right. For an airdrop the difference is the whole runtime —
// a fused account sends as fast as the node accepts blocks, an unfused one
// pays seconds of PoW per recipient.
func (s *Sender) payPlasma(ctx context.Context, block *nom.AccountBlock) error {
	req, err := s.client.RequiredPoW(ctx, block.Address, block.ToAddress, block.BlockType, block.Data)
	if err != nil {
		return fmt.Errorf("required plasma: %w", err)
	}

	if req.RequiredDifficulty == 0 {
		block.FusedPlasma = req.BasePlasma
		block.Difficulty = 0
		block.Nonce = nom.Nonce{}
		return nil
	}

	block.FusedPlasma = req.AvailablePlasma
	block.Difficulty = req.RequiredDifficulty

	// The PoW preimage is address ‖ previousHash — deliberately not the block
	// hash, so the nonce can be found before the rest of the block is final.
	nonce := pow.GetPoWNonce(new(big.Int).SetUint64(req.RequiredDifficulty), pow.GetAccountBlockHash(block))
	copy(block.Nonce.Data[:], nonce)
	return nil
}

// EstimatePlasma reports what one airdrop send would cost from this account
// right now, so the run can warn about a multi-hour PoW grind before it
// starts rather than after the first few recipients.
func (s *Sender) EstimatePlasma(ctx context.Context, to types.Address) (*RequiredPlasma, error) {
	return s.client.RequiredPoW(ctx, s.addr, to, blockTypeUserSend, nil)
}
