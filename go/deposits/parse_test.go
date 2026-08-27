package deposits

import (
	"testing"

	sdkmath "cosmossdk.io/math"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func coins(pairs ...string) sdk.Coins {
	c, err := sdk.ParseCoinsNormalized(joinComma(pairs))
	if err != nil {
		panic(err)
	}
	return c
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func mustParse(t *testing.T, block *client.Block, results *client.BlockResults, ws WatchSet) *BlockParse {
	t.Helper()
	bp, err := ParseBlockTransfers(block, results, ws)
	require.NoError(t, err)
	return bp
}

func TestParseSingleExternalSendRealTxBytes(t *testing.T) {
	ws, accts := testWatchSet(t)
	txBytes := signedSendTx(t, accts[0], accts[2].Bech32, "1000000", "hello", "500", 200000)

	bp := mustParse(t, makeBlock(100, 0xAA, 0xA9, txBytes), makeResults(100, okResult()), ws)
	require.Len(t, bp.Transfers, 1)
	c := bp.Transfers[0]
	require.Equal(t, TxHashHex(txBytes), c.TxHash)
	require.Equal(t, storage.DirectionIn, c.Direction)
	require.Equal(t, storage.ClassExternalDeposit, c.Classification)
	require.Equal(t, accts[2].Bech32, c.Address)
	require.Equal(t, []string{accts[0].Bech32}, c.CounterpartySet)
	require.NotNil(t, c.SenderAddress)
	require.Equal(t, accts[0].Bech32, *c.SenderAddress)
	require.Equal(t, "1000000", c.AmountBaseUnits.String())
	require.Equal(t, "hello", c.Memo)
	require.Equal(t, uint32(0), c.MessageIndex)
	require.Equal(t, uint32(0), c.CoinIndex)
	require.Empty(t, c.ReviewReason)
	// External payer ⇒ no FEE_DEDUCTION even with a fee event present.
	require.Empty(t, bp.FeeDeductions)
	require.Empty(t, bp.ReviewCandidates)
	require.Empty(t, bp.BlockEvents)
}

func TestParseMultiMsgMultiCoinAttribution(t *testing.T) {
	ws, accts := testWatchSet(t)
	// msg0: external→customer (usovr at coin 0, foreign at coin 1);
	// msg1: external→hot wallet (foreign at coin 0, usovr at coin 1).
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("250000usovr", "9foo")},
		&banktypes.MsgSend{FromAddress: accts[1].Bech32, ToAddress: accts[4].Bech32, Amount: coins("3bar", "777usovr")},
	}, "", nil)

	bp := mustParse(t, makeBlock(101, 0xAB, 0xAA, txBytes), makeResults(101, okResult()), ws)
	require.Len(t, bp.Transfers, 2)

	// sdk.Coins are denom-sorted on the wire: foo precedes usovr, so the
	// usovr coin sits at wire index 1 in both messages.
	first := bp.Transfers[0]
	require.Equal(t, uint32(0), first.MessageIndex)
	require.Equal(t, uint32(1), first.CoinIndex)
	require.Equal(t, uint32(1), first.OpIndex)
	require.Equal(t, "250000", first.AmountBaseUnits.String())
	require.Equal(t, accts[2].Bech32, first.Address)

	// Non-usovr coins consume wire-order indexes but never create records.
	second := bp.Transfers[1]
	require.Equal(t, uint32(1), second.MessageIndex)
	require.Equal(t, uint32(1), second.CoinIndex)
	require.Equal(t, uint32(1), second.OpIndex)
	require.Equal(t, "777", second.AmountBaseUnits.String())
	require.Equal(t, accts[4].Bech32, second.Address)
	require.Equal(t, storage.ClassExternalDeposit, second.Classification)
}

