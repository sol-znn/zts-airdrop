package main

import (
	"strings"
	"testing"
)

// A replay pays real money to a list nobody re-derives, so the log's own
// integrity check is the only thing standing between an edited file and an
// airdrop to addresses that were never approved.

func TestReplayListPrefersFilteredRecipients(t *testing.T) {
	scanned := addrs(t, addrA, addrB, addrC)
	kept := addrs(t, addrA, addrB)
	log := &ScanLog{
		Addresses: addressStrings(scanned),
		Digest:    AddressDigest(scanned),
		Filter: &ScanFilter{
			Recipients:      addressStrings(kept),
			RecipientDigest: AddressDigest(kept),
			RecipientCount:  len(kept),
		},
	}
	got, label, _, err := replayList(log)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("replayed %d addresses, want the 2 filtered recipients", len(got))
	}
	if label != "filtered recipients" {
		t.Errorf("label = %q", label)
	}
}

// With no filter recorded, the raw scan is what there is to replay.
func TestReplayListFallsBackToScanned(t *testing.T) {
	scanned := addrs(t, addrA, addrB, addrC)
	log := &ScanLog{Addresses: addressStrings(scanned), Digest: AddressDigest(scanned)}
	got, label, _, err := replayList(log)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || label != "scanned addresses" {
		t.Errorf("got %d addresses labelled %q", len(got), label)
	}
}

// The check that matters: a list that does not hash to its recorded digest is
// refused rather than paid.
func TestReplayListRejectsTamperedList(t *testing.T) {
	scanned := addrs(t, addrA, addrB)
	log := &ScanLog{
		Addresses: addressStrings(scanned),
		Digest:    AddressDigest(scanned),
	}
	// Somebody swaps an address in without recomputing the digest.
	log.Addresses[1] = addrC

	if _, _, _, err := replayList(log); err == nil {
		t.Fatal("a tampered address list was accepted")
	} else if !strings.Contains(err.Error(), "inconsistent") {
		t.Errorf("error does not explain the mismatch: %v", err)
	}
}

// The same check has to apply to the filtered set, which is the list actually
// replayed when one is present.
func TestReplayListRejectsTamperedFilter(t *testing.T) {
	scanned := addrs(t, addrA, addrB, addrC)
	kept := addrs(t, addrA, addrB)
	log := &ScanLog{
		Addresses: addressStrings(scanned),
		Digest:    AddressDigest(scanned),
		Filter: &ScanFilter{
			Recipients:      addressStrings(kept),
			RecipientDigest: AddressDigest(kept),
		},
	}
	log.Filter.Recipients = append(log.Filter.Recipients, addrC)

	if _, _, _, err := replayList(log); err == nil {
		t.Fatal("an extra recipient was accepted into a replay")
	}
}

func TestReplayListRejectsEmptyAndUnparseable(t *testing.T) {
	if _, _, _, err := replayList(&ScanLog{}); err == nil {
		t.Error("an empty log was accepted")
	}
	bad := &ScanLog{Addresses: []string{"not-an-address"}, Digest: "x"}
	if _, _, _, err := replayList(bad); err == nil {
		t.Error("a malformed address was accepted")
	}
}
