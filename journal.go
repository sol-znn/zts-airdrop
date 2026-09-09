package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"time"

	"github.com/zenon-network/go-zenon/common/types"
)

// Journal is the append-only record of what has already been paid.
//
// It exists because a bulk transfer is the one operation where a crash in the
// middle is worse than a crash at the start. Without a record, the only safe
// recovery from an interrupted run is to start over, and starting over pays
// everyone who was already paid a second time. The journal turns a resumed
// run into the remainder of the original one.
//
// Each line is written and flushed before the next send is built, never after
// a batch, so the worst case is a send that landed on chain without a line —
// one duplicate payment — rather than a line without a send, which would skip
// a real recipient silently. That asymmetry is the deliberate direction to
// fail in: it is visible on chain and recoverable, whereas a silent skip is
// neither.
type Journal struct {
	path string
	f    *os.File
	w    *bufio.Writer
	run  string
}

// Entry is one line of the journal.
type Entry struct {
	Run     string    `json:"run"`
	Index   int       `json:"index"`
	Address string    `json:"address"`
	Amount  string    `json:"amount"` // smallest unit, as a decimal string
	ZTS     string    `json:"zts"`
	Hash    string    `json:"hash"`
	Time    time.Time `json:"time"`
}

// RunKey identifies one airdrop by everything that would make paying twice
// wrong: the token, the per-recipient amount, and the block range the
// recipient list came from.
//
// Keying on this rather than on the file alone means one journal file can
// hold several airdrops without a later run inheriting an earlier run's
// "already paid" marks — and means changing the amount correctly starts a new
// run rather than resuming under the old one.
func RunKey(zts types.ZenonTokenStandard, amount *big.Int, from, to uint64) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%d", zts.String(), amount.String(), from, to)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// OpenJournal opens (creating if needed) the journal at path for one run.
func OpenJournal(path, run string) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	return &Journal{path: path, f: f, w: bufio.NewWriter(f), run: run}, nil
}

// Paid returns the addresses already recorded for this run, along with the
// hash each was paid in.
func Paid(path, run string) (map[types.Address]string, error) {
	paid := make(map[types.Address]string)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return paid, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if e.Run != run {
			continue
		}
		addr, err := types.ParseAddress(e.Address)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		paid[addr] = e.Hash
	}
	return paid, scanner.Err()
}

// Record appends one payment and flushes it to disk before returning.
func (j *Journal) Record(index int, addr types.Address, amount *big.Int, zts types.ZenonTokenStandard, hash types.Hash) error {
	line, err := json.Marshal(Entry{
		Run:     j.run,
		Index:   index,
		Address: addr.String(),
		Amount:  amount.String(),
		ZTS:     zts.String(),
		Hash:    hash.String(),
		Time:    time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if _, err := j.w.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := j.w.Flush(); err != nil {
		return err
	}
	// Flushing the buffer only reaches the OS. Sync is what survives the
	// machine losing power between two sends.
	return j.f.Sync()
}

func (j *Journal) Close() error {
	if err := j.w.Flush(); err != nil {
		j.f.Close()
		return err
	}
	return j.f.Close()
}