func TestParseFailedTxCarriesCode(t *testing.T) {
	ws, accts := testWatchSet(t)
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("5000usovr")},
	}, "", nil)

	res := client.TxExecResult{Code: 5, Log: "out of gas", Events: []client.Event{feeEvent("400usovr", accts[0].Bech32)}}
	bp := mustParse(t, makeBlock(102, 0xAC, 0xAB, txBytes), makeResults(102, res), ws)
	require.Len(t, bp.Transfers, 1)
	require.Equal(t, uint32(5), bp.Transfers[0].TxCode)
	require.Equal(t, "out of gas", bp.Transfers[0].TxLog)
	// Failed txs never yield event-review candidates (nothing moved).
	require.Empty(t, bp.ReviewCandidates)
}

func TestParseNonUsovrOnlyIgnored(t *testing.T) {
	ws, accts := testWatchSet(t)
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("123foo")},
	}, "", nil)
	bp := mustParse(t, makeBlock(103, 0xAD, 0xAC, txBytes), makeResults(103, okResult()), ws)
	require.Empty(t, bp.Transfers)
	require.Empty(t, bp.ReviewCandidates)
}

func TestParseOmnibusMissingMemoRoutesToReview(t *testing.T) {
	ws, accts := testWatchSet(t)
	for _, tc := range []struct {
		name, memo string
		recognizer func(string) bool
		wantReview bool
	}{
		{name: "missing memo", memo: "", wantReview: true},
		{name: "memo present default recognizer", memo: "cust-42", wantReview: false},
		{name: "unrecognized memo", memo: "garbage", recognizer: func(m string) bool { return m == "cust-42" }, wantReview: true},
		{name: "recognized memo", memo: "cust-42", recognizer: func(m string) bool { return m == "cust-42" }, wantReview: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ws.WithMemoRecognizer(tc.recognizer)
			txBytes := rawTx(t, []proto.Message{
				&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[3].Bech32, Amount: coins("70000usovr")},
			}, tc.memo, nil)
			bp := mustParse(t, makeBlock(104, 0xAE, 0xAD, txBytes), makeResults(104, okResult()), w)
			require.Len(t, bp.Transfers, 1)
			c := bp.Transfers[0]
			// Classification stays EXTERNAL_DEPOSIT; routing happens via the
			// review reason (deposit → REVIEW_REQUIRED, never auto-credited).
			require.Equal(t, storage.ClassExternalDeposit, c.Classification)
			require.Equal(t, tc.wantReview, c.ReviewReason != "", "review reason %q", c.ReviewReason)
		})
	}
}

func TestParseInternalTransfersNeverExternal(t *testing.T) {
	ws, accts := testWatchSet(t)
	for _, tc := range []struct {
		name     string
		from, to int
		wantIn   storage.Classification
		wantOut  storage.Classification
	}{
		{name: "fee funding to customer", from: 5, to: 2, wantIn: storage.ClassFeeFunding, wantOut: storage.ClassFeeFunding},
		{name: "customer sweep to hot wallet", from: 2, to: 4, wantIn: storage.ClassSweep, wantOut: storage.ClassSweep},
		{name: "customer to customer", from: 2, to: 3, wantIn: storage.ClassInternalTransfer, wantOut: storage.ClassInternalTransfer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			txBytes := rawTx(t, []proto.Message{
				&banktypes.MsgSend{FromAddress: accts[tc.from].Bech32, ToAddress: accts[tc.to].Bech32, Amount: coins("90000usovr")},
			}, "memo", nil)
			bp := mustParse(t, makeBlock(105, 0xAF, 0xAE, txBytes), makeResults(105, okResult()), ws)
			require.Len(t, bp.Transfers, 2)
			var in, out *TransferCandidate
			for i := range bp.Transfers {
				switch bp.Transfers[i].Direction {
				case storage.DirectionIn:
					in = &bp.Transfers[i]
				case storage.DirectionOut:
					out = &bp.Transfers[i]
				}
			}
			require.NotNil(t, in)
			require.NotNil(t, out)
			require.Equal(t, tc.wantIn, in.Classification)
			require.Equal(t, tc.wantOut, out.Classification)
			require.NotEqual(t, storage.ClassExternalDeposit, in.Classification)
			// IN op reuses the coin index; OUT continues after all output coins.
			require.Equal(t, uint32(0), in.OpIndex)
			require.Equal(t, uint32(1), out.OpIndex)
		})
	}
}

