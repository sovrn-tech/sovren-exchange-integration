package deposits

// Shared cross-language parse fixtures: the Go suite generates
// typescript/src/deposits/fixtures/test-vectors/parse-cases.json (regenerate with
// SOVREN_WRITE_FIXTURES=1 go test ./deposits -run TestParseFixtures) and
// both the Go and TS parsers must reproduce the expected outputs (SC-006).

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

const fixturePath = "../../typescript/src/deposits/fixtures/test-vectors/parse-cases.json"

type fxAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type fxEvent struct {
	Type       string   `json:"type"`
	Attributes []fxAttr `json:"attributes"`
}

type fxTxResult struct {
	Code   uint32    `json:"code"`
	Log    string    `json:"log,omitempty"`
	Events []fxEvent `json:"events,omitempty"`
}

type fxBlock struct {
	Height           int64    `json:"height"`
	HashHex          string   `json:"hash_hex"`
	LastBlockHashHex string   `json:"last_block_hash_hex"`
	Time             string   `json:"time"`
	TxsBase64        []string `json:"txs_base64"`
}

type fxWatch struct {
	Address      string `json:"address"`
	Kind         string `json:"kind"`
	MemoRequired bool   `json:"memo_required,omitempty"`
}

type fxTransfer struct {
	TxHash          string   `json:"tx_hash"`
	MessageIndex    uint32   `json:"message_index"`
	CoinIndex       uint32   `json:"coin_index"`
	OpIndex         uint32   `json:"op_index"`
	Direction       string   `json:"direction"`
	Address         string   `json:"address"`
	CounterpartySet []string `json:"counterparty_set"`
	SenderAddress   *string  `json:"sender_address"`
	Denom           string   `json:"denom"`
	AmountBaseUnits string   `json:"amount_base_units"`
	Memo            string   `json:"memo"`
	TxCode          uint32   `json:"tx_code"`
	Classification  string   `json:"classification"`
	ReviewReason    string   `json:"review_reason"`
}

type fxFeeDeduction struct {
	TxHash         string `json:"tx_hash"`
	PayerAddress   string `json:"payer_address"`
	FeeBaseUnits   string `json:"fee_base_units"`
	TxCode         uint32 `json:"tx_code"`
	FeeGranterUsed bool   `json:"fee_granter_used"`
}

type fxReviewCandidate struct {
	TxHash          string   `json:"tx_hash"`
	MessageIndex    uint32   `json:"message_index"`
	EventIndex      uint32   `json:"event_index"`
	OpIndex         uint32   `json:"op_index"`
	Direction       string   `json:"direction"`
	Address         string   `json:"address"`
	CounterpartySet []string `json:"counterparty_set"`
	AmountBaseUnits string   `json:"amount_base_units"`
}

type fxBlockEvent struct {
	EventIndex      uint32   `json:"event_index"`
	Direction       string   `json:"direction"`
	Address         string   `json:"address"`
	CounterpartySet []string `json:"counterparty_set"`
	AmountBaseUnits string   `json:"amount_base_units"`
}

type fxExpected struct {
	Transfers        []fxTransfer        `json:"transfers"`
	FeeDeductions    []fxFeeDeduction    `json:"fee_deductions"`
	ReviewCandidates []fxReviewCandidate `json:"review_candidates"`
	BlockEvents      []fxBlockEvent      `json:"block_events"`
}

type fxCase struct {
	ID                  string       `json:"id"`
	Description         string       `json:"description"`
	Block               fxBlock      `json:"block"`
	TxResults           []fxTxResult `json:"tx_results"`
	FinalizeBlockEvents []fxEvent    `json:"finalize_block_events,omitempty"`
	Expected            fxExpected   `json:"expected"`
}

type fxFile struct {
	Description string    `json:"description"`
	ChainID     string    `json:"chain_id"`
	Watch       []fxWatch `json:"watch"`
	Cases       []fxCase  `json:"cases"`
}

