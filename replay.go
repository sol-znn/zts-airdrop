package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"

	"github.com/zenon-network/go-zenon/common/types"
)

// replayScan runs an airdrop from a saved scan log instead of scanning the
// chain.
//
// The reason to have this at all is that the recipient list and the airdrop
// are two decisions, and they do not have to be made at the same moment. A
// scan is reviewed, argued about, maybe sat on for a day; the range it covered
// has long since stopped being "the last N momentums" by the time anybody
// agrees to fund it. Replaying the log pays the list that was actually
// approved, rather than whatever a fresh scan of a drifted range would now
// return.
//
// It also makes the expensive half free. A scan of a large range costs minutes
// and thousands of RPC calls; a replay costs one file read, so recovering an
// interrupted airdrop does not mean re-deriving the list it was working from.
//
// Everything sent comes from the log:
//
//	recipients   filter.recipients if the log has them, else addresses
//	token        airdrop.zts
//	amount       airdrop.amountRaw — the exact smallest-unit value, never
//	             re-parsed from the displayed form, so no decimal conversion
//	             happens twice and the replay cannot drift by a factor of ten
//	range        range.from/to, which feed the run key, so a replay resumes
//	             the original run's journal instead of starting a new one
//
// The key is not in the log and never will be; it comes from the flags as
// usual.
func replayScan(ctx context.Context, o *options) error {
	logged, err := ReadScanLog(o.replay)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "replay  %s\n", o.replay)
	fmt.Fprintf(os.Stderr, "scanned %s by %s %s\n",
		logged.StartedAt.Format("2006-01-02 15:04:05 MST"), logged.Tool, logged.ToolVersion)
	fmt.Fprintf(os.Stderr, "range   momentums %d..%d", logged.Range.From, logged.Range.To)
	if logged.Range.NamedBy != "" {
		fmt.Fprintf(os.Stderr, " (asked for as %s)", logged.Range.NamedBy)
	}
	fmt.Fprintln(os.Stderr)

	// --- the recipient list ---

	recipients, label, digest, err := replayList(logged)
	if err != nil {
		return fmt.Errorf("%s: %w", o.replay, err)
	}
	fmt.Fprintf(os.Stderr, "list    %d %s, digest verified (%s)\n", len(recipients), label, digest[:16])

	// --- what to send ---

	node := logged.Node
	if o.node != defaultNode {
		node = o.node
	}
	client := NewClient(node)

	ztsText, amountRaw := "", ""
	if logged.Airdrop != nil {
		ztsText, amountRaw = logged.Airdrop.ZTS, logged.Airdrop.AmountRaw
	}
	// Flags override the log. Overriding is a legitimate thing to want — the
	// same approved list, a different token or size — and because the run key
	// is derived from the token, amount and range together, an override
	// naturally starts a new run rather than colliding with the original.
	if o.ztsFlag != "" {
		ztsText = o.ztsFlag
	}
	if ztsText == "" {
		return fmt.Errorf("%s records no token; pass -zts", o.replay)
	}
	zts, err := types.ParseZTS(ztsText)
	if err != nil {
		return fmt.Errorf("bad token standard %q: %w", ztsText, err)
	}
	token, err := client.TokenByZTS(ctx, zts)
	if err != nil {
		return err
	}

	var amount *big.Int
	switch {
	case o.amount != "":
		if amount, err = ParseAmount(o.amount, token.Decimals); err != nil {
			return err
		}
	case amountRaw != "":
		// The raw value goes back on the wire exactly as it was recorded. It
		// is deliberately not re-derived from the displayed amount: that would
		// run the decimal conversion a second time, which is the one place a
		// bulk transfer can silently change size.
		v, ok := new(big.Int).SetString(amountRaw, 10)
		if !ok || v.Sign() <= 0 {
			return fmt.Errorf("%s records an unusable amount %q", o.replay, amountRaw)
		}
		amount = v
	default:
		return fmt.Errorf("%s records no amount; pass -amount", o.replay)
	}

	// --- the key ---

	var (
		sender *Sender
		self   types.Address
	)
	sending := o.confirm && !o.dryRun
	if src, keyErr := keySource(o); keyErr == nil {
		key, err := LoadKey(src)
		if err != nil {
			return err
		}
		tip, err := client.FrontierMomentum(ctx)
		if err != nil {
			return fmt.Errorf("reach node: %w", err)
		}
		if logged.Chain != 0 && tip.ChainIdentifier != logged.Chain {
			return fmt.Errorf("scan log is from chain %d but %s is chain %d",
				logged.Chain, node, tip.ChainIdentifier)
		}
		sender = NewSender(client, key, tip.ChainIdentifier)
		self = sender.Address()
	} else if sending {
		return keyErr
	}

	// Paying from a different address than the one that ran the scan is
	// allowed — a treasury key is often not the key that did the reading — but
	// it is worth saying out loud, because the other possibility is that the
	// wrong -index was passed.
	if sender != nil && logged.Airdrop != nil && logged.Airdrop.Sender != "" &&
		logged.Airdrop.Sender != self.String() {
		fmt.Fprintf(os.Stderr, "note    the log was written for sender %s; paying from %s instead\n",
			logged.Airdrop.Sender, self)
	}

	// A log whose airdrop was already published is the case worth flagging.
	// The journal is what actually prevents a double payment, so the check
	// that matters is whether the journal still accounts for those sends.
	if logged.Airdrop != nil && logged.Airdrop.Published && logged.Airdrop.Sent > 0 {
		run := RunKey(zts, amount, logged.Range.From, logged.Range.To)
		paid, err := Paid(o.journalPath, run)
		if err != nil {
			return err
		}
		if len(paid) < logged.Airdrop.Sent {
			fmt.Fprintf(os.Stderr,
				"\nWARNING  this log records %d sends already published, but the journal\n"+
					"         at %s accounts for only %d of them.\n"+
					"         Replaying will pay the unaccounted addresses again. Point\n"+
					"         -journal at the file the original run used if it still exists.\n",
				logged.Airdrop.Sent, o.journalPath, len(paid))
		}
	}

	if o.outFile != "" {
		if err := writeList(recipients, o.outFile, false); err != nil {
			return err
		}
	}

	return planAndSend(ctx, o, &runPlan{
		client:     client,
		sender:     sender,
		self:       self,
		zts:        zts,
		token:      token,
		amount:     amount,
		recipients: recipients,
		// The original range, not anything about now: it feeds the run key,
		// so carrying it forward is what makes a replay resume the original
		// journal instead of paying everybody a second time.
		from:    logged.Range.From,
		to:      logged.Range.To,
		sending: sending,
		scanLog: nil,
	})
}