func TestParseHotWalletWithdrawalOutflow(t *testing.T) {
	ws, accts := testWatchSet(t)
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[4].Bech32, ToAddress: accts[0].Bech32, Amount: coins("400000usovr")},
	}, "", nil)
	bp := mustParse(t, makeBlock(106, 0xB0, 0xAF, txBytes),
		makeResults(106, okResult(feeEvent("999usovr", accts[4].Bech32))), ws)
	require.Len(t, bp.Transfers, 1)
	require.Equal(t, storage.DirectionOut, bp.Transfers[0].Direction)
	require.Equal(t, storage.ClassWithdrawal, bp.Transfers[0].Classification)
	// Watched payer + fee event present ⇒ FEE_DEDUCTION.
	require.Len(t, bp.FeeDeductions, 1)
	require.Equal(t, accts[4].Bech32, bp.FeeDeductions[0].PayerAddress)
	require.Equal(t, "999", bp.FeeDeductions[0].FeeBaseUnits.String())
}

func TestParseMultiSendMixedInputsRouteToReview(t *testing.T) {
	ws, accts := testWatchSet(t)
	msg := &banktypes.MsgMultiSend{
		Inputs: []banktypes.Input{
			{Address: accts[0].Bech32, Coins: coins("60000usovr")},
			{Address: accts[4].Bech32, Coins: coins("40000usovr")},
		},
		Outputs: []banktypes.Output{
			{Address: accts[2].Bech32, Coins: coins("100000usovr")},
		},
	}
	txBytes := rawTx(t, []proto.Message{msg}, "", nil)
	bp := mustParse(t, makeBlock(107, 0xB1, 0xB0, txBytes), makeResults(107, okResult()), ws)

	var in *TransferCandidate
	var outs []TransferCandidate
	for i := range bp.Transfers {
		if bp.Transfers[i].Direction == storage.DirectionIn {
			in = &bp.Transfers[i]
		} else {
			outs = append(outs, bp.Transfers[i])
		}
	}
	require.NotNil(t, in)
	// Mixed watched/external inputs ⇒ UNATTRIBUTED_REVIEW, ambiguous sender.
	require.Equal(t, storage.ClassUnattributedReview, in.Classification)
	require.NotEmpty(t, in.ReviewReason)
	require.Nil(t, in.SenderAddress)
	require.ElementsMatch(t, []string{accts[0].Bech32, accts[4].Bech32}, in.CounterpartySet)
	// The watched input's own coins give its exact OUT row.
	require.Len(t, outs, 1)
	require.Equal(t, accts[4].Bech32, outs[0].Address)
	require.Equal(t, "40000", outs[0].AmountBaseUnits.String())
	require.Equal(t, uint32(2), outs[0].OpIndex)
}

func TestParseMultiSendSingleInputDeterministicCoinIndex(t *testing.T) {
	ws, accts := testWatchSet(t)
	msg := &banktypes.MsgMultiSend{
		Inputs: []banktypes.Input{{Address: accts[0].Bech32, Coins: coins("30000usovr")}},
		Outputs: []banktypes.Output{
			{Address: accts[1].Bech32, Coins: coins("10000usovr")},         // coin 0, unwatched
			{Address: accts[2].Bech32, Coins: coins("5foo", "20000usovr")}, // coins 1,2
		},
	}
	txBytes := rawTx(t, []proto.Message{msg}, "", nil)
	bp := mustParse(t, makeBlock(108, 0xB2, 0xB1, txBytes), makeResults(108, okResult()), ws)
	require.Len(t, bp.Transfers, 1)
	c := bp.Transfers[0]
	require.Equal(t, storage.ClassExternalDeposit, c.Classification)
	require.Equal(t, uint32(2), c.CoinIndex)
	require.NotNil(t, c.SenderAddress)
	require.Equal(t, accts[0].Bech32, *c.SenderAddress)
}