func fxEventsToClient(evs []fxEvent) []client.Event {
	out := make([]client.Event, 0, len(evs))
	for _, e := range evs {
		attrs := make([]client.EventAttribute, 0, len(e.Attributes))
		for _, a := range e.Attributes {
			attrs = append(attrs, client.EventAttribute{Key: a.Key, Value: a.Value})
		}
		out = append(out, client.Event{Type: e.Type, Attributes: attrs})
	}
	return out
}

func fxToClientBlock(t *testing.T, c fxCase) (*client.Block, *client.BlockResults) {
	t.Helper()
	hash, err := hex.DecodeString(strings.ToLower(c.Block.HashHex))
	require.NoError(t, err)
	last, err := hex.DecodeString(strings.ToLower(c.Block.LastBlockHashHex))
	require.NoError(t, err)
	ts, err := time.Parse(time.RFC3339, c.Block.Time)
	require.NoError(t, err)
	txs := make([][]byte, 0, len(c.Block.TxsBase64))
	for _, b := range c.Block.TxsBase64 {
		raw, err := base64.StdEncoding.DecodeString(b)
		require.NoError(t, err)
		txs = append(txs, raw)
	}
	results := make([]client.TxExecResult, 0, len(c.TxResults))
	for _, r := range c.TxResults {
		results = append(results, client.TxExecResult{Code: r.Code, Log: r.Log, Events: fxEventsToClient(r.Events)})
	}
	return &client.Block{
			ChainID:       testChainID,
			Height:        c.Block.Height,
			Hash:          hash,
			LastBlockHash: last,
			Time:          ts,
			Txs:           txs,
		}, &client.BlockResults{
			Height:              c.Block.Height,
			TxResults:           results,
			FinalizeBlockEvents: fxEventsToClient(c.FinalizeBlockEvents),
		}
}

func expectedFromParse(bp *BlockParse) fxExpected {
	out := fxExpected{
		Transfers:        []fxTransfer{},
		FeeDeductions:    []fxFeeDeduction{},
		ReviewCandidates: []fxReviewCandidate{},
		BlockEvents:      []fxBlockEvent{},
	}
	for _, c := range bp.Transfers {
		out.Transfers = append(out.Transfers, fxTransfer{
			TxHash:          c.TxHash,
			MessageIndex:    c.MessageIndex,
			CoinIndex:       c.CoinIndex,
			OpIndex:         c.OpIndex,
			Direction:       string(c.Direction),
			Address:         c.Address,
			CounterpartySet: append([]string{}, c.CounterpartySet...),
			SenderAddress:   c.SenderAddress,
			Denom:           c.Denom,
			AmountBaseUnits: c.AmountBaseUnits.String(),
			Memo:            c.Memo,
			TxCode:          c.TxCode,
			Classification:  string(c.Classification),
			ReviewReason:    c.ReviewReason,
		})
	}
	for _, f := range bp.FeeDeductions {
		out.FeeDeductions = append(out.FeeDeductions, fxFeeDeduction{
			TxHash:         f.TxHash,
			PayerAddress:   f.PayerAddress,
			FeeBaseUnits:   f.FeeBaseUnits.String(),
			TxCode:         f.TxCode,
			FeeGranterUsed: f.FeeGranterUsed,
		})
	}
	for _, rc := range bp.ReviewCandidates {
		out.ReviewCandidates = append(out.ReviewCandidates, fxReviewCandidate{
			TxHash:          rc.TxHash,
			MessageIndex:    rc.MessageIndex,
			EventIndex:      rc.EventIndex,
			OpIndex:         rc.OpIndex,
			Direction:       string(rc.Direction),
			Address:         rc.Address,
			CounterpartySet: append([]string{}, rc.CounterpartySet...),
			AmountBaseUnits: rc.AmountBaseUnits.String(),
		})
	}
	for _, be := range bp.BlockEvents {
		out.BlockEvents = append(out.BlockEvents, fxBlockEvent{
			EventIndex:      be.EventIndex,
			Direction:       string(be.Direction),
			Address:         be.Address,
			CounterpartySet: append([]string{}, be.CounterpartySet...),
			AmountBaseUnits: be.AmountBaseUnits.String(),
		})
	}
	return out
}