// replayList picks the address list a log should be replayed against and
// checks it against the digest recorded beside it.
//
// A log that ran filters recorded both the raw scan and the narrowed set. The
// narrowed set is the one somebody approved, so it is the one replayed.
//
// The digest check costs nothing and catches the one failure that would
// otherwise be silent: a log edited by hand, or truncated by a full disk,
// paying a set nobody ever approved.
func replayList(logged *ScanLog) (addrs []types.Address, label, digest string, err error) {
	source := logged.Addresses
	digest = logged.Digest
	label = "scanned addresses"
	if logged.Filter != nil && len(logged.Filter.Recipients) > 0 {
		source, digest = logged.Filter.Recipients, logged.Filter.RecipientDigest
		label = "filtered recipients"
	}
	if len(source) == 0 {
		return nil, label, digest, errors.New("records no addresses")
	}

	addrs = make([]types.Address, 0, len(source))
	for _, s := range source {
		addr, perr := types.ParseAddress(s)
		if perr != nil {
			return nil, label, digest, fmt.Errorf("%q is not an address: %w", s, perr)
		}
		addrs = append(addrs, addr)
	}
	if got := AddressDigest(addrs); got != digest {
		return nil, label, digest, fmt.Errorf(
			"inconsistent: its %s digest is %s but the list hashes to %s", label, digest, got)
	}
	return addrs, label, digest, nil
}