func TestParseUnsupportedShapeYieldsTxScopedReviewCandidate(t *testing.T) {
	ws, accts := testWatchSet(t)
	unknown, err := codectypes.NewAnyWithValue(&banktypes.MsgSend{
		FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("500000usovr"),
	})
	require.NoError(t, err)
	unknown.TypeUrl = "/cosmos.authz.v1beta1.MsgExec"
	txBytes := rawTxFromAnys(t, []*codectypes.Any{unknown}, "", nil)

	res := okResult(
		feeEvent("100usovr", accts[0].Bech32), // ante event: no msg_index, never a candidate
		client.Event{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[2].Bech32},
			{Key: attrAmount, Value: "500000usovr"},
			{Key: attrMsgIndex, Value: "0"},
		}},
	)
	bp := mustParse(t, makeBlock(109, 0xB3, 0xB2, txBytes), makeResults(109, res), ws)
	require.Empty(t, bp.Transfers)
	require.Len(t, bp.ReviewCandidates, 1)
	rc := bp.ReviewCandidates[0]
	require.Equal(t, storage.DirectionIn, rc.Direction)
	require.Equal(t, accts[2].Bech32, rc.Address)
	require.Equal(t, "500000", rc.AmountBaseUnits.String())
	require.Equal(t, uint32(1), rc.EventIndex)
	require.Equal(t, ReviewOpIndexBase+2, rc.OpIndex)
}

// A tx mixing a supported bank send (msg 0, to a watched addr) with an
// unsupported message (msg 1) must NOT re-emit a review row for msg 0's
// already-attributed transfer — otherwise the watched recipient is counted
// twice in the ledger (canonical transfer + review), yielding a false
// reconciliation discrepancy (PR #300 review).
func TestParseMixedKnownUnknownNoDoubleAttribution(t *testing.T) {
	ws, accts := testWatchSet(t)
	send, err := codectypes.NewAnyWithValue(&banktypes.MsgSend{
		FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("500000usovr"),
	})
	require.NoError(t, err)
	unknown, err := codectypes.NewAnyWithValue(&banktypes.MsgSend{
		FromAddress: accts[0].Bech32, ToAddress: accts[3].Bech32, Amount: coins("700000usovr"),
	})
	require.NoError(t, err)
	unknown.TypeUrl = "/cosmos.authz.v1beta1.MsgExec" // makes the tx not fully attributed
	txBytes := rawTxFromAnys(t, []*codectypes.Any{send, unknown}, "", nil)

	res := okResult(
		// msg 0: the supported send's bank event (already attributed).
		client.Event{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[2].Bech32},
			{Key: attrAmount, Value: "500000usovr"},
			{Key: attrMsgIndex, Value: "0"},
		}},
		// msg 1: the unsupported message's transfer to a watched addr → review.
		client.Event{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[3].Bech32},
			{Key: attrAmount, Value: "700000usovr"},
			{Key: attrMsgIndex, Value: "1"},
		}},
	)
	bp := mustParse(t, makeBlock(120, 0xC0, 0xBF, txBytes), makeResults(120, res), ws)

	// msg 0 attributed once as a canonical transfer; NOT duplicated as review.
	require.Len(t, bp.Transfers, 1)
	require.Equal(t, accts[2].Bech32, bp.Transfers[0].Address)
	require.Equal(t, "500000", bp.Transfers[0].AmountBaseUnits.String())
	// Only msg 1 (unattributed) yields a review candidate.
	require.Len(t, bp.ReviewCandidates, 1)
	require.Equal(t, accts[3].Bech32, bp.ReviewCandidates[0].Address)
	require.Equal(t, uint32(1), bp.ReviewCandidates[0].MessageIndex)
	// No review row references the already-attributed recipient/amount.
	for _, rc := range bp.ReviewCandidates {
		require.NotEqual(t, accts[2].Bech32, rc.Address, "attributed recipient must not appear in review rows")
	}
}