// buildFixtureCases constructs the case inputs; expected sections are filled
// from the Go parser at write time and pinned for both languages afterwards.
func buildFixtureCases(t *testing.T, accts []testAccount) []fxCase {
	t.Helper()
	mk := func(id, desc string, height int64, txs [][]byte, results []fxTxResult, finalize []fxEvent) fxCase {
		b64 := make([]string, 0, len(txs))
		for _, raw := range txs {
			b64 = append(b64, base64.StdEncoding.EncodeToString(raw))
		}
		return fxCase{
			ID:          id,
			Description: desc,
			Block: fxBlock{
				Height:           height,
				HashHex:          strings.Repeat("AB", 16) + hex.EncodeToString([]byte{byte(height)}),
				LastBlockHashHex: strings.Repeat("CD", 16) + hex.EncodeToString([]byte{byte(height - 1)}),
				Time:             testBlockTime.Format(time.RFC3339),
				TxsBase64:        b64,
			},
			TxResults:           results,
			FinalizeBlockEvents: finalize,
		}
	}
	ok := func(events ...fxEvent) fxTxResult { return fxTxResult{Code: 0, Events: events} }
	feeEv := func(fee, payer string) fxEvent {
		return fxEvent{Type: eventTypeTx, Attributes: []fxAttr{{Key: attrFee, Value: fee}, {Key: attrFeePayer, Value: payer}}}
	}

	external, external2 := accts[0], accts[1]
	customer, omnibus, hot, feeW := accts[2], accts[3], accts[4], accts[5]

	signed := signedSendTx(t, external, customer.Bech32, "1000000", "hello", "500", 200000)
	multiMsg := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: external.Bech32, ToAddress: customer.Bech32, Amount: coins("250000usovr", "9foo")},
		&banktypes.MsgSend{FromAddress: external2.Bech32, ToAddress: hot.Bech32, Amount: coins("3bar", "777usovr")},
	}, "", nil)
	failed := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: external.Bech32, ToAddress: customer.Bech32, Amount: coins("5000usovr")},
	}, "", nil)
	nonUsovr := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: external.Bech32, ToAddress: customer.Bech32, Amount: coins("123foo")},
	}, "", nil)
	omnibusNoMemo := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: external.Bech32, ToAddress: omnibus.Bech32, Amount: coins("70000usovr")},
	}, "", nil)
	sweep := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: customer.Bech32, ToAddress: hot.Bech32, Amount: coins("90000usovr")},
	}, "", nil)
	feeFunding := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: feeW.Bech32, ToAddress: customer.Bech32, Amount: coins("50000usovr")},
	}, "", nil)
	withdrawal := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: hot.Bech32, ToAddress: external.Bech32, Amount: coins("400000usovr")},
	}, "", &txtypes.Fee{Amount: coins("999usovr"), GasLimit: 200000})
	mixedMulti := rawTx(t, []proto.Message{&banktypes.MsgMultiSend{
		Inputs: []banktypes.Input{
			{Address: external.Bech32, Coins: coins("60000usovr")},
			{Address: hot.Bech32, Coins: coins("40000usovr")},
		},
		Outputs: []banktypes.Output{{Address: customer.Bech32, Coins: coins("100000usovr")}},
	}}, "", nil)
	singleMulti := rawTx(t, []proto.Message{&banktypes.MsgMultiSend{
		Inputs: []banktypes.Input{{Address: external.Bech32, Coins: coins("30000usovr")}},
		Outputs: []banktypes.Output{
			{Address: external2.Bech32, Coins: coins("10000usovr")},
			{Address: customer.Bech32, Coins: coins("5foo", "20000usovr")},
		},
	}}, "", nil)
	unknownShape := func() []byte {
		a, err := anyWithTypeURL("/cosmos.authz.v1beta1.MsgExec", &banktypes.MsgSend{
			FromAddress: external.Bech32, ToAddress: customer.Bech32, Amount: coins("500000usovr"),
		})
		require.NoError(t, err)
		return rawTxFromAnys(t, a, "", nil)
	}()
	mixedKnownUnknown := func() []byte {
		send, err := codectypes.NewAnyWithValue(&banktypes.MsgSend{
			FromAddress: external.Bech32, ToAddress: customer.Bech32, Amount: coins("500000usovr"),
		})
		require.NoError(t, err)
		unknown, err := anyWithTypeURL("/cosmos.authz.v1beta1.MsgExec", &banktypes.MsgSend{
			FromAddress: external.Bech32, ToAddress: omnibus.Bech32, Amount: coins("700000usovr"),
		})
		require.NoError(t, err)
		return rawTxFromAnys(t, []*codectypes.Any{send, unknown[0]}, "", nil)
	}()
	granted := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: customer.Bech32, ToAddress: hot.Bech32, Amount: coins("100usovr")},
	}, "", &txtypes.Fee{Amount: coins("250usovr"), GasLimit: 100000, Granter: feeW.Bech32})
	dupEventReview := func() []byte {
		a, err := anyWithTypeURL("/cosmos.authz.v1beta1.MsgExec", &banktypes.MsgSend{
			FromAddress: external.Bech32, ToAddress: customer.Bech32, Amount: coins("500000usovr"),
		})
		require.NoError(t, err)
		return rawTxFromAnys(t, a, "", nil)
	}()

	return []fxCase{
		mk("single-send-external", "kit-built signed MsgSend from an external sender to a watched customer address; fee payer is external so no FEE_DEDUCTION",
			100, [][]byte{signed}, []fxTxResult{ok(feeEv("500usovr", external.Bech32))}, nil),
		mk("multi-msg-multi-coin", "two MsgSend in one tx, mixed denoms; usovr coins sit at wire index 1 (coins are denom-sorted)",
			101, [][]byte{multiMsg}, []fxTxResult{ok()}, nil),
		mk("failed-tx", "DeliverTx failure: transfer recorded with tx_code, fee still deducted post-ante, no review candidates",
			102, [][]byte{failed}, []fxTxResult{{Code: 5, Log: "out of gas", Events: []fxEvent{feeEv("400usovr", external.Bech32)}}}, nil),
		mk("non-usovr-only", "foreign-denom transfer to a watched address: never recorded",
			103, [][]byte{nonUsovr}, []fxTxResult{ok()}, nil),
		mk("omnibus-missing-memo", "memo_required omnibus recipient without a memo: EXTERNAL_DEPOSIT with a review reason (never auto-credited)",
			104, [][]byte{omnibusNoMemo}, []fxTxResult{ok()}, nil),
		mk("internal-sweep", "customer deposit address to hot wallet: SWEEP both sides, never a customer credit",
			105, [][]byte{sweep}, []fxTxResult{ok()}, nil),
		mk("fee-funding", "fee wallet to customer address: FEE_FUNDING, never a customer credit",
			106, [][]byte{feeFunding}, []fxTxResult{ok()}, nil),
		mk("hot-wallet-withdrawal", "hot wallet to external recipient: OUT WITHDRAWAL plus FEE_DEDUCTION (watched payer, event present)",
			107, [][]byte{withdrawal}, []fxTxResult{ok(feeEv("999usovr", hot.Bech32))}, nil),
		mk("multisend-mixed-inputs", "mixed watched/external MultiSend inputs: recipient row is UNATTRIBUTED_REVIEW with ambiguous sender",
			108, [][]byte{mixedMulti}, []fxTxResult{ok()}, nil),
		mk("multisend-single-input", "single-input MultiSend: deterministic coin_index across outputs in wire order",
			109, [][]byte{singleMulti}, []fxTxResult{ok()}, nil),
		mk("unsupported-shape-event-review", "unknown message type with a watched recipient in msg-indexed events: tx-scoped review candidate, no transfer",
			110, [][]byte{unknownShape}, []fxTxResult{ok(
				feeEv("100usovr", external.Bech32),
				fxEvent{Type: eventTypeCoinReceived, Attributes: []fxAttr{
					{Key: attrReceiver, Value: customer.Bech32},
					{Key: attrAmount, Value: "500000usovr"},
					{Key: attrMsgIndex, Value: "0"},
				}},
			)}, nil),
		mk("mixed-known-unknown-no-double-attribution", "supported MsgSend (msg 0, watched customer) + unsupported message (msg 1, watched omnibus): msg 0 is a canonical transfer, event review only covers the unattributed msg 1 — the attributed recipient is never double-counted (PR #300)",
			113, [][]byte{mixedKnownUnknown}, []fxTxResult{ok(
				fxEvent{Type: eventTypeCoinReceived, Attributes: []fxAttr{
					{Key: attrReceiver, Value: customer.Bech32},
					{Key: attrAmount, Value: "500000usovr"},
					{Key: attrMsgIndex, Value: "0"},
				}},
				fxEvent{Type: eventTypeCoinReceived, Attributes: []fxAttr{
					{Key: attrReceiver, Value: omnibus.Bech32},
					{Key: attrAmount, Value: "700000usovr"},
					{Key: attrMsgIndex, Value: "1"},
				}},
			)}, nil),
		mk("finalize-block-events", "block-scoped events touching watched addresses: block-level unattributed records only",
			111, nil, nil, []fxEvent{
				{Type: eventTypeCoinReceived, Attributes: []fxAttr{
					{Key: attrReceiver, Value: customer.Bech32},
					{Key: attrAmount, Value: "8888usovr"},
				}},
				{Type: eventTypeCoinSpent, Attributes: []fxAttr{
					{Key: attrSpender, Value: hot.Bech32},
					{Key: attrAmount, Value: "7777usovr"},
				}},
			}),
		mk("dup-event-family-single-review", "one movement emits BOTH coin_received and transfer for the same msg_index/amount: exactly one IN review candidate — the transfer view wins (carries the counterparty), the coin_received duplicate is dropped so the expected balance is not doubled (PR #300)",
			114, [][]byte{dupEventReview}, []fxTxResult{ok(
				fxEvent{Type: eventTypeCoinReceived, Attributes: []fxAttr{
					{Key: attrReceiver, Value: customer.Bech32},
					{Key: attrAmount, Value: "500000usovr"},
					{Key: attrMsgIndex, Value: "0"},
				}},
				fxEvent{Type: eventTypeTransfer, Attributes: []fxAttr{
					{Key: attrRecipient, Value: customer.Bech32},
					{Key: attrSender, Value: external.Bech32},
					{Key: attrAmount, Value: "500000usovr"},
					{Key: attrMsgIndex, Value: "0"},
				}},
			)}, nil),
		mk("three-event-family-single-review", "one external→customer movement emits ALL THREE bank events (coin_spent + coin_received + transfer) for the same msg_index/amount: coin_spent names the unwatched sender (no OUT candidate), the coin_received IN view is suppressed by the transfer, and exactly ONE IN review candidate is emitted from the transfer family (counterparty=sender) — the full event family collapses to a single review row so the expected balance is not tripled (PR #300)",
			117, [][]byte{dupEventReview}, []fxTxResult{ok(
				fxEvent{Type: eventTypeCoinSpent, Attributes: []fxAttr{
					{Key: attrSpender, Value: external.Bech32},
					{Key: attrAmount, Value: "500000usovr"},
					{Key: attrMsgIndex, Value: "0"},
				}},
				fxEvent{Type: eventTypeCoinReceived, Attributes: []fxAttr{
					{Key: attrReceiver, Value: customer.Bech32},
					{Key: attrAmount, Value: "500000usovr"},
					{Key: attrMsgIndex, Value: "0"},
				}},
				fxEvent{Type: eventTypeTransfer, Attributes: []fxAttr{
					{Key: attrRecipient, Value: customer.Bech32},
					{Key: attrSender, Value: external.Bech32},
					{Key: attrAmount, Value: "500000usovr"},
					{Key: attrMsgIndex, Value: "0"},
				}},
			)}, nil),
		mk("dup-finalize-family-single-block-event", "block-scoped movement emits BOTH coin_received and transfer for the same address/amount: exactly one IN block event — the transfer view wins, the coin_received duplicate is dropped (PR #300)",
			115, nil, nil, []fxEvent{
				{Type: eventTypeCoinReceived, Attributes: []fxAttr{
					{Key: attrReceiver, Value: customer.Bech32},
					{Key: attrAmount, Value: "6000usovr"},
				}},
				{Type: eventTypeTransfer, Attributes: []fxAttr{
					{Key: attrRecipient, Value: customer.Bech32},
					{Key: attrSender, Value: external.Bech32},
					{Key: attrAmount, Value: "6000usovr"},
				}},
			}),
		mk("feegrant-used", "use_feegrant present: the granter is the recorded fee payer",
			112, [][]byte{granted}, []fxTxResult{ok(
				fxEvent{Type: eventTypeUseFeeGrant, Attributes: []fxAttr{
					{Key: attrGranter, Value: feeW.Bech32},
					{Key: "grantee", Value: customer.Bech32},
				}},
				feeEv("250usovr", feeW.Bech32),
			)}, nil),
	}
}

