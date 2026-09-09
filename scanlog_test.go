package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zenon-network/go-zenon/common/types"
)

func addrs(t *testing.T, ss ...string) []types.Address {
	t.Helper()
	out := make([]types.Address, len(ss))
	for i, s := range ss {
		a, err := types.ParseAddress(s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		out[i] = a
	}
	return out
}

const (
	addrA = "z1qpffqavsz3dvm5wygzrpnu8e2v9zcnxy3zz2wp"
	addrB = "z1qphppnntrhdapxeqzcxj3pnuvcz26t0r2mf400"
	addrC = "z1qq9n7fpaqd8lpcljandzmx4xtku9w4ftwyg0mq"
)

// The digest is what makes a scan log checkable, so the property that matters
// is that it identifies the *set*: order must not change it, and membership
// must.
func TestAddressDigestIgnoresOrder(t *testing.T) {
	one := AddressDigest(addrs(t, addrA, addrB, addrC))
	two := AddressDigest(addrs(t, addrC, addrA, addrB))
	if one != two {
		t.Errorf("digest depends on order:\n %s\n %s", one, two)
	}
}

func TestAddressDigestDetectsMembership(t *testing.T) {
	full := AddressDigest(addrs(t, addrA, addrB, addrC))
	short := AddressDigest(addrs(t, addrA, addrB))
	if full == short {
		t.Error("dropping an address did not change the digest")
	}
	if empty := AddressDigest(nil); empty == full {
		t.Error("the empty set digests the same as a populated one")
	}
}

// A log must survive the round trip through disk unchanged, because the whole
// point is that somebody reads it back later — possibly with a different build
// of this tool.
func TestScanLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	res := &ScanResult{
		FromHeight: 26600, ToHeight: 27300,
		Momentums: 701, Blocks: 214, Contracts: 76,
		Addresses: addrs(t, addrA, addrB, addrC),
		Elapsed:   38 * time.Millisecond,
	}
	log := NewScanLog("http://node:35997", 69,
		ScanTip{Height: 46410, Hash: "abc"}, "-last 700",
		ScanOptions{}, res, time.Now())
	log.SetFilter(addrC, "", addrs(t, addrA, addrB))

	path, err := log.Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadScanLog(path)
	if err != nil {
		t.Fatal(err)
	}

	if back.Digest != log.Digest {
		t.Errorf("digest changed on round trip: %s vs %s", back.Digest, log.Digest)
	}
	if len(back.Addresses) != 3 {
		t.Errorf("got %d addresses, want 3", len(back.Addresses))
	}
	if back.Range.From != 26600 || back.Range.To != 27300 {
		t.Errorf("range changed: %+v", back.Range)
	}
	// The range must be reproducible by absolute height even though it was
	// named with -last, which resolves differently every momentum.
	if back.Range.NamedBy != "-last 700" {
		t.Errorf("namedBy = %q", back.Range.NamedBy)
	}
	want := "zts-airdrop -node http://node:35997 -from 26600 -to 27300 -list"
	if back.Reproduce != want {
		t.Errorf("reproduce =\n %q\nwant\n %q", back.Reproduce, want)
	}
	if back.Filter == nil || back.Filter.RecipientCount != 2 || back.Filter.RemovedByFilters != 1 {
		t.Errorf("filter = %+v", back.Filter)
	}
}

// The filename carries the timestamp and the range so a directory of logs is
// navigable without opening any of them.
func TestScanLogFilename(t *testing.T) {
	log := &ScanLog{
		StartedAt: time.Date(2026, 9, 9, 2, 8, 17, 0, time.UTC),
		Range:     ScanRange{From: 26600, To: 27300},
	}
	if got, want := log.Filename(), "scan-20260909T020817Z-26600-27300.json"; got != want {
		t.Errorf("Filename() = %q, want %q", got, want)
	}
}

// An empty directory disables logging rather than writing somewhere arbitrary.
func TestScanLogDisabled(t *testing.T) {
	log := &ScanLog{StartedAt: time.Now(), Range: ScanRange{From: 1, To: 2}}
	path, err := log.Write("")
	if err != nil || path != "" {
		t.Errorf("Write(\"\") = %q, %v; want no file and no error", path, err)
	}
}

// A log from a future schema must be refused rather than half-understood: the
// scan rules it was written under may not be the ones this build applies.
func TestScanLogVersionRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadScanLog(path); err == nil {
		t.Error("a version 999 log was accepted")
	}
}
