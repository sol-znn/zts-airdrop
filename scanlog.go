package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zenon-network/go-zenon/common/types"
)

// ScanLogVersion is the schema version of a scan log. It is the first field
// in the file so that a reader can decide whether it understands the rest
// before parsing it.
const ScanLogVersion = 1

// ScanLog is the record of one scan, written every time a scan runs.
//
// It exists because the recipient list is derived, not supplied. Somebody
// looking at an airdrop after the fact — including the person who ran it —
// needs to be able to answer "where did these addresses come from, and would
// I get the same list again". A log that only said how many were found could
// not answer either question.
//
// # What makes it reproducible
//
// The range is always recorded as resolved absolute heights. That is the
// whole trick: "-last 8640" names a different range every momentum, so it is
// not a reproducible input and is never what gets written. `Reproduce` is the
// exact command that re-derives this list, and it is spelled with -from/-to
// no matter how the original run named the range.
//
// A scan over a settled range is deterministic — the momentums are immutable
// and the filter is a pure function of them — so re-running the recorded
// command against a node with that history must produce the same addresses.
// Digest is what makes that check cheap: compare two 64-character strings
// rather than two lists of ten thousand addresses.
type ScanLog struct {
	Version int    `json:"version"`
	Tool    string `json:"tool"`
	// ToolVersion is "dev" for a local build. A log that cannot say which
	// binary produced it cannot be reproduced with confidence, because the
	// scan rules could have changed between builds.
	ToolVersion string `json:"toolVersion"`

	// StartedAt and FinishedAt bracket the scan in UTC. Both are recorded
	// rather than one plus a duration: the pair survives a log that is
	// truncated or merged, and the wall clock is what correlates a scan with
	// what else was happening on the node.
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	ElapsedMS  int64     `json:"elapsedMs"`

	Node  string `json:"node"`
	Chain uint64 `json:"chainIdentifier"`

	Range ScanRange `json:"range"`
	Tip   ScanTip   `json:"tipAtScan"`

	Options ScanOptions `json:"options"`
	Totals  ScanTotals  `json:"totals"`

	// Digest is sha256 over the sorted addresses, one per line, each
	// newline-terminated. Two scans agree if and only if their digests do.
	Digest    string   `json:"digest"`
	Addresses []string `json:"addresses"`

	// Filter is present only when something narrowed the scanned set into the
	// recipient set. Keeping the two apart matters: the scan is a fact about
	// the chain, the filter is a choice made by the operator, and conflating
	// them would make a log that cannot be checked against a re-scan.
	Filter *ScanFilter `json:"filter,omitempty"`

	// Airdrop is present only when the log belongs to a run that priced or
	// published an airdrop.
	Airdrop *ScanAirdrop `json:"airdrop,omitempty"`

	// Reproduce is the command that re-derives Addresses.
	Reproduce string `json:"reproduce"`
}

type ScanRange struct {
	From  uint64 `json:"from"`
	To    uint64 `json:"to"`
	Count uint64 `json:"count"`
	// NamedBy records how the operator asked for this range, so that a log of
	// a "-last 8640" run still shows the intent behind the absolute heights.
	NamedBy string `json:"namedBy"`
}

type ScanTip struct {
	Height uint64 `json:"height"`
	Hash   string `json:"hash"`
}

type ScanOptions struct {
	IncludeRecipients bool `json:"includeRecipients"`
}

type ScanTotals struct {
	Momentums      int `json:"momentums"`
	Blocks         int `json:"blocks"`
	ContractBlocks int `json:"contractBlocks"`
	UserAddresses  int `json:"userAddresses"`
}

type ScanFilter struct {
	ExcludedSelf     string   `json:"excludedSelf,omitempty"`
	MinHolding       string   `json:"minHolding,omitempty"`
	RecipientCount   int      `json:"recipientCount"`
	RecipientDigest  string   `json:"recipientDigest"`
	Recipients       []string `json:"recipients"`
	RemovedByFilters int      `json:"removedByFilters"`
}

type ScanAirdrop struct {
	ZTS      string `json:"zts"`
	Symbol   string `json:"symbol"`
	Decimals uint8  `json:"decimals"`
	// AmountRaw is the smallest-unit value actually put on the wire, and
	// Amount is how it was displayed. Both are recorded because the decimals
	// are the one place a bulk transfer can go wrong by a factor of a
	// hundred, and a log that showed only "10" could not prove which was sent.
	AmountRaw   string `json:"amountRaw"`
	Amount      string `json:"amount"`
	TotalRaw    string `json:"totalRaw"`
	Total       string `json:"total"`
	Sender      string `json:"sender"`
	RunKey      string `json:"runKey"`
	Journal     string `json:"journal"`
	Published   bool   `json:"published"`
	Sent        int    `json:"sent"`
	Skipped     int    `json:"skipped"`
	Failed      int    `json:"failed"`
	AlreadyPaid int    `json:"alreadyPaidAtStart"`
}