func TestParseFixturesSharedWithTypeScript(t *testing.T) {
	ws, accts := testWatchSet(t)

	if os.Getenv("SOVREN_WRITE_FIXTURES") == "1" {
		cases := buildFixtureCases(t, accts)
		for i := range cases {
			block, results := fxToClientBlock(t, cases[i])
			bp, err := ParseBlockTransfers(block, results, ws)
			require.NoError(t, err)
			cases[i].Expected = expectedFromParse(bp)
		}
		watch := make([]fxWatch, 0)
		for _, w := range testWatchedAddresses(accts) {
			watch = append(watch, fxWatch{Address: w.Address, Kind: string(w.Kind), MemoRequired: w.MemoRequired})
		}
		file := fxFile{
			Description: "UNSAFE_TEST_ONLY fixture material (standard test mnemonic, no real funds). Shared Go/TS parseBlockTransfers conformance fixtures. Regenerate: SOVREN_WRITE_FIXTURES=1 go test ./deposits -run TestParseFixtures (exchange-kit/go).",
			ChainID:     testChainID,
			Watch:       watch,
			Cases:       cases,
		}
		data, err := json.MarshalIndent(file, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(fixturePath), 0o755))
		require.NoError(t, os.WriteFile(fixturePath, append(data, '\n'), 0o644))
	}

	data, err := os.ReadFile(fixturePath)
	require.NoError(t, err, "fixture file missing — regenerate with SOVREN_WRITE_FIXTURES=1")
	var file fxFile
	require.NoError(t, json.Unmarshal(data, &file))
	require.Equal(t, testChainID, file.ChainID)
	require.NotEmpty(t, file.Cases)

	for _, c := range file.Cases {
		t.Run(c.ID, func(t *testing.T) {
			block, results := fxToClientBlock(t, c)
			bp, err := ParseBlockTransfers(block, results, ws)
			require.NoError(t, err)
			require.Equal(t, c.Expected, expectedFromParse(bp))
		})
	}
}

func anyWithTypeURL(url string, msg proto.Message) ([]*codectypes.Any, error) {
	a, err := codectypes.NewAnyWithValue(msg)
	if err != nil {
		return nil, err
	}
	a.TypeUrl = url
	return []*codectypes.Any{a}, nil
}