func TestParseFullyAttributedTxNeverEventScanned(t *testing.T) {
	ws, accts := testWatchSet(t)
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("11111usovr")},
	}, "", nil)
	res := okResult(client.Event{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
		{Key: attrReceiver, Value: accts[2].Bech32},
		{Key: attrAmount, Value: "11111usovr"},
		{Key: attrMsgIndex, Value: "0"},
	}})
	bp := mustParse(t, makeBlock(110, 0xB4, 0xB3, txBytes), makeResults(110, res), ws)
	require.Len(t, bp.Transfers, 1)
	require.Empty(t, bp.ReviewCandidates)
}

func TestParseFinalizeBlockEventsBlockScopedOnly(t *testing.T) {
	ws, accts := testWatchSet(t)
	results := makeResults(111)
	results.FinalizeBlockEvents = []client.Event{
		{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[2].Bech32},
			{Key: attrAmount, Value: "8888usovr"},
		}},
		{Type: eventTypeCoinSpent, Attributes: []client.EventAttribute{
			{Key: attrSpender, Value: accts[4].Bech32},
			{Key: attrAmount, Value: "7777usovr"},
		}},
		{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[0].Bech32}, // unwatched
			{Key: attrAmount, Value: "1usovr"},
		}},
	}
	bp := mustParse(t, makeBlock(111, 0xB5, 0xB4), results, ws)
	require.Empty(t, bp.Transfers)
	require.Empty(t, bp.ReviewCandidates, "block events must never yield tx-level candidates")
	require.Len(t, bp.BlockEvents, 2)
	require.Equal(t, storage.DirectionIn, bp.BlockEvents[0].Direction)
	require.Equal(t, uint32(0), bp.BlockEvents[0].EventIndex)
	require.Equal(t, storage.DirectionOut, bp.BlockEvents[1].Direction)
	require.Equal(t, uint32(3), bp.BlockEvents[1].EventIndex)
}

// A single bank movement surfaces as BOTH a coin_received AND a transfer
// event with the same msg_index and amount. Event review must emit exactly
// ONE review candidate for that movement — the transfer view wins (it carries
// the counterparty). Emitting one per event family double-counts the deposit
// in ExpectedBalance and produces a false reconciliation discrepancy (PR #300).
func TestParseEventReviewDedupsCoinReceivedAndTransfer(t *testing.T) {
	ws, accts := testWatchSet(t)
	unknown, err := codectypes.NewAnyWithValue(&banktypes.MsgSend{
		FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("500000usovr"),
	})
	require.NoError(t, err)
	unknown.TypeUrl = "/cosmos.authz.v1beta1.MsgExec" // unattributed → event review
	txBytes := rawTxFromAnys(t, []*codectypes.Any{unknown}, "", nil)

	res := okResult(
		client.Event{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[2].Bech32},
			{Key: attrAmount, Value: "500000usovr"},
			{Key: attrMsgIndex, Value: "0"},
		}},
		client.Event{Type: eventTypeTransfer, Attributes: []client.EventAttribute{
			{Key: attrRecipient, Value: accts[2].Bech32},
			{Key: attrSender, Value: accts[0].Bech32},
			{Key: attrAmount, Value: "500000usovr"},
			{Key: attrMsgIndex, Value: "0"},
		}},
	)
	bp := mustParse(t, makeBlock(115, 0xB9, 0xB8, txBytes), makeResults(115, res), ws)
	require.Empty(t, bp.Transfers)
	require.Len(t, bp.ReviewCandidates, 1, "one movement must yield one review candidate, not one per event family")
	rc := bp.ReviewCandidates[0]
	require.Equal(t, storage.DirectionIn, rc.Direction)
	require.Equal(t, accts[2].Bech32, rc.Address)
	require.Equal(t, "500000", rc.AmountBaseUnits.String())
	// Transfer wins: the candidate carries the sender as its counterparty.
	require.Equal(t, []string{accts[0].Bech32}, rc.CounterpartySet)
	require.Equal(t, uint32(1), rc.EventIndex)
	require.Equal(t, ReviewOpIndexBase+2, rc.OpIndex)
}

