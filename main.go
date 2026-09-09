// Command zts-airdrop sends a fixed amount of a ZTS token to every user
// address that transacted on the Network of Momentum in a range of momentums.
//
//	zts-airdrop -last 8640 -zts zts1... -amount 10 -keystore w.json
//
// The default is a dry run. Nothing is published until -confirm is passed,
// because the run is a bulk irreversible transfer and the recipient list is
// derived from chain state rather than supplied by hand — the list is exactly
// the thing worth looking at before spending money on it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zenon-network/go-zenon/common/types"
)

const defaultNode = "http://localhost:35997"

// version is stamped at build time with -ldflags "-X main.version=...". It
// stays "dev" for a local build, so a binary that reports a tag is one that
// came from the release workflow and can be traced back to a commit.
var version = "dev"

// devnetMnemonic is the phrase committed to go-zenon's docker/devnet. It is
// public by design and worthless outside chain 69 — it is here so that
// -devnet works with no setup, and it must never be the default for any other
// network.
const devnetMnemonic = "abstract affair idle position alien fluid board ordinary exist afraid chapter wood wood guide sun walnut crew perfect place firm poverty model side million"

type options struct {
	node string

	// key source
	mnemonicFile string
	mnemonicEnv  string
	keystore     string
	password     string
	index        uint
	devnet       bool

	// what to send
	ztsFlag string
	amount  string

	// what to scan
	from uint64
	to   uint64
	last uint64

	includeRecipients bool
	excludeSelf       bool
	minHoldingFlag    string

	// how to run
	confirm     bool
	dryRun      bool
	receive     bool
	journalPath string
	delay       time.Duration
	stopAfter   int
	listOnly    bool
	outFile     string
	envFile     string
	showVersion bool
	scanLogDir  string
	verifyScan  string
	replay      string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\ninterrupted; re-run with the same flags to resume from the journal")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "zts-airdrop: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	var o options
	set := flag.NewFlagSet("zts-airdrop", flag.ExitOnError)
	set.Usage = func() { usage(set) }

	set.StringVar(&o.node, "node", defaultNode, "NoM node JSON-RPC endpoint")

	set.StringVar(&o.mnemonicFile, "mnemonic-file", "", "file holding a BIP-39 phrase")
	set.StringVar(&o.mnemonicEnv, "mnemonic-env", "", "environment variable holding a BIP-39 phrase")
	set.StringVar(&o.keystore, "keystore", "", "encrypted go-zenon keystore file")
	set.StringVar(&o.password, "password", "", "keystore password (prefer ZTS_AIRDROP_PASSWORD)")
	set.UintVar(&o.index, "index", 0, "account index under m/44'/73404'/i'")
	set.BoolVar(&o.devnet, "devnet", false, "use the public devnet mnemonic (chain 69 only)")

	set.StringVar(&o.ztsFlag, "zts", "", "token standard to airdrop, e.g. zts1znnxxxxxxxxxxxxx9z4ulx")
	set.StringVar(&o.amount, "amount", "", "amount per recipient, in whole token units")

	set.Uint64Var(&o.from, "from", 0, "first momentum height to scan")
	set.Uint64Var(&o.to, "to", 0, "last momentum height to scan (default: chain tip)")
	set.Uint64Var(&o.last, "last", 0, "scan the last N momentums instead of naming a start height")

	set.BoolVar(&o.includeRecipients, "include-recipients", false,
		"also count addresses that only received; slower, needs full block bodies")
	set.BoolVar(&o.excludeSelf, "exclude-self", true, "leave the sending address out of the recipient list")
	set.StringVar(&o.minHoldingFlag, "min-holding", "",
		"skip addresses holding less than this much of -zts already")

	set.BoolVar(&o.confirm, "confirm", false, "actually publish the sends (without this it is a dry run)")
	set.BoolVar(&o.receive, "receive", false,
		"take delivery of the sending address's pending blocks first, so a freshly minted supply is spendable")
	set.BoolVar(&o.dryRun, "dry-run", false,
		"size the airdrop and publish nothing, even with -confirm; -zts and -amount are optional")
	set.StringVar(&o.journalPath, "journal", "airdrop-journal.jsonl", "append-only record of what was paid; also the resume source")
	set.DurationVar(&o.delay, "delay", 0, "pause between sends, e.g. 200ms")
	set.IntVar(&o.stopAfter, "stop-after-errors", 10, "abort once this many sends fail in a row (0 disables)")
	set.BoolVar(&o.listOnly, "list", false, "print the recipient list and exit without touching a key")
	set.StringVar(&o.outFile, "out", "", "also write the recipient list to this file, one address per line")
	set.StringVar(&o.scanLogDir, "scan-log-dir", "scans",
		"directory for scan logs, written every run; empty disables them")
	set.StringVar(&o.replay, "replay", "",
		"airdrop the recipient list recorded in this scan log instead of scanning; -confirm publishes it")
	set.StringVar(&o.verifyScan, "verify-scan", "",
		"re-run the scan recorded in this log and report whether it reproduces")
	set.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	set.StringVar(&o.envFile, "env", ".env", "file of ZTS_AIRDROP_* settings; flags and the real environment win over it")

	if err := set.Parse(args); err != nil {
		return err
	}
	if o.showVersion {
		fmt.Println("zts-airdrop", version)
		return nil
	}

	// .env is read after parsing so that -env can name a different file, and
	// applied only to flags the command line left alone.
	if err := LoadDotEnv(o.envFile); err != nil {
		return err
	}
	if err := ApplyEnvDefaults(set); err != nil {
		return err
	}

	if o.verifyScan != "" {
		return verifyScan(ctx, &o)
	}
	if o.replay != "" {
		return replayScan(ctx, &o)
	}

	if len(args) == 0 && o.ztsFlag == "" && o.last == 0 && o.from == 0 {
		usage(set)
		return errors.New("nothing to do: pass flags or create a .env")
	}

	client := NewClient(o.node)

	from, to, tip, err := ResolveRange(ctx, client, o.from, o.last, o.to)
	if err != nil {
		return err
	}

	// --- scan ---

	fmt.Fprintf(os.Stderr, "node    %s\n", o.node)
	fmt.Fprintf(os.Stderr, "range   momentums %d..%d (%d)\n", from, to, to-from+1)

	scanner := &Scanner{Client: client, IncludeRecipients: o.includeRecipients, Progress: os.Stderr}
	scanStart := time.Now()
	res, err := scanner.Scan(ctx, from, to)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "found   %d user addresses in %d blocks across %d momentums (%d contract blocks skipped) in %s\n",
		len(res.Addresses), res.Blocks, res.Momentums, res.Contracts, res.Elapsed.Round(time.Millisecond))

	// The log is built now and written on the way out, whatever happens next.
	// A scan that ran is a fact worth keeping even when the airdrop that
	// followed it failed — that is exactly the run somebody will want to
	// reconstruct afterwards.
	scanLog := NewScanLog(o.node, tip.ChainIdentifier,
		ScanTip{Height: tip.Height, Hash: tip.Hash.String()},
		rangeNamedBy(&o), ScanOptions{IncludeRecipients: o.includeRecipients},
		res, scanStart)
	defer func() {
		path, err := scanLog.Write(o.scanLogDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scan log: %v\n", err)
			return
		}
		if path != "" {
			fmt.Fprintf(os.Stderr, "scanlog %s  digest %s\n", path, scanLog.Digest[:16])
		}
	}()

	if o.listOnly {
		return writeList(res.Addresses, o.outFile, true)
	}

	// sending is the one mode that needs both halves of the configuration.
	// A sizing run is allowed to be missing either: the point of it is to
	// answer "how many, and how much would that cost" before a token has been
	// chosen or a key is anywhere near the machine.
	sending := o.confirm && !o.dryRun

	// --- what is being sent ---

	var (
		zts    types.ZenonTokenStandard
		token  *Token
		amount *big.Int
	)
	if o.ztsFlag != "" {
		zts, err = types.ParseZTS(o.ztsFlag)
		if err != nil {
			return fmt.Errorf("bad -zts: %w", err)
		}
		if token, err = client.TokenByZTS(ctx, zts); err != nil {
			return err
		}
		if o.amount != "" {
			if amount, err = ParseAmount(o.amount, token.Decimals); err != nil {
				return err
			}
		}
	}
	if sending {
		if token == nil {
			return errors.New("pass -zts <token standard>")
		}
		if amount == nil {
			return errors.New("pass -amount <per recipient>")
		}
	}

	// --- the key ---

	// A dry run loads the key only if one is configured. Without it the run
	// still reports the recipient count and the total, and only the
	// sender-specific lines — balance, plasma, self-exclusion — drop out.
	var (
		sender *Sender
		self   types.Address
	)
	if src, keyErr := keySource(&o); keyErr == nil {
		key, err := LoadKey(src)
		if err != nil {
			return err
		}
		tip, err := client.FrontierMomentum(ctx)
		if err != nil {
			return fmt.Errorf("reach node: %w", err)
		}
		sender = NewSender(client, key, tip.ChainIdentifier)
		self = sender.Address()
	} else if sending {
		return keyErr
	}

	recipients := res.Addresses
	if o.excludeSelf && sender != nil {
		recipients = without(recipients, self)
	}
	if o.minHoldingFlag != "" {
		if token == nil {
			return errors.New("-min-holding needs -zts, to know which token to weigh")
		}
		floor, err := ParseAmount(o.minHoldingFlag, token.Decimals)
		if err != nil {
			return fmt.Errorf("bad -min-holding: %w", err)
		}
		recipients, err = filterByHolding(ctx, client, recipients, zts, floor)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "filter  %d addresses hold at least %s %s\n",
			len(recipients), FormatAmount(floor, token.Decimals), token.Symbol)
	}
	selfLabel := ""
	if sender != nil && o.excludeSelf {
		selfLabel = self.String()
	}
	scanLog.SetFilter(selfLabel, o.minHoldingFlag, recipients)

	if len(recipients) == 0 {
		return errors.New("no recipients in that range")
	}
	if o.outFile != "" {
		if err := writeList(recipients, o.outFile, false); err != nil {
			return err
		}
	}

	return planAndSend(ctx, &o, &runPlan{
		client:     client,
		sender:     sender,
		self:       self,
		zts:        zts,
		token:      token,
		amount:     amount,
		recipients: recipients,
		from:       from,
		to:         to,
		sending:    sending,
		scanLog:    scanLog,
	})
}

