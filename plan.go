package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/zenon-network/go-zenon/common/types"
)

// runPlan is everything the plan-and-send stage needs, gathered by whichever
// path produced it.
//
// Two paths produce one: a fresh scan, and a replay of a saved scan log. They
// share this stage rather than each having their own copy, because the parts
// that matter — the balance check, the resume lookup, the journal, the
// per-send loop — are exactly the parts where two divergent implementations
// would eventually disagree about who has been paid.
type runPlan struct {
	client *Client

	// sender is nil when no key is configured, which is allowed for sizing.
	sender *Sender
	self   types.Address

	zts    types.ZenonTokenStandard
	token  *Token
	amount *big.Int

	recipients []types.Address

	// from and to identify the momentum range the recipients came from. They
	// are part of the run key, so a replay must carry the *original* range
	// rather than anything about when the replay happened — otherwise the
	// replay would compute a different key and pay everybody again.
	from, to uint64

	sending bool

	// scanLog is the log this run will write. On a replay it is nil: a replay
	// scanned nothing, and writing a log that claimed otherwise would put a
	// scan in the record that never happened.
	scanLog *ScanLog
}

// hasLog reports whether there is a scan log to annotate.
func (p *runPlan) hasLog() bool { return p.scanLog != nil }

// planAndSend prices the run, prints it, and — only under -confirm — publishes
// it.
func planAndSend(ctx context.Context, o *options, p *runPlan) error {
	// --- the plan ---

	fmt.Fprintln(os.Stderr)
	if p.sender != nil {
		fmt.Fprintf(os.Stderr, "from    %s\n", p.self)
	} else {
		fmt.Fprintf(os.Stderr, "from    (no key configured; sizing only)\n")
	}

	// Sizing with no token chosen stops here: the recipient count is the
	// answer, and everything below it is denominated in a token that has not
	// been named.
	if p.token == nil || p.amount == nil {
		fmt.Fprintf(os.Stderr, "to      %d addresses would receive the airdrop\n", len(p.recipients))
		if p.amount == nil && p.token != nil {
			fmt.Fprintf(os.Stderr, "token   %s (%s), %d decimals — pass -amount to price the run\n",
				p.token.Symbol, p.zts, p.token.Decimals)
		} else {
			fmt.Fprintf(os.Stderr, "        pass -zts and -amount to price the run\n")
		}
		return nil
	}

	total := new(big.Int).Mul(p.amount, big.NewInt(int64(len(p.recipients))))

	run := RunKey(p.zts, p.amount, p.from, p.to)
	paid, err := Paid(o.journalPath, run)
	if err != nil {
		return err
	}
	outstanding := 0
	for _, addr := range p.recipients {
		if _, ok := paid[addr]; !ok {
			outstanding++
		}
	}
	needed := new(big.Int).Mul(p.amount, big.NewInt(int64(outstanding)))

	// A replay writes no scan log — it scanned nothing, and a log claiming
	// otherwise would put a scan in the record that never happened.
	if p.hasLog() {
		p.scanLog.Airdrop = &ScanAirdrop{
			ZTS: p.zts.String(), Symbol: p.token.Symbol, Decimals: p.token.Decimals,
			AmountRaw: p.amount.String(), Amount: FormatAmount(p.amount, p.token.Decimals),
			TotalRaw: total.String(), Total: FormatAmount(total, p.token.Decimals),
			RunKey: run, Journal: o.journalPath, AlreadyPaid: len(paid),
		}
		if p.sender != nil {
			p.scanLog.Airdrop.Sender = p.self.String()
		}
	}

	fmt.Fprintf(os.Stderr, "token   %s (%s), %d decimals\n", p.token.Symbol, p.zts, p.token.Decimals)
	fmt.Fprintf(os.Stderr, "each    %s %s\n", FormatAmount(p.amount, p.token.Decimals), p.token.Symbol)
	fmt.Fprintf(os.Stderr, "to      %d addresses, total %s %s\n",
		len(p.recipients), FormatAmount(total, p.token.Decimals), p.token.Symbol)
	if len(paid) > 0 {
		fmt.Fprintf(os.Stderr, "resume  %d already paid in run %s; %d outstanding, needing %s %s\n",
			len(paid), run, outstanding, FormatAmount(needed, p.token.Decimals), p.token.Symbol)
	}

	// Receiving before the balance is read, not after: a supply that has been
	// minted but not collected shows as zero, and an airdrop that checked the
	// balance first would refuse to run on funds it is about to hold.
	if p.sender != nil && o.receive {
		n, err := p.sender.ReceiveAll(ctx, os.Stderr)
		if err != nil {
			return fmt.Errorf("receiving pending blocks: %w", err)
		}
		fmt.Fprintf(os.Stderr, "receive %d pending blocks collected\n", n)
	}

	if p.sender != nil {
		balance, err := p.client.Balance(ctx, p.self, p.zts)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "balance %s %s\n", FormatAmount(balance, p.token.Decimals), p.token.Symbol)

		if balance.Cmp(needed) < 0 {
			missing := new(big.Int).Sub(needed, balance)
			// A shortfall is fatal for a real run and merely a finding for a
			// sizing run — reporting how much is missing is the point of
			// sizing, so it must not exit here.
			msg := fmt.Sprintf("balance is short by %s %s — fund %s, or re-run with -receive to collect its pending blocks",
				FormatAmount(missing, p.token.Decimals), p.token.Symbol, p.self)
			if p.sending {
				return errors.New(msg)
			}
			fmt.Fprintf(os.Stderr, "short   %s\n", msg)
		}

		// Plasma decides whether this run takes a minute or a week, so it is
		// worth saying before the first send rather than letting it be
		// inferred from a crawling progress line.
		if plasma, err := p.sender.EstimatePlasma(ctx, p.recipients[0]); err == nil {
			if plasma.RequiredDifficulty > 0 {
				fmt.Fprintf(os.Stderr, "plasma  none fused: every send needs proof of work (difficulty %d).\n"+
					"        Fuse QSR to %s to make this fast.\n", plasma.RequiredDifficulty, p.self)
			} else {
				fmt.Fprintf(os.Stderr, "plasma  fused, %d available against %d needed per send\n",
					plasma.AvailablePlasma, plasma.BasePlasma)
			}
		}
	}

	if !p.sending {
		if o.dryRun && o.confirm {
			fmt.Fprintf(os.Stderr, "\nDRY RUN — -dry-run overrides -confirm; nothing was published.\n")
		} else {
			fmt.Fprintf(os.Stderr, "\nDRY RUN — nothing was published. Re-run with -confirm to send.\n")
		}
		for i, addr := range p.recipients {
			mark := ""
			if _, ok := paid[addr]; ok {
				mark = "  (already paid)"
			}
			fmt.Printf("%d\t%s\t%s%s\n", i+1, addr, FormatAmount(p.amount, p.token.Decimals), mark)
		}
		return nil
	}

	// --- send ---

	journal, err := OpenJournal(o.journalPath, run)
	if err != nil {
		return err
	}
	defer journal.Close()

	fmt.Fprintf(os.Stderr, "\nsending (run %s, journal %s)\n\n", run, o.journalPath)

	drop := &Airdrop{
		Sender:          p.sender,
		Journal:         journal,
		Token:           p.token,
		ZTS:             p.zts,
		Amount:          p.amount,
		Delay:           o.delay,
		StopAfterErrors: o.stopAfter,
		Out:             os.Stdout,
	}
	rep, runErr := drop.Run(ctx, p.recipients, paid)

	if p.hasLog() {
		p.scanLog.Airdrop.Published = true
		p.scanLog.Airdrop.Sent = rep.Sent
		p.scanLog.Airdrop.Skipped = rep.Skipped
		p.scanLog.Airdrop.Failed = rep.Failed
	}

	fmt.Fprintf(os.Stderr, "\nsent %d, skipped %d, failed %d in %s — %s %s moved\n",
		rep.Sent, rep.Skipped, rep.Failed, rep.Elapsed.Round(time.Second),
		FormatAmount(rep.Total, p.token.Decimals), p.token.Symbol)
	if len(rep.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "\nfailures (re-run with the same flags to retry exactly these):\n")
		for _, e := range rep.Errors {
			fmt.Fprintf(os.Stderr, "  %v\n", e)
		}
	}
	if runErr != nil {
		return runErr
	}
	if rep.Failed > 0 {
		return fmt.Errorf("%d sends failed", rep.Failed)
	}
	return nil
}