// The real chain emits the FULL bank event family for one movement:
// coin_spent (spender), coin_received (receiver), AND transfer (recipient +
// sender). Event review must still yield exactly ONE review candidate for the
// movement — the transfer view wins (carries the counterparty), the
// coin_received IN view is suppressed, and coin_spent names the unwatched
// sender so it contributes no OUT candidate. Emitting one per event family
// would triple the movement in ExpectedBalance (PR #300).
func TestParseEventReviewThreeEventFamilySingleCandidate(t *testing.T) {
	ws, accts := testWatchSet(t)
	unknown, err := codectypes.NewAnyWithValue(&banktypes.MsgSend{
		FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("500000usovr"),
	})
	require.NoError(t, err)
	unknown.TypeUrl = "/cosmos.authz.v1beta1.MsgExec" // unattributed → event review
	txBytes := rawTxFromAnys(t, []*codectypes.Any{unknown}, "", nil)

	res := okResult(
		client.Event{Type: eventTypeCoinSpent, Attributes: []client.EventAttribute{
			{Key: attrSpender, Value: accts[0].Bech32}, // unwatched sender: no OUT candidate
			{Key: attrAmount, Value: "500000usovr"},
			{Key: attrMsgIndex, Value: "0"},
		}},
		client.Event{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[2].Bech32},
			{Key: attrAmount, Value: "500000usovr"},
			{Key: attrMsgIndex, Value: "0"},
		}},
		client.Event{Type: eventTypeTransfer, Attributes: []client.EventAttribute{
			{Key: attrRecipient, Value: accts[2].Bech32},
			{Key: attrSender, Value: accts[0].Bech32},
			{Key: attrAmount, Value: "500000usovr"},
			{Key: attrMsgIndex, Value: "0"},
		}},
	)
	bp := mustParse(t, makeBlock(117, 0xBB, 0xBA, txBytes), makeResults(117, res), ws)
	require.Empty(t, bp.Transfers)
	require.Len(t, bp.ReviewCandidates, 1, "the full three-event family for one movement must yield exactly one review candidate")
	rc := bp.ReviewCandidates[0]
	require.Equal(t, storage.DirectionIn, rc.Direction)
	require.Equal(t, accts[2].Bech32, rc.Address)
	require.Equal(t, "500000", rc.AmountBaseUnits.String())
	// Transfer wins: the candidate carries the sender as its counterparty.
	require.Equal(t, []string{accts[0].Bech32}, rc.CounterpartySet)
	require.Equal(t, uint32(2), rc.EventIndex)
	require.Equal(t, ReviewOpIndexBase+4, rc.OpIndex)
}

// The coin_received/coin_spent + transfer duplication also appears in
// block-scoped finalize events; the same one-movement-one-row dedup applies
// (keyed without msg_index), transfer view winning (PR #300).
func TestParseFinalizeEventsDedupsCoinReceivedAndTransfer(t *testing.T) {
	ws, accts := testWatchSet(t)
	results := makeResults(116)
	results.FinalizeBlockEvents = []client.Event{
		{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[2].Bech32},
			{Key: attrAmount, Value: "6000usovr"},
		}},
		{Type: eventTypeTransfer, Attributes: []client.EventAttribute{
			{Key: attrRecipient, Value: accts[2].Bech32},
			{Key: attrSender, Value: accts[0].Bech32},
			{Key: attrAmount, Value: "6000usovr"},
		}},
	}
	bp := mustParse(t, makeBlock(116, 0xBA, 0xB9), results, ws)
	require.Empty(t, bp.ReviewCandidates)
	require.Len(t, bp.BlockEvents, 1, "one block movement must yield one block event, not one per event family")
	be := bp.BlockEvents[0]
	require.Equal(t, storage.DirectionIn, be.Direction)
	require.Equal(t, accts[2].Bech32, be.Address)
	require.Equal(t, "6000", be.AmountBaseUnits.String())
	require.Equal(t, []string{accts[0].Bech32}, be.CounterpartySet)
	require.Equal(t, uint32(2), be.EventIndex)
}

