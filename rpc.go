// Package main — rpc.go is a small JSON-RPC client for a NoM node.
//
// It mirrors the node's JSON shapes rather than importing go-zenon's rpc/api
// package, which would drag the whole chain implementation in for the sake of
// a few struct tags. Only the calls this tool actually makes are here.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zenon-network/go-zenon/chain/nom"
	"github.com/zenon-network/go-zenon/common/types"
)

// maxCountSize mirrors rpc/api.RpcMaxCountSize — the node rejects a larger
// count outright rather than clamping it.
const maxCountSize = 1024

type Client struct {
	url  string
	http *http.Client
	id   atomic.Uint64
}

func NewClient(url string) *Client {
	return &Client{url: url, http: &http.Client{Timeout: 60 * time.Second}}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: http %d: %s", method, resp.StatusCode, truncate(raw, 200))
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("%s: bad json: %w", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %w", method, r.Error)
	}
	if out == nil {
		return nil
	}
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return fmt.Errorf("%s: null result", method)
	}
	return json.Unmarshal(r.Result, out)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// --- ledger types ---

// AccountHeader is one entry of momentum.content: the author, hash and height
// of a block the momentum confirmed. The author address is all the default
// scan needs, which is why the light momentum call is enough for it.
type AccountHeader struct {
	Address types.Address `json:"address"`
	Hash    types.Hash    `json:"hash"`
	Height  uint64        `json:"height"`
}

type Momentum struct {
	Version         uint64          `json:"version"`
	ChainIdentifier uint64          `json:"chainIdentifier"`
	Hash            types.Hash      `json:"hash"`
	PreviousHash    types.Hash      `json:"previousHash"`
	Height          uint64          `json:"height"`
	Timestamp       int64           `json:"timestamp"`
	Content         []AccountHeader `json:"content"`
	Producer        types.Address   `json:"producer"`
}

type ConfirmationDetail struct {
	NumConfirmations uint64     `json:"numConfirmations"`
	MomentumHeight   uint64     `json:"momentumHeight"`
	MomentumHash     types.Hash `json:"momentumHash"`
}

type AccountBlock struct {
	BlockType     uint64                   `json:"blockType"`
	Hash          types.Hash               `json:"hash"`
	PreviousHash  types.Hash               `json:"previousHash"`
	Height        uint64                   `json:"height"`
	Address       types.Address            `json:"address"`
	ToAddress     types.Address            `json:"toAddress"`
	Amount        BigInt                   `json:"amount"`
	TokenStandard types.ZenonTokenStandard `json:"tokenStandard"`
	FromBlockHash types.Hash               `json:"fromBlockHash"`

	ConfirmationDetail *ConfirmationDetail `json:"confirmationDetail"`
}

// DetailedMomentum pairs a momentum with the bodies of its blocks. Only the
// -include-recipients scan needs it: a block body is the only place the
// counterparty of a send is recorded.
type DetailedMomentum struct {
	AccountBlocks []*AccountBlock `json:"blocks"`
	Momentum      *Momentum       `json:"momentum"`
}

// BigInt decodes a NoM amount, which the node renders as a JSON *string* so
// that values above 2^53 survive JavaScript clients. A plain *big.Int field
// cannot read that, and the failure is total — the whole response fails to
// parse — so the tolerance belongs in the type.
type BigInt struct{ *big.Int }

func (b *BigInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), "\"")
	if s == "" || s == "null" {
		b.Int = big.NewInt(0)
		return nil
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return fmt.Errorf("amount %q is not a decimal integer", s)
	}
	b.Int = v
	return nil
}

// --- ledger calls ---

func (c *Client) FrontierMomentum(ctx context.Context) (*Momentum, error) {
	var m Momentum
	return &m, c.call(ctx, "ledger.getFrontierMomentum", nil, &m)
}

// MomentumsByHeight fetches count momentums starting at height, headers only.
// This is the fast path for scanning: the author of every block is already in
// momentum.content, so bodies are dead weight unless recipients are wanted.
func (c *Client) MomentumsByHeight(ctx context.Context, height, count uint64) ([]*Momentum, error) {
	if count > maxCountSize {
		count = maxCountSize
	}
	var list struct {
		List  []*Momentum `json:"list"`
		Count int         `json:"count"`
	}
	if err := c.call(ctx, "ledger.getMomentumsByHeight", []any{height, count}, &list); err != nil {
		return nil, err
	}
	return list.List, nil
}

