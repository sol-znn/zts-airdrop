# zts-airdrop

Sends a fixed amount of a ZTS token to every user address that transacted on
the Network of Momentum in a range of momentums.

```
zts-airdrop -last 8640 -zts zts1... -amount 10 -keystore wallet.json -confirm
```

## What it does

1. Resolves a momentum range — either from a height (`-from`) or a count back
   from the tip (`-last`).
2. Scans it and collects the distinct **user** addresses that acted in it.
3. Reads the token's decimals from the chain and prices the run.
4. Sends the amount to each address in turn, journalling every payment.

Nothing is published without `-confirm`. The default is a dry run that prints
the plan and the full recipient list.

## Who counts as a recipient

An address that **authored an account block** in the range. That is the
definition that survives contact with the ledger: a momentum's content lists
the author of every block it confirmed, and an author is by construction
somebody who held a key and signed with it.

- **Embedded contracts are excluded.** They author blocks constantly — every
  autoreceive and callback is one — and they cannot spend an ordinary transfer,
  so funding them burns that share. The test is a single byte of the address,
  not a hardcoded list, so a contract added after this was written is still
  excluded.
- **Recipients are opt-in** via `-include-recipients`. An address that only
  received took no action, and may not exist as an account at all. Including
  them forces the heavier block-body scan, because a counterparty is only
  recorded in a block body.
- **The sending address is excluded** by default (`-exclude-self`).
- `-min-holding X` further restricts the list to addresses already holding at
  least X of the token. It costs one RPC call per address.

## Decimals

The amount is written in whole token units and scaled by the decimals the node
reports for that token. `-amount 10` against an 8-decimal token sends
1000000000 base units; against a 0-decimal token it sends 10. Nothing assumes
ZNN's 8.

An amount too precise for the token is refused rather than rounded —
`-amount 1.5` against a 0-decimal token is an error, because rounding it would
silently change what every recipient gets. Trailing zeros that carry no value
(`10.0` against a 0-decimal token) are accepted.

## Resuming

Every payment is appended to a journal (`-journal`, default
`airdrop-journal.jsonl`) and flushed to disk before the next send is built.
Re-running with the same flags skips whoever is already recorded, so an
interrupted airdrop resumes instead of paying twice.

The journal is keyed by token, amount and momentum range together, so one file
can hold several airdrops without a later run inheriting an earlier one's
marks — and changing the amount correctly starts a new run rather than
resuming under the old one.

A send that fails is not journalled and is not retried in the same run; the
failures are listed at the end, and re-running picks up exactly those.

## Scan logs

Every run writes a scan log to `scans/` (`-scan-log-dir`, empty to disable),
named `scan-<UTC timestamp>-<from>-<to>.json`. It is written on the way out
even when the airdrop that followed the scan failed, since that is exactly the
run somebody will want to reconstruct.

A log records the timestamp, the tool version, the node and chain identifier,
the chain tip the range was resolved against, the scan options, the block and
contract-block counts, the full address list, and a `sha256` digest of it.
When the run went on to price or publish an airdrop, it also records the
token, its decimals, the amount both as displayed and as the raw
smallest-unit value put on the wire, the sender, the run key, and the outcome.

**The range is always recorded as resolved absolute heights.** That is what
makes a log reproducible: `-last 8640` names a different range every momentum,
so it is never what gets written. The log's `reproduce` field is the exact
command that re-derives the list, spelled with `-from`/`-to` regardless of how
the original run named the range. `namedBy` keeps the original intent beside
it.

To check a log actually reproduces:

```sh
zts-airdrop -verify-scan scans/scan-20260909T020817Z-26600-27300.json
```

It re-scans the same absolute range and compares digests, exiting non-zero on
a mismatch and reporting how many addresses appeared or vanished. A mismatch
means either the node is serving different history or the scan rules changed
between builds — which is why the tool version is in the log.

The digest covers the address *set*, not the order it was collected in, so it
answers "is this the same list" rather than "did the chain get walked the same
way".

## Replaying a scan

A saved scan log can be airdropped directly, with no scan at all:

```sh
zts-airdrop -replay scans/scan-20260909T020817Z-26600-27300.json          # dry run
zts-airdrop -replay scans/scan-20260909T020817Z-26600-27300.json -confirm # send
```

The recipient list, token, and amount all come from the log; only `-confirm`
turns it into a send. This exists because the list and the airdrop are two
decisions that need not happen at the same moment: a scan gets reviewed and
sat on, and by the time it is approved "the last N momentums" means a
different range. Replaying pays the list that was actually approved. It also
makes recovering an interrupted run cheap, since the expensive half — the scan
— is already on disk.

What comes from the log:

- **Recipients** — `filter.recipients` when the log has them (that is the
  approved, post-filter set), otherwise `addresses`.
- **Amount** — `airdrop.amountRaw`, the exact smallest-unit value, put back on
  the wire as recorded. It is deliberately not re-derived from the displayed
  form, so the decimal conversion never runs twice.
- **Range** — feeds the run key, so a replay **resumes the original journal**
  rather than starting a new run. Point `-journal` at the original file.