// AddressDigest hashes a list of addresses in sorted order.
//
// Sorting inside the digest rather than trusting the caller's order means the
// digest answers "is this the same set", which is the question, rather than
// "is this the same list in the same order", which is an artefact of how the
// scan happened to walk the chain.
func AddressDigest(addrs []types.Address) string {
	strs := make([]string, len(addrs))
	for i, a := range addrs {
		strs[i] = a.String()
	}
	return digestStrings(strs)
}

func digestStrings(strs []string) string {
	sorted := append([]string(nil), strs...)
	// A plain lexicographic sort over the bech32 form: locale-free, and
	// identical in any language a reimplementation might use.
	sort.Strings(sorted)
	h := sha256.New()
	for _, s := range sorted {
		h.Write([]byte(s))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func addressStrings(addrs []types.Address) []string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return out
}

// NewScanLog builds the log for a completed scan.
func NewScanLog(node string, chain uint64, tip ScanTip, namedBy string, opts ScanOptions, res *ScanResult, startedAt time.Time) *ScanLog {
	return &ScanLog{
		Version:     ScanLogVersion,
		Tool:        "zts-airdrop",
		ToolVersion: version,
		StartedAt:   startedAt.UTC(),
		FinishedAt:  startedAt.Add(res.Elapsed).UTC(),
		ElapsedMS:   res.Elapsed.Milliseconds(),
		Node:        node,
		Chain:       chain,
		Range: ScanRange{
			From: res.FromHeight, To: res.ToHeight,
			Count: res.ToHeight - res.FromHeight + 1, NamedBy: namedBy,
		},
		Tip:     tip,
		Options: opts,
		Totals: ScanTotals{
			Momentums: res.Momentums, Blocks: res.Blocks,
			ContractBlocks: res.Contracts, UserAddresses: len(res.Addresses),
		},
		Digest:    AddressDigest(res.Addresses),
		Addresses: addressStrings(res.Addresses),
		Reproduce: reproduceCommand(node, res.FromHeight, res.ToHeight, opts),
	}
}

// reproduceCommand spells the range with -from/-to even when the original run
// used -last, because -last is not reproducible.
func reproduceCommand(node string, from, to uint64, opts ScanOptions) string {
	parts := []string{
		"zts-airdrop",
		"-node " + node,
		fmt.Sprintf("-from %d", from),
		fmt.Sprintf("-to %d", to),
	}
	if opts.IncludeRecipients {
		parts = append(parts, "-include-recipients")
	}
	parts = append(parts, "-list")
	return strings.Join(parts, " ")
}

// SetFilter records the narrowing from scanned addresses to recipients.
func (l *ScanLog) SetFilter(self string, minHolding string, recipients []types.Address) {
	l.Filter = &ScanFilter{
		ExcludedSelf:     self,
		MinHolding:       minHolding,
		RecipientCount:   len(recipients),
		RecipientDigest:  AddressDigest(recipients),
		Recipients:       addressStrings(recipients),
		RemovedByFilters: len(l.Addresses) - len(recipients),
	}
}

// Filename is the log's name: a UTC timestamp first so that a directory of
// them sorts chronologically, then the range, so the file a reader wants is
// identifiable without opening any of them.
func (l *ScanLog) Filename() string {
	return fmt.Sprintf("scan-%s-%d-%d.json",
		l.StartedAt.Format("20060102T150405Z"), l.Range.From, l.Range.To)
}

// Write saves the log under dir, creating it if needed, and returns the path.
// An empty dir disables logging.
//
// The write is atomic — a temporary file renamed into place — so a run
// interrupted mid-write leaves either the previous log or none, never a
// half-written one that a later reader would mistake for a real record.
func (l *ScanLog) Write(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("scan log dir: %w", err)
	}
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return "", err
	}
	body = append(body, '\n')

	path := filepath.Join(dir, l.Filename())
	tmp, err := os.CreateTemp(dir, ".scan-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return path, nil
}

// ReadScanLog loads a saved log.
func ReadScanLog(path string) (*ScanLog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l ScanLog
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.Version != ScanLogVersion {
		return nil, fmt.Errorf("%s: scan log version %d, this build understands %d",
			path, l.Version, ScanLogVersion)
	}
	return &l, nil
}