// keySource resolves the key flags. A phrase is never accepted as a
// command-line argument: arguments land in shell history and in the process
// table, where any local user can read them.
func keySource(o *options) (KeySource, error) {
	src := KeySource{Index: uint32(o.index)}
	switch {
	case o.devnet:
		src.Mnemonic = devnetMnemonic
	case o.mnemonicFile != "":
		m, err := ReadMnemonicFile(o.mnemonicFile)
		if err != nil {
			return src, err
		}
		src.Mnemonic = m
	case o.mnemonicEnv != "":
		m := os.Getenv(o.mnemonicEnv)
		if m == "" {
			return src, fmt.Errorf("environment variable %s is empty", o.mnemonicEnv)
		}
		src.Mnemonic = m
	case o.keystore != "":
		src.KeyFilePath = o.keystore
		src.Password = o.password
		if env := os.Getenv("ZTS_AIRDROP_PASSWORD"); env != "" {
			src.Password = env
		}
		if src.Password == "" {
			return src, errors.New("keystore needs a password: set ZTS_AIRDROP_PASSWORD or pass -password")
		}
	default:
		// The .env path: a phrase set as ZTS_AIRDROP_MNEMONIC needs no flag
		// at all, which is the whole point of having a config file.
		if m := os.Getenv(envPrefix + "MNEMONIC"); m != "" {
			src.Mnemonic = m
			break
		}
		return src, errors.New("no key source: pass -devnet, -mnemonic-file, -mnemonic-env, or -keystore, " +
			"or set ZTS_AIRDROP_MNEMONIC in .env")
	}
	return src, nil
}

