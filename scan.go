package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/zenon-network/go-zenon/common/types"
)

// Scanner walks a range of momentums and collects the distinct user addresses
// that acted in it.
//
// "Acted" means authored an account block. That is the definition that
// survives contact with the ledger: a momentum's content lists the author of
// every block it confirmed, and an author is by construction someone who held
// a key and signed. Recipients are a different population — they took no
// action and may not exist as accounts at all — so they are opt-in via
// IncludeRecipients rather than folded into the same set.
//
// Embedded contracts are dropped. They author blocks constantly (every
// autoreceive and callback is one), they cannot spend what they are sent
// through an ordinary transfer, and an airdrop that funds them is an airdrop
// that burns that share. The test is a single byte of the address, not a
// lookup against a hardcoded list, so a contract added after this was written
// is still excluded.
type Scanner struct {
	Client *Client

	// IncludeRecipients also counts the toAddress of send blocks. It forces
	// the heavier detailed-momentum call, because a recipient is only
	// recorded in a block body.
	IncludeRecipients bool

	// Progress receives a line per batch. Nil silences it.
	Progress io.Writer
}

// ScanResult is what a scan found, along with the counts needed to explain the
// number of recipients to whoever is about to spend money on them.
type ScanResult struct {
	FromHeight uint64
	ToHeight   uint64
	Momentums  int
	Blocks     int
	Contracts  int // blocks skipped because an embedded contract authored them
	Addresses  []types.Address
	Elapsed    time.Duration
}

// Scan collects unique user addresses over [from, to] inclusive.
func (s *Scanner) Scan(ctx context.Context, from, to uint64) (*ScanResult, error) {
	if from == 0 {
		// Momentum heights start at 1; height 0 is the genesis sentinel and
		// the node rejects a request for it.
		from = 1
	}
	if to < from {
		return nil, fmt.Errorf("empty range: to (%d) is below from (%d)", to, from)
	}

	res := &ScanResult{FromHeight: from, ToHeight: to}
	seen := make(map[types.Address]struct{})
	start := time.Now()

	// Detailed momentums carry every block body in the range, so they are
	// fetched in much smaller batches — a 1024-momentum batch of bodies is a
	// response large enough to time out on a busy chain.
	batch := uint64(maxCountSize)
	if s.IncludeRecipients {
		batch = 100
	}

	for height := from; height <= to; {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		count := batch
		if remaining := to - height + 1; remaining < count {
			count = remaining
		}

		got, err := s.scanBatch(ctx, height, count, seen, res)
		if err != nil {
			return nil, fmt.Errorf("scan from height %d: %w", height, err)
		}
		if got == 0 {
			// The node returned nothing for a range it should have covered,
			// which means the range runs past what it has. Stop rather than
			// spin on the same height forever.
			break
		}
		height += uint64(got)

		if s.Progress != nil {
			done := height - from
			total := to - from + 1
			fmt.Fprintf(s.Progress, "\rscanning %d/%d momentums  %d blocks  %d addresses",
				min(done, total), total, res.Blocks, len(seen))
		}
	}
	if s.Progress != nil {
		fmt.Fprintln(s.Progress)
	}

	res.Addresses = make([]types.Address, 0, len(seen))
	for addr := range seen {
		res.Addresses = append(res.Addresses, addr)
	}
	// Sorted so that two runs over the same range produce the same order, and
	// so a resumed airdrop walks the list in the order it walked before.
	sort.Slice(res.Addresses, func(i, j int) bool {
		return res.Addresses[i].String() < res.Addresses[j].String()
	})
	res.Elapsed = time.Since(start)
	return res, nil
}

// scanBatch fetches one batch and folds it into seen, returning how many
// momentums it actually covered.
func (s *Scanner) scanBatch(ctx context.Context, height, count uint64, seen map[types.Address]struct{}, res *ScanResult) (int, error) {
	add := func(addr types.Address) {
		if addr.IsZero() {
			return
		}
		if types.IsEmbeddedAddress(addr) {
			res.Contracts++
			return
		}
		seen[addr] = struct{}{}
	}

	if !s.IncludeRecipients {
		list, err := s.Client.MomentumsByHeight(ctx, height, count)
		if err != nil {
			return 0, err
		}
		for _, m := range list {
			res.Momentums++
			for _, h := range m.Content {
				res.Blocks++
				add(h.Address)
			}
		}
		return len(list), nil
	}

	list, err := s.Client.DetailedMomentumsByHeight(ctx, height, count)
	if err != nil {
		return 0, err
	}
	for _, dm := range list {
		res.Momentums++
		for _, b := range dm.AccountBlocks {
			res.Blocks++
			add(b.Address)
			// ToAddress is meaningful only on a send; a receive block carries
			// the zero address there, which add already ignores.
			add(b.ToAddress)
		}
	}
	return len(list), nil
}

// ResolveRange turns the two ways of naming a range — an absolute start
// height, or a count of momentums back from the tip — into a concrete
// [from, to]. The tip is read once here so that the range does not drift
// while the scan runs.
//
// The tip momentum is returned too: the scan log records the chain state the
// range was resolved against, which is what lets a later reader tell a scan
// that covered the whole chain from one that stopped short of it.
func ResolveRange(ctx context.Context, client *Client, fromHeight, lastN, toHeight uint64) (uint64, uint64, *Momentum, error) {
	tip, err := client.FrontierMomentum(ctx)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("reach node: %w", err)
	}

	to := tip.Height
	if toHeight > 0 {
		if toHeight > tip.Height {
			return 0, 0, nil, fmt.Errorf("-to %d is above the chain tip at %d", toHeight, tip.Height)
		}
		to = toHeight
	}

	var from uint64
	switch {
	case fromHeight > 0:
		from = fromHeight
	case lastN > 0:
		if lastN >= to {
			from = 1
		} else {
			from = to - lastN + 1
		}
	default:
		return 0, 0, nil, fmt.Errorf("pass -from <height> or -last <momentums>")
	}
	if from > to {
		return 0, 0, nil, fmt.Errorf("-from %d is above the end of the range at %d", from, to)
	}
	return from, to, tip, nil
}