// DetailedMomentumsByHeight fetches count momentums with their block bodies.
func (c *Client) DetailedMomentumsByHeight(ctx context.Context, height, count uint64) ([]*DetailedMomentum, error) {
	if count > maxCountSize {
		count = maxCountSize
	}
	var list struct {
		List  []*DetailedMomentum `json:"list"`
		Count int                 `json:"count"`
	}
	if err := c.call(ctx, "ledger.getDetailedMomentumsByHeight", []any{height, count}, &list); err != nil {
		return nil, err
	}
	return list.List, nil
}

func (c *Client) AccountBlockByHash(ctx context.Context, hash types.Hash) (*AccountBlock, error) {
	var b AccountBlock
	if err := c.call(ctx, "ledger.getAccountBlockByHash", []any{hash.String()}, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// FrontierAccountBlock returns the account's newest block, or nil if it has
// never published one. Absence is an expected state, not a failure.
func (c *Client) FrontierAccountBlock(ctx context.Context, addr types.Address) (*AccountBlock, error) {
	var b AccountBlock
	if err := c.call(ctx, "ledger.getFrontierAccountBlock", []any{addr.String()}, &b); err != nil {
		return nil, nil //nolint:nilerr // an account with no blocks has no frontier
	}
	return &b, nil
}

func (c *Client) Balance(ctx context.Context, addr types.Address, zts types.ZenonTokenStandard) (*big.Int, error) {
	var info struct {
		BalanceInfoMap map[string]struct {
			Balance BigInt `json:"balance"`
		} `json:"balanceInfoMap"`
	}
	if err := c.call(ctx, "ledger.getAccountInfoByAddress", []any{addr.String()}, &info); err != nil {
		return nil, err
	}
	if e, ok := info.BalanceInfoMap[zts.String()]; ok && e.Balance.Int != nil {
		return e.Balance.Int, nil
	}
	return big.NewInt(0), nil
}

func (c *Client) PublishRawTransaction(ctx context.Context, block *nom.AccountBlock) error {
	return c.call(ctx, "ledger.publishRawTransaction", []any{block}, nil)
}

// UnreceivedBlocks lists sends addressed to an account that it has not taken
// delivery of yet.
//
// On NoM a send is final for the sender the moment it confirms, but the value
// only enters the recipient's balance once the recipient publishes a matching
// receive block. Until then it sits here. That is why a freshly issued token
// shows a full supply on the token contract and a zero balance on the issuer:
// nothing is wrong, the mint simply has not been collected.
func (c *Client) UnreceivedBlocks(ctx context.Context, addr types.Address, page, size uint32) ([]*AccountBlock, error) {
	var list struct {
		List  []*AccountBlock `json:"list"`
		Count int             `json:"count"`
	}
	if err := c.call(ctx, "ledger.getUnreceivedBlocksByAddress",
		[]any{addr.String(), page, size}, &list); err != nil {
		return nil, err
	}
	return list.List, nil
}

// Token is what the node knows about one token standard. Decimals is the
// field that matters most here: an amount on the wire is in the smallest
// unit, so airdropping "1" without it moves 1e-8 of a token.
type Token struct {
	Name          string                   `json:"name"`
	Symbol        string                   `json:"symbol"`
	Decimals      uint8                    `json:"decimals"`
	TokenStandard types.ZenonTokenStandard `json:"tokenStandard"`
}

func (c *Client) TokenByZTS(ctx context.Context, zts types.ZenonTokenStandard) (*Token, error) {
	var t Token
	if err := c.call(ctx, "embedded.token.getByZts", []any{zts.String()}, &t); err != nil {
		return nil, err
	}
	if t.Symbol == "" {
		return nil, fmt.Errorf("no token registered for %s", zts)
	}
	return &t, nil
}

// RequiredPlasma is what the node says a block will cost, including the PoW
// difficulty to target when the account has no fused plasma. Asking rather
// than recomputing the formula locally is deliberate: under dynamic plasma
// the price moves per momentum, so a local copy can be wrong at exactly the
// wrong moment.
type RequiredPlasma struct {
	AvailablePlasma    uint64 `json:"availablePlasma"`
	BasePlasma         uint64 `json:"basePlasma"`
	RequiredDifficulty uint64 `json:"requiredDifficulty"`
}

func (c *Client) RequiredPoW(ctx context.Context, from, to types.Address, blockType uint64, data []byte) (*RequiredPlasma, error) {
	var r RequiredPlasma
	param := map[string]any{
		"address":   from.String(),
		"blockType": blockType,
		"toAddress": to.String(),
		"data":      data, // marshals to base64, which is what the node expects
	}
	return &r, c.call(ctx, "embedded.plasma.getRequiredPoWForAccountBlock", []any{param}, &r)
}