// filterByHolding keeps only the addresses already holding at least floor of
// the token. It costs one call per address, which is why it is opt-in.
func filterByHolding(ctx context.Context, client *Client, addrs []types.Address, zts types.ZenonTokenStandard, floor *big.Int) ([]types.Address, error) {
	kept := make([]types.Address, 0, len(addrs))
	for i, addr := range addrs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		bal, err := client.Balance(ctx, addr, zts)
		if err != nil {
			return nil, fmt.Errorf("balance of %s: %w", addr, err)
		}
		if bal.Cmp(floor) >= 0 {
			kept = append(kept, addr)
		}
		fmt.Fprintf(os.Stderr, "\rchecking holdings %d/%d", i+1, len(addrs))
	}
	fmt.Fprintln(os.Stderr)
	return kept, nil
}

func without(addrs []types.Address, drop types.Address) []types.Address {
	out := make([]types.Address, 0, len(addrs))
	for _, a := range addrs {
		if a != drop {
			out = append(out, a)
		}
	}
	return out
}

func writeList(addrs []types.Address, path string, alsoStdout bool) error {
	var b strings.Builder
	for _, a := range addrs {
		b.WriteString(a.String())
		b.WriteByte('\n')
	}
	if alsoStdout {
		fmt.Print(b.String())
	}
	if path == "" {
		return nil
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote   %d addresses to %s\n", len(addrs), path)
	return nil
}

func usage(set *flag.FlagSet) {
	fmt.Fprint(os.Stderr, `zts-airdrop — send a ZTS token to every user address that transacted in a range of momentums

usage: zts-airdrop -last N | -from H  -zts <token> -amount <per recipient> <key flags> [-confirm]

Without -confirm the run scans, prints the plan and the full recipient list,
and publishes nothing.

examples:
  # who transacted in the last day (~8640 momentums), no key needed
  zts-airdrop -last 8640 -list

  # dry run of 10 tokens each to everyone active since height 1000000
  zts-airdrop -from 1000000 -zts zts1... -amount 10 -keystore wallet.json

  # send it
  ZTS_AIRDROP_PASSWORD=... zts-airdrop -from 1000000 -zts zts1... -amount 10 \
      -keystore wallet.json -confirm

flags:
`)
	set.PrintDefaults()
}

// rangeNamedBy records how the operator asked for the range, so a log of a
// "-last 8640" run still shows that intent beside the absolute heights it
// resolved to.
func rangeNamedBy(o *options) string {
	switch {
	case o.from > 0 && o.to > 0:
		return fmt.Sprintf("-from %d -to %d", o.from, o.to)
	case o.from > 0:
		return fmt.Sprintf("-from %d", o.from)
	case o.last > 0 && o.to > 0:
		return fmt.Sprintf("-last %d -to %d", o.last, o.to)
	case o.last > 0:
		return fmt.Sprintf("-last %d", o.last)
	default:
		return ""
	}
}

// verifyScan re-runs a recorded scan and reports whether it reproduces.
//
// This is what makes the log's claim checkable rather than merely asserted.
// It re-derives the address set from the same absolute range and compares
// digests; a mismatch means either the node has different history or the scan
// rules changed between builds, and the version fields in the log are there
// to tell those two apart.
func verifyScan(ctx context.Context, o *options) error {
	logged, err := ReadScanLog(o.verifyScan)
	if err != nil {
		return err
	}

	node := logged.Node
	if o.node != defaultNode {
		node = o.node // an explicit -node re-verifies against a different node
	}
	fmt.Fprintf(os.Stderr, "verifying %s\n", o.verifyScan)
	fmt.Fprintf(os.Stderr, "scanned   %s by %s %s\n",
		logged.StartedAt.Format(time.RFC3339), logged.Tool, logged.ToolVersion)
	fmt.Fprintf(os.Stderr, "range     momentums %d..%d on %s\n",
		logged.Range.From, logged.Range.To, node)

	client := NewClient(node)
	scanner := &Scanner{
		Client:            client,
		IncludeRecipients: logged.Options.IncludeRecipients,
		Progress:          os.Stderr,
	}
	res, err := scanner.Scan(ctx, logged.Range.From, logged.Range.To)
	if err != nil {
		return err
	}

	got := AddressDigest(res.Addresses)
	fmt.Fprintf(os.Stderr, "recorded  %d addresses, digest %s\n", logged.Totals.UserAddresses, logged.Digest)
	fmt.Fprintf(os.Stderr, "now       %d addresses, digest %s\n", len(res.Addresses), got)

	if got == logged.Digest {
		fmt.Fprintln(os.Stderr, "\nREPRODUCED - the scan yields the same address set")
		return nil
	}

	// A mismatch is worth explaining rather than merely reporting: which side
	// gained or lost addresses says whether the node is behind, ahead, or
	// serving different history.
	was := make(map[string]bool, len(logged.Addresses))
	for _, a := range logged.Addresses {
		was[a] = true
	}
	added := 0
	for _, a := range addressStrings(res.Addresses) {
		if !was[a] {
			added++
		} else {
			delete(was, a)
		}
	}
	return fmt.Errorf("NOT REPRODUCED: %d addresses now present that were not logged, %d logged that are now absent",
		added, len(was))
}