func TestParseEventReviewPreservesIdenticalTransferMultiplicity(t *testing.T) {
	ws, accts := testWatchSet(t)
	unknown, err := codectypes.NewAnyWithValue(&banktypes.MsgSend{
		FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("7000usovr"),
	})
	require.NoError(t, err)
	unknown.TypeUrl = "/cosmos.authz.v1beta1.MsgExec"
	txBytes := rawTxFromAnys(t, []*codectypes.Any{unknown}, "", nil)

	coinReceived := client.Event{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
		{Key: attrReceiver, Value: accts[2].Bech32},
		{Key: attrAmount, Value: "7000usovr"},
		{Key: attrMsgIndex, Value: "0"},
	}}
	transfer := client.Event{Type: eventTypeTransfer, Attributes: []client.EventAttribute{
		{Key: attrRecipient, Value: accts[2].Bech32},
		{Key: attrSender, Value: accts[0].Bech32},
		{Key: attrAmount, Value: "7000usovr"},
		{Key: attrMsgIndex, Value: "0"},
	}}
	res := okResult(coinReceived, coinReceived, transfer, transfer)
	bp := mustParse(t, makeBlock(118, 0xBC, 0xBB, txBytes), makeResults(118, res), ws)

	require.Len(t, bp.ReviewCandidates, 2, "two canonical transfer occurrences must remain two ledger movements")
	for _, rc := range bp.ReviewCandidates {
		require.Equal(t, storage.DirectionIn, rc.Direction)
		require.Equal(t, "7000", rc.AmountBaseUnits.String())
		require.Equal(t, []string{accts[0].Bech32}, rc.CounterpartySet)
	}
	require.Equal(t, uint32(2), bp.ReviewCandidates[0].EventIndex)
	require.Equal(t, uint32(3), bp.ReviewCandidates[1].EventIndex)
}

func TestParseFinalizeEventsPreservesIdenticalTransferMultiplicity(t *testing.T) {
	ws, accts := testWatchSet(t)
	transfer := client.Event{Type: eventTypeTransfer, Attributes: []client.EventAttribute{
		{Key: attrRecipient, Value: accts[2].Bech32},
		{Key: attrSender, Value: accts[0].Bech32},
		{Key: attrAmount, Value: "8000usovr"},
	}}
	results := makeResults(119)
	results.FinalizeBlockEvents = []client.Event{transfer, transfer}

	bp := mustParse(t, makeBlock(119, 0xBD, 0xBC), results, ws)
	require.Len(t, bp.BlockEvents, 2, "identical canonical movements in one block must not collapse")
	require.Equal(t, uint32(0), bp.BlockEvents[0].EventIndex)
	require.Equal(t, uint32(2), bp.BlockEvents[1].EventIndex)
}

