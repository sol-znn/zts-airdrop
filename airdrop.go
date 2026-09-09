package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/zenon-network/go-zenon/common/types"
)

// Airdrop sends a fixed amount to each recipient, one at a time.
//
// Sequential is not a simplification here, it is what the ledger allows. Every
// block from an account names its predecessor, so two sends in flight at once
// from the same address are two claims on the same height and one of them
// loses. Concurrency would have to come from several funded addresses, which
// is a different tool.
type Airdrop struct {
	Sender  *Sender
	Journal *Journal

	Token  *Token
	ZTS    types.ZenonTokenStandard
	Amount *big.Int // per recipient, smallest unit

	// Delay is an optional pause between sends, for holding back from a
	// public node that rate-limits.
	Delay time.Duration

	// StopAfterErrors ends the run once this many consecutive sends have
	// failed. A run that is failing every send is not going to recover by
	// being allowed to fail four hundred more times.
	StopAfterErrors int

	Out io.Writer
}

// Report is the outcome of a run.
type Report struct {
	Sent    int
	Skipped int // already in the journal for this run
	Failed  int
	Total   *big.Int
	Elapsed time.Duration
	Errors  []error
}

// Run pays each recipient in turn, printing a line per send.
//
// A failed send does not end the run by default. Recipients are independent —
// nobody's payment depends on anybody else's — so one address that trips a
// node-side error should not cost the remaining hundred their share. The
// failures are collected and reported at the end, and because they were never
// journalled, re-running resumes exactly them.
func (a *Airdrop) Run(ctx context.Context, recipients []types.Address, alreadyPaid map[types.Address]string) (*Report, error) {
	rep := &Report{Total: big.NewInt(0)}
	start := time.Now()
	consecutive := 0

	for i, addr := range recipients {
		select {
		case <-ctx.Done():
			rep.Elapsed = time.Since(start)
			return rep, ctx.Err()
		default:
		}

		n := i + 1
		if hash, ok := alreadyPaid[addr]; ok {
			rep.Skipped++
			fmt.Fprintf(a.Out, "[%*d/%d] %s  skip (paid in %s)\n",
				width(len(recipients)), n, len(recipients), addr, short(hash))
			continue
		}

		sendStart := time.Now()
		hash, err := a.Sender.Send(ctx, addr, a.Amount, a.ZTS)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				rep.Elapsed = time.Since(start)
				return rep, err
			}
			rep.Failed++
			consecutive++
			rep.Errors = append(rep.Errors, fmt.Errorf("%s: %w", addr, err))
			fmt.Fprintf(a.Out, "[%*d/%d] %s  FAILED: %v\n",
				width(len(recipients)), n, len(recipients), addr, err)
			if a.StopAfterErrors > 0 && consecutive >= a.StopAfterErrors {
				rep.Elapsed = time.Since(start)
				return rep, fmt.Errorf("%d sends in a row failed; stopping", consecutive)
			}
			continue
		}
		consecutive = 0

		// Journalled before the next send is built, so an interruption at
		// worst loses the record of a send that succeeded — never records a
		// send that did not happen.
		if a.Journal != nil {
			if err := a.Journal.Record(n, addr, a.Amount, a.ZTS, hash); err != nil {
				return rep, fmt.Errorf("journal write failed after paying %s in %s: %w", addr, hash, err)
			}
		}

		rep.Sent++
		rep.Total.Add(rep.Total, a.Amount)

		remaining := len(recipients) - n
		fmt.Fprintf(a.Out, "[%*d/%d] %s  %s %s  %s  %.1fs  eta %s\n",
			width(len(recipients)), n, len(recipients), addr,
			FormatAmount(a.Amount, a.Token.Decimals), a.Token.Symbol,
			short(hash.String()), time.Since(sendStart).Seconds(),
			eta(time.Since(start), rep.Sent, remaining))

		if a.Delay > 0 && remaining > 0 {
			select {
			case <-ctx.Done():
				rep.Elapsed = time.Since(start)
				return rep, ctx.Err()
			case <-time.After(a.Delay):
			}
		}
	}

	rep.Elapsed = time.Since(start)
	return rep, nil
}

// eta extrapolates from the sends that have actually completed. It is
// deliberately based on measured sends rather than a fixed per-send estimate,
// because the two plasma regimes differ by orders of magnitude and the run
// only learns which one it is in by doing one.
func eta(elapsed time.Duration, done, remaining int) string {
	if done == 0 || remaining == 0 {
		return "—"
	}
	per := elapsed / time.Duration(done)
	return (per * time.Duration(remaining)).Round(time.Second).String()
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func width(n int) int {
	w := 1
	for n >= 10 {
		n /= 10
		w++
	}
	return w
}