Before anything is sent, the list is hashed and checked against the digest
recorded beside it; a log edited by hand or truncated by a full disk is
refused, not paid.

`-zts` and `-amount` still override the log. That is a legitimate thing to
want — the same approved list, a different token or size — and since the run
key derives from token, amount and range together, an override correctly
starts a new run instead of colliding with the original.

A replay writes no scan log of its own: it scanned nothing, and a log claiming
otherwise would put a scan in the record that never happened.

## Plasma

Each send needs plasma. An account with QSR fused sends as fast as the node
accepts blocks; an account without pays proof of work per recipient, which on
a real network is seconds to a minute each. The run says which regime it is in
before the first send. **Fuse QSR to the sending address** before a large
airdrop.

## Sending sequentially

Every block from an account names its predecessor's hash, so two sends in
flight from the same address are two claims on the same height and one loses.
The sender waits for the node to report each published block as the account
frontier before building the next. Concurrency would have to come from several
funded addresses, which is a different tool.

## Balances that look wrong

On NoM a send is final for the sender the moment it confirms, but the value
enters the recipient's balance only once the recipient publishes a matching
receive block. A freshly issued or minted token therefore shows full supply on
the token contract and a zero balance on the issuer — nothing is wrong, the
mint has not been collected. `-receive` collects the sending address's pending
blocks before the airdrop starts.

## Configuration

Every flag has an environment name: `ZTS_AIRDROP_` plus the flag in upper case
with dashes as underscores, so `-min-holding` is `ZTS_AIRDROP_MIN_HOLDING`.
Copy `.env.example` to `.env`. Precedence is flags, then the real environment,
then `.env`.

A mnemonic is never accepted as a command-line argument: arguments land in
shell history and in the process table, where any local user can read them.
Use `ZTS_AIRDROP_MNEMONIC` in `.env`, `-mnemonic-file`, `-mnemonic-env`, or an
encrypted `-keystore` with `ZTS_AIRDROP_PASSWORD`.

## Flags

| Flag | Meaning |
| --- | --- |
| `-node` | Node JSON-RPC endpoint (default `http://localhost:35997`) |
| `-from` / `-to` | Momentum height range |
| `-last N` | The last N momentums instead of a start height |
| `-zts` | Token standard to airdrop |
| `-amount` | Amount per recipient, in whole token units |
| `-keystore` / `-password` | Encrypted go-zenon keystore |
| `-mnemonic-file` / `-mnemonic-env` | BIP-39 phrase from a file or an env var |
| `-index` | Account index under `m/44'/73404'/i'` |
| `-devnet` | Use the public devnet phrase (chain 69 only) |
| `-confirm` | Actually publish; without it the run is a dry run |
| `-dry-run` | Size the airdrop and publish nothing, even with `-confirm`; `-zts` and `-amount` optional |
| `-list` | Print the recipient list and exit without touching a key |
| `-out` | Also write the recipient list to a file |
| `-receive` | Collect the sending address's pending blocks first |
| `-journal` | Payment record and resume source |
| `-delay` | Pause between sends, for a node that rate-limits |
| `-stop-after-errors` | Abort after this many consecutive failures (default 10) |
| `-include-recipients` | Also count addresses that only received |
| `-exclude-self` | Leave the sending address out (default true) |
| `-min-holding` | Only addresses already holding this much |
| `-scan-log-dir` | Where scan logs go (default `scans`; empty disables) |
| `-replay` | Airdrop the list in a saved scan log; `-confirm` publishes it |
| `-verify-scan` | Re-run a recorded scan and check it reproduces |
| `-env` | Path to the settings file (default `.env`) |
| `-version` | Print the version |

## Examples

```sh
# Who transacted in the last day? No key needed.
zts-airdrop -last 8640 -list

# How many would an airdrop reach, and what would it cost?
zts-airdrop -last 8640 -zts zts1... -amount 10 -dry-run

# Dry run with the full recipient list and a balance check.
zts-airdrop -from 1000000 -zts zts1... -amount 10 -keystore wallet.json

# Send it.
ZTS_AIRDROP_PASSWORD=... zts-airdrop -from 1000000 -zts zts1... -amount 10 \
    -keystore wallet.json -confirm
```

## Building

The module depends on `go-zenon` through a `replace` pointing at a sibling
directory:

```sh
git clone https://github.com/zenon-network/go-zenon.git
git clone <this repo> zts-airdrop
cd zts-airdrop && go build .
```

## Tested

Exercised end to end against a devnet node (chain 69): a 21-recipient airdrop
of an 8-decimal token (ZNN) and of a 0-decimal token, both 21/21 with no
failures, verified on chain against the sender's and recipients' balances, and
a re-run confirmed to skip every already-paid address. Scan logs were checked
both ways: an untouched log reproduces (exit 0), and a log describing a
different address set is rejected with the difference reported (exit 1).
Replay was exercised the same way: a log replayed with `-confirm` published its
send, a second replay skipped it from the journal, a log with a tampered
recipient list was refused (exit 1), and a log recording sends the journal
cannot account for warns before proceeding.