func TestParseFeeEntryOnlyWhenEventPresentMatrix(t *testing.T) {
	ws, accts := testWatchSet(t)
	send := func() []byte {
		return rawTx(t, []proto.Message{
			&banktypes.MsgSend{FromAddress: accts[2].Bech32, ToAddress: accts[4].Bech32, Amount: coins("100usovr")},
		}, "", &txtypes.Fee{Amount: coins("250usovr"), GasLimit: 200000})
	}
	for _, tc := range []struct {
		name      string
		events    []client.Event
		wantCount int
		wantPayer string
	}{
		{name: "no fee event: authinfo fee alone records nothing", events: nil, wantCount: 0},
		{name: "fee event with watched payer", events: []client.Event{feeEvent("250usovr", accts[2].Bech32)}, wantCount: 1, wantPayer: accts[2].Bech32},
		{name: "fee event with unwatched payer", events: []client.Event{feeEvent("250usovr", accts[0].Bech32)}, wantCount: 0},
		{name: "zero fee event records nothing", events: []client.Event{feeEvent("", accts[2].Bech32)}, wantCount: 0},
		{
			name: "feegrant used: granter is the payer",
			events: []client.Event{
				{Type: eventTypeUseFeeGrant, Attributes: []client.EventAttribute{
					{Key: attrGranter, Value: accts[5].Bech32},
					{Key: "grantee", Value: accts[2].Bech32},
				}},
				feeEvent("250usovr", accts[5].Bech32),
			},
			wantCount: 1,
			wantPayer: accts[5].Bech32,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bp := mustParse(t, makeBlock(112, 0xB6, 0xB5, send()),
				makeResults(112, client.TxExecResult{Code: 0, Events: tc.events}), ws)
			require.Len(t, bp.FeeDeductions, tc.wantCount)
			if tc.wantCount > 0 {
				require.Equal(t, tc.wantPayer, bp.FeeDeductions[0].PayerAddress)
				require.Equal(t, "250", bp.FeeDeductions[0].FeeBaseUnits.String())
			}
		})
	}
}

func TestParseExplicitFeePayerWinsOverEventAttr(t *testing.T) {
	ws, accts := testWatchSet(t)
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[1].Bech32, Amount: coins("5usovr")},
	}, "", &txtypes.Fee{Amount: coins("300usovr"), GasLimit: 100000, Payer: accts[4].Bech32})
	bp := mustParse(t, makeBlock(113, 0xB7, 0xB6, txBytes),
		makeResults(113, okResult(feeEvent("300usovr", accts[4].Bech32))), ws)
	require.Len(t, bp.FeeDeductions, 1)
	require.Equal(t, accts[4].Bech32, bp.FeeDeductions[0].PayerAddress)
}

func TestParseUndecodableTxFallsBackToEvents(t *testing.T) {
	ws, accts := testWatchSet(t)
	garbage := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	res := okResult(client.Event{Type: eventTypeTransfer, Attributes: []client.EventAttribute{
		{Key: attrRecipient, Value: accts[2].Bech32},
		{Key: attrSender, Value: accts[0].Bech32},
		{Key: attrAmount, Value: "42usovr"},
		{Key: attrMsgIndex, Value: "0"},
	}})
	bp := mustParse(t, makeBlock(114, 0xB8, 0xB7, garbage), makeResults(114, res), ws)
	require.Empty(t, bp.Transfers)
	require.Len(t, bp.ReviewCandidates, 1)
	require.Equal(t, []string{accts[0].Bech32}, bp.ReviewCandidates[0].CounterpartySet)
}

func TestParseInputMismatchErrors(t *testing.T) {
	ws, _ := testWatchSet(t)
	_, err := ParseBlockTransfers(nil, makeResults(1), ws)
	require.Error(t, err)
	_, err = ParseBlockTransfers(makeBlock(1, 0x01, 0x00), nil, ws)
	require.Error(t, err)
	_, err = ParseBlockTransfers(makeBlock(1, 0x01, 0x00), makeResults(2), ws)
	require.Error(t, err)
	_, err = ParseBlockTransfers(makeBlock(1, 0x01, 0x00, []byte{0x01}), makeResults(1), ws)
	require.Error(t, err, "tx count mismatch")
}

func TestUsovrAmountParsing(t *testing.T) {
	require.Equal(t, sdkmath.NewInt(5), usovrAmount("5usovr"))
	require.Equal(t, sdkmath.NewInt(7), usovrAmount("3foo,7usovr"))
	require.True(t, usovrAmount("").IsZero())
	require.True(t, usovrAmount("12foo").IsZero())
	require.True(t, usovrAmount("not-coins").IsZero())
}
