// Package deposits is the kit's deposit-scanning core (contracts/
// go-client-api.md §deposits, R6): tolerant block parsing and watch-set
// classification into the ChainTransferLedger (parse.go), FR-023
// credit-condition evaluation and the transactional block/credit write paths
// (state.go), and the checkpointed ascending scanner engine (scanner.go).
package deposits

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// Only these two message shapes are ever unpacked (R6): the SDK TxConfig
// decoder's strict unknown-type rejection fails on this chain's
// custom-module traffic, so TxRaw → TxBody + AuthInfo are decoded with
// gogoproto and every other Any is kept packed.
const (
	TypeURLMsgSend      = "/cosmos.bank.v1beta1.MsgSend"
	TypeURLMsgMultiSend = "/cosmos.bank.v1beta1.MsgMultiSend"
)

// ReviewOpIndexBase offsets event-derived UNATTRIBUTED_REVIEW ledger rows
// above every body-decoded op index (bounded by 2 × the message's coin
// count), so tx-scoped review entries never collide with decoded transfers
// under UNIQUE(chain_id, tx_hash, message_index, op_index).
const ReviewOpIndexBase uint32 = 1 << 16

// Event and attribute names emitted by the SDK bank module, the fee ante
// decorator, feegrant, and baseapp message execution.
const (
	eventTypeTransfer     = "transfer"
	eventTypeCoinReceived = "coin_received"
	eventTypeCoinSpent    = "coin_spent"
	eventTypeTx           = "tx"
	eventTypeUseFeeGrant  = "use_feegrant"

	attrRecipient = "recipient"
	attrSender    = "sender"
	attrReceiver  = "receiver"
	attrSpender   = "spender"
	attrAmount    = "amount"
	attrFee       = "fee"
	attrFeePayer  = "fee_payer"
	attrGranter   = "granter"
	attrMsgIndex  = "msg_index"
)

// TransferCandidate is one classified usovr movement decoded from a tx body
// — exactly one ChainTransferLedger row (data model §3). Deposit records
// derive only from Direction IN + Classification EXTERNAL_DEPOSIT rows.
type TransferCandidate struct {
	TxHash       string
	MessageIndex uint32
	// CoinIndex is the deposit-identity output-coin index (FR-024):
	// outputs' coins flattened in wire order, non-usovr coins included in
	// the numbering so indexes are stable regardless of filtering.
	CoinIndex uint32
	// OpIndex is the ledger row index: IN rows reuse CoinIndex; sender-side
	// OUT rows continue the numbering after all output coins.
	OpIndex   uint32
	Direction storage.LedgerDirection
	// Address is the watched address this row is about.
	Address string
	// CounterpartySet is the full opposite-side address set — MsgMultiSend
	// inputs have no deterministic input→output attribution.
	CounterpartySet []string
	// SenderAddress is the single unambiguous input for IN rows; nil when
	// the input set is ambiguous.
	SenderAddress   *string
	Denom           string
	AmountBaseUnits sdkmath.Int
	Memo            string
	TxCode          uint32
	TxLog           string
	Classification  storage.Classification
	// ReviewReason is non-empty when the movement must route to review
	// (omnibus memo policy, mixed watched/external input set).
	ReviewReason string
}

// LedgerEntry converts the candidate to its ChainTransferLedger row.
func (c TransferCandidate) LedgerEntry(chainID string, blockHeight uint64, now time.Time) storage.LedgerEntry {
	return storage.LedgerEntry{
		ChainID:         chainID,
		Kind:            storage.LedgerKindTx,
		TxHash:          c.TxHash,
		MessageIndex:    c.MessageIndex,
		OpIndex:         c.OpIndex,
		BlockHeight:     blockHeight,
		Direction:       c.Direction,
		Address:         c.Address,
		CounterpartySet: c.CounterpartySet,
		AmountBaseUnits: c.AmountBaseUnits,
		Denom:           c.Denom,
		TxCode:          c.TxCode,
		Classification:  c.Classification,
		CreatedAt:       now,
	}
}

// FeeDeduction is one FEE_DEDUCTION capture (data model §8a). Recorded iff
// the fee-deduction ante event was present and the resolved payer is watched.
type FeeDeduction struct {
	TxHash         string
	PayerAddress   string
	FeeBaseUnits   sdkmath.Int
	TxCode         uint32
	FeeGranterUsed bool
	GranterAddress string
}

// ReviewCandidate is a tx-correlated secondary-detection hit (FR-030): a
// watched address appears in txs_results[i].events of a tx the body decode
// could not fully attribute (authz-wrapped sends, wasm transfers, module
// payouts). Never a crediting source.
type ReviewCandidate struct {
	TxHash       string
	MessageIndex uint32
	EventIndex   uint32
	// OpIndex is ReviewOpIndexBase + 2×EventIndex (+1 for OUT rows).
	OpIndex         uint32
	Direction       storage.LedgerDirection
	Address         string
	CounterpartySet []string
	AmountBaseUnits sdkmath.Int
	TxCode          uint32
	Reason          string
}

// LedgerEntry converts the review candidate to its tx-scoped
// UNATTRIBUTED_REVIEW ledger row.
func (c ReviewCandidate) LedgerEntry(chainID string, blockHeight uint64, now time.Time) storage.LedgerEntry {
	return storage.LedgerEntry{
		ChainID:         chainID,
		Kind:            storage.LedgerKindTx,
		TxHash:          c.TxHash,
		MessageIndex:    c.MessageIndex,
		OpIndex:         c.OpIndex,
		BlockHeight:     blockHeight,
		Direction:       c.Direction,
		Address:         c.Address,
		CounterpartySet: c.CounterpartySet,
		AmountBaseUnits: c.AmountBaseUnits,
		Denom:           storage.BaseDenom,
		TxCode:          c.TxCode,
		Classification:  storage.ClassUnattributedReview,
		CreatedAt:       now,
	}
}

// BlockEventEntry is a block-scoped unattributed-activity record derived
// from finalize_block_events, which carry no transaction association —
// never a tx-level candidate (R6 event scoping).
type BlockEventEntry struct {
	// EventIndex is 2 × the finalize_block_events index (+1 for OUT rows)
	// so one event touching two watched addresses yields distinct rows
	// under UNIQUE(chain_id, block_height, event_index).
	EventIndex      uint32
	Direction       storage.LedgerDirection
	Address         string
	CounterpartySet []string
	AmountBaseUnits sdkmath.Int
	Reason          string
}

// LedgerEntry converts the record to its BLOCK_EVENT ledger row.
func (b BlockEventEntry) LedgerEntry(chainID string, blockHeight uint64, now time.Time) storage.LedgerEntry {
	return storage.LedgerEntry{
		ChainID:         chainID,
		Kind:            storage.LedgerKindBlockEvent,
		BlockHeight:     blockHeight,
		EventIndex:      b.EventIndex,
		Direction:       b.Direction,
		Address:         b.Address,
		CounterpartySet: b.CounterpartySet,
		AmountBaseUnits: b.AmountBaseUnits,
		Denom:           storage.BaseDenom,
		Classification:  storage.ClassUnattributedReview,
		CreatedAt:       now,
	}
}

// BlockParse is the full parse output for one block.
type BlockParse struct {
	ChainID       string
	Height        uint64
	BlockHash     string
	LastBlockHash string
	Time          time.Time

	Transfers        []TransferCandidate
	FeeDeductions    []FeeDeduction
	ReviewCandidates []ReviewCandidate
	BlockEvents      []BlockEventEntry
}

// TxHashHex is the CometBFT transaction hash: uppercase hex SHA-256 of the
// raw tx bytes.
func TxHashHex(txBytes []byte) string {
	sum := sha256.Sum256(txBytes)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// ParseBlockTransfers performs the tolerant raw decode and watch-set
// classification for one block (contracts/go-client-api.md §deposits).
// Individual undecodable txs are never an error — they route through the
// event secondary-detection channel instead.
func ParseBlockTransfers(block *client.Block, results *client.BlockResults, watch WatchSet) (*BlockParse, error) {
	if block == nil || results == nil {
		return nil, fmt.Errorf("deposits: nil block or block results")
	}
	if block.Height != results.Height {
		return nil, fmt.Errorf("deposits: block height %d does not match results height %d", block.Height, results.Height)
	}
	if len(block.Txs) != len(results.TxResults) {
		return nil, fmt.Errorf("deposits: block carries %d txs but results carry %d", len(block.Txs), len(results.TxResults))
	}
	if block.Height < 0 {
		return nil, fmt.Errorf("deposits: negative block height %d", block.Height)
	}

	bp := &BlockParse{
		ChainID:       block.ChainID,
		Height:        uint64(block.Height),
		BlockHash:     strings.ToUpper(hex.EncodeToString(block.Hash)),
		LastBlockHash: strings.ToUpper(hex.EncodeToString(block.LastBlockHash)),
		Time:          block.Time,
	}
	p := &parser{watch: watch, out: bp}
	for i, txBytes := range block.Txs {
		p.tx(txBytes, results.TxResults[i])
	}
	p.blockEvents(results.FinalizeBlockEvents)
	return bp, nil
}

type parser struct {
	watch WatchSet
	out   *BlockParse
}

func (p *parser) tx(txBytes []byte, res client.TxExecResult) {
	txHash := TxHashHex(txBytes)

	var raw txtypes.TxRaw
	var body txtypes.TxBody
	var auth txtypes.AuthInfo
	rawOK := proto.Unmarshal(txBytes, &raw) == nil
	bodyOK := rawOK && proto.Unmarshal(raw.BodyBytes, &body) == nil
	authOK := rawOK && proto.Unmarshal(raw.AuthInfoBytes, &auth) == nil

	fullyAttributed := bodyOK
	// Message indexes the body decode canonically attributed (emitted transfer
	// rows for). Event review must NOT re-emit review rows for these — doing so
	// double-counts a watched recipient in the ledger (ExpectedBalance counts
	// review rows), producing a false reconciliation discrepancy.
	attributed := map[uint32]bool{}
	if bodyOK {
		for mi, anyMsg := range body.Messages {
			if anyMsg == nil {
				fullyAttributed = false
				continue
			}
			switch anyMsg.TypeUrl {
			case TypeURLMsgSend:
				var m banktypes.MsgSend
				if proto.Unmarshal(anyMsg.Value, &m) != nil {
					fullyAttributed = false
					continue
				}
				p.send(txHash, uint32(mi), &m, body.Memo, res)
				attributed[uint32(mi)] = true
			case TypeURLMsgMultiSend:
				var m banktypes.MsgMultiSend
				if proto.Unmarshal(anyMsg.Value, &m) != nil {
					fullyAttributed = false
					continue
				}
				p.multiSend(txHash, uint32(mi), &m, body.Memo, res)
				attributed[uint32(mi)] = true
			default:
				fullyAttributed = false
			}
		}
	}

	p.feeDeduction(txHash, &auth, authOK, res)

	// Secondary detection only for successful txs the body decode could not
	// fully attribute; failed txs move no funds (FR-029). Only the message
	// indexes NOT already attributed are eligible for event review.
	if !fullyAttributed && res.Code == 0 {
		p.eventReview(txHash, res, attributed)
	}
}

func (p *parser) send(txHash string, mi uint32, m *banktypes.MsgSend, memo string, res client.TxExecResult) {
	inputSet := []string{m.FromAddress}
	outputSet := []string{m.ToAddress}
	nOut := uint32(len(m.Amount))
	for k, coin := range m.Amount {
		ci := uint32(k)
		if coin.Denom != storage.BaseDenom || !coin.Amount.IsPositive() {
			continue
		}
		if entry, ok := p.watch.Get(m.ToAddress); ok {
			class, reason := p.classifyIn(entry, inputSet)
			if class == storage.ClassExternalDeposit && entry.MemoRequired && !p.watch.memoRecognized(memo) {
				reason = "omnibus memo required but missing or unrecognized (FR-016)"
			}
			sender := m.FromAddress
			p.out.Transfers = append(p.out.Transfers, TransferCandidate{
				TxHash: txHash, MessageIndex: mi, CoinIndex: ci, OpIndex: ci,
				Direction: storage.DirectionIn, Address: m.ToAddress,
				CounterpartySet: inputSet, SenderAddress: &sender,
				Denom: coin.Denom, AmountBaseUnits: coin.Amount, Memo: memo,
				TxCode: res.Code, TxLog: res.Log,
				Classification: class, ReviewReason: reason,
			})
		}
		if entry, ok := p.watch.Get(m.FromAddress); ok {
			p.out.Transfers = append(p.out.Transfers, TransferCandidate{
				TxHash: txHash, MessageIndex: mi, CoinIndex: ci, OpIndex: nOut + ci,
				Direction: storage.DirectionOut, Address: m.FromAddress,
				CounterpartySet: outputSet,
				Denom:           coin.Denom, AmountBaseUnits: coin.Amount, Memo: memo,
				TxCode: res.Code, TxLog: res.Log,
				Classification: p.classifyOut(entry, outputSet),
			})
		}
	}
}

func (p *parser) multiSend(txHash string, mi uint32, m *banktypes.MsgMultiSend, memo string, res client.TxExecResult) {
	inputSet := make([]string, 0, len(m.Inputs))
	for _, in := range m.Inputs {
		inputSet = append(inputSet, in.Address)
	}
	outputSet := make([]string, 0, len(m.Outputs))
	nOut := uint32(0)
	for _, o := range m.Outputs {
		outputSet = append(outputSet, o.Address)
		nOut += uint32(len(o.Coins))
	}
	var singleSender *string
	if len(inputSet) == 1 {
		singleSender = &inputSet[0]
	}

	ci := uint32(0)
	for _, o := range m.Outputs {
		for _, coin := range o.Coins {
			k := ci
			ci++
			if coin.Denom != storage.BaseDenom || !coin.Amount.IsPositive() {
				continue
			}
			entry, ok := p.watch.Get(o.Address)
			if !ok {
				continue
			}
			class, reason := p.classifyIn(entry, inputSet)
			if class == storage.ClassExternalDeposit && entry.MemoRequired && !p.watch.memoRecognized(memo) {
				reason = "omnibus memo required but missing or unrecognized (FR-016)"
			}
			p.out.Transfers = append(p.out.Transfers, TransferCandidate{
				TxHash: txHash, MessageIndex: mi, CoinIndex: k, OpIndex: k,
				Direction: storage.DirectionIn, Address: o.Address,
				CounterpartySet: inputSet, SenderAddress: singleSender,
				Denom: coin.Denom, AmountBaseUnits: coin.Amount, Memo: memo,
				TxCode: res.Code, TxLog: res.Log,
				Classification: class, ReviewReason: reason,
			})
		}
	}

	// Input-side OUT rows continue the op numbering after all output coins;
	// each input's own coins give exact per-sender outflow attribution.
	ji := uint32(0)
	for _, in := range m.Inputs {
		for _, coin := range in.Coins {
			op := nOut + ji
			ji++
			if coin.Denom != storage.BaseDenom || !coin.Amount.IsPositive() {
				continue
			}
			entry, ok := p.watch.Get(in.Address)
			if !ok {
				continue
			}
			p.out.Transfers = append(p.out.Transfers, TransferCandidate{
				TxHash: txHash, MessageIndex: mi, CoinIndex: op - nOut, OpIndex: op,
				Direction: storage.DirectionOut, Address: in.Address,
				CounterpartySet: outputSet,
				Denom:           coin.Denom, AmountBaseUnits: coin.Amount, Memo: memo,
				TxCode: res.Code, TxLog: res.Log,
				Classification: p.classifyOut(entry, outputSet),
			})
		}
	}
}

// classifyIn applies the data-model §3 input-set rules for a watched
// recipient: entirely external ⇒ EXTERNAL_DEPOSIT; entirely watched ⇒
// internal (never a customer credit — FR-023); mixed ⇒ UNATTRIBUTED_REVIEW.
func (p *parser) classifyIn(recipient storage.WatchedAddress, inputs []string) (storage.Classification, string) {
	anyW, allW := p.watch.watchStats(inputs)
	switch {
	case !anyW:
		return storage.ClassExternalDeposit, ""
	case allW:
		return p.internalInSubtype(recipient, inputs), ""
	default:
		return storage.ClassUnattributedReview, "mixed watched/external input set — no deterministic input→output attribution"
	}
}

func (p *parser) internalInSubtype(recipient storage.WatchedAddress, inputs []string) storage.Classification {
	for _, in := range inputs {
		if e, ok := p.watch.Get(in); ok && e.Kind == storage.WatchFeeWallet {
			return storage.ClassFeeFunding
		}
	}
	if recipient.Kind == storage.WatchHotWallet || recipient.Kind == storage.WatchColdWallet {
		return storage.ClassSweep
	}
	return storage.ClassInternalTransfer
}

// classifyOut classifies a watched sender's outflow: any external output ⇒
// WITHDRAWAL; all-watched outputs ⇒ internal subtype.
func (p *parser) classifyOut(sender storage.WatchedAddress, outputs []string) storage.Classification {
	_, allW := p.watch.watchStats(outputs)
	if !allW {
		return storage.ClassWithdrawal
	}
	if sender.Kind == storage.WatchFeeWallet {
		return storage.ClassFeeFunding
	}
	for _, o := range outputs {
		if e, ok := p.watch.Get(o); ok && (e.Kind == storage.WatchHotWallet || e.Kind == storage.WatchColdWallet) {
			return storage.ClassSweep
		}
	}
	return storage.ClassInternalTransfer
}

// feeDeduction records a FEE_DEDUCTION iff the fee-deduction ante event is
// present (data model §8a): no event ⇒ no deduction ⇒ no entry (pre-ante
// failures pay nothing; post-ante execution failures still pay). Payer
// resolution follows the SDK rule — granter only when a fee grant was
// actually used, else explicit Fee.payer, else the first signer (the event's
// fee_payer attribute, which a tolerant decode uses since packed signer
// pubkeys cannot always be resolved).
func (p *parser) feeDeduction(txHash string, auth *txtypes.AuthInfo, authOK bool, res client.TxExecResult) {
	feeStr, payerAttr, present := findFeeEvent(res.Events)
	if !present {
		return
	}
	granter, grantUsed := findFeeGrantUse(res.Events)
	payer := ""
	switch {
	case grantUsed && granter != "":
		payer = granter
	case authOK && auth.Fee != nil && auth.Fee.Payer != "":
		payer = auth.Fee.Payer
	default:
		payer = payerAttr
	}
	if payer == "" || !p.watch.Contains(payer) {
		return
	}
	amt := usovrAmount(feeStr)
	if !amt.IsPositive() {
		return
	}
	p.out.FeeDeductions = append(p.out.FeeDeductions, FeeDeduction{
		TxHash: txHash, PayerAddress: payer, FeeBaseUnits: amt,
		TxCode: res.Code, FeeGranterUsed: grantUsed, GranterAddress: granter,
	})
}

// eventReview scans txs_results[i].events of a not-fully-attributed tx for
// watched-address activity (FR-030). Only message-execution events carry a
// msg_index attribute; ante/fee events do not and are excluded here (fee
// outflows are captured separately as FEE_DEDUCTION).
//
// A single bank movement surfaces as BOTH a coin_received/coin_spent event
// AND a transfer event with the same msg_index and amount. Transfer events are
// canonical (they carry the counterparty), so matching coin aliases are
// dropped. Every transfer occurrence is retained: two real, identical
// movements can share msg_index, direction, address, and amount.
func (p *parser) eventReview(txHash string, res client.TxExecResult, attributed map[uint32]bool) {
	transferKeys := map[string]bool{}
	for _, ev := range res.Events {
		if ev.Type != eventTypeTransfer {
			continue
		}
		msgIdx, hasMsgIdx := attrUint32(ev, attrMsgIndex)
		if !hasMsgIdx || attributed[msgIdx] {
			continue
		}
		amt := usovrAmount(attr(ev, attrAmount))
		if !amt.IsPositive() {
			continue
		}
		inAddr, outAddr, _ := transferEventParties(ev)
		if inAddr != "" {
			transferKeys[reviewKey(msgIdx, storage.DirectionIn, inAddr, amt)] = true
		}
		if outAddr != "" {
			transferKeys[reviewKey(msgIdx, storage.DirectionOut, outAddr, amt)] = true
		}
	}

	for evIdx, ev := range res.Events {
		msgIdx, hasMsgIdx := attrUint32(ev, attrMsgIndex)
		if !hasMsgIdx {
			continue
		}
		// Skip events belonging to a message the body decode already
		// attributed — their transfers are canonical rows, not review rows.
		if attributed[msgIdx] {
			continue
		}
		inAddr, outAddr, counter := transferEventParties(ev)
		amt := usovrAmount(attr(ev, attrAmount))
		if !amt.IsPositive() {
			continue
		}
		fromTransfer := ev.Type == eventTypeTransfer
		base := ReviewOpIndexBase + 2*uint32(evIdx)
		if inAddr != "" && p.watch.Contains(inAddr) &&
			selectEventFamily(fromTransfer, transferKeys, reviewKey(msgIdx, storage.DirectionIn, inAddr, amt)) {
			p.out.ReviewCandidates = append(p.out.ReviewCandidates, ReviewCandidate{
				TxHash: txHash, MessageIndex: msgIdx, EventIndex: uint32(evIdx), OpIndex: base,
				Direction: storage.DirectionIn, Address: inAddr, CounterpartySet: counter,
				AmountBaseUnits: amt, TxCode: res.Code,
				Reason: "unattributed transfer activity to watched address (unsupported message shape — FR-030)",
			})
		}
		if outAddr != "" && p.watch.Contains(outAddr) &&
			selectEventFamily(fromTransfer, transferKeys, reviewKey(msgIdx, storage.DirectionOut, outAddr, amt)) {
			p.out.ReviewCandidates = append(p.out.ReviewCandidates, ReviewCandidate{
				TxHash: txHash, MessageIndex: msgIdx, EventIndex: uint32(evIdx), OpIndex: base + 1,
				Direction: storage.DirectionOut, Address: outAddr, CounterpartySet: counter,
				AmountBaseUnits: amt, TxCode: res.Code,
				Reason: "unattributed outflow from watched address (unsupported message shape — FR-030)",
			})
		}
	}
}

// blockEvents turns finalize_block_events touching watched addresses into
// block-scoped UNATTRIBUTED_REVIEW records only — never tx-level candidates
// (strict event scoping, R6). The same coin_received/coin_spent + transfer
// duplication applies; dedup is keyed by (direction, address, amount) since
// block-scoped events carry no msg_index (PR #300).
func (p *parser) blockEvents(events []client.Event) {
	transferKeys := map[string]bool{}
	for _, ev := range events {
		if ev.Type != eventTypeTransfer {
			continue
		}
		amt := usovrAmount(attr(ev, attrAmount))
		if !amt.IsPositive() {
			continue
		}
		inAddr, outAddr, _ := transferEventParties(ev)
		if inAddr != "" {
			transferKeys[blockKey(storage.DirectionIn, inAddr, amt)] = true
		}
		if outAddr != "" {
			transferKeys[blockKey(storage.DirectionOut, outAddr, amt)] = true
		}
	}

	for evIdx, ev := range events {
		inAddr, outAddr, counter := transferEventParties(ev)
		amt := usovrAmount(attr(ev, attrAmount))
		if !amt.IsPositive() {
			continue
		}
		fromTransfer := ev.Type == eventTypeTransfer
		base := 2 * uint32(evIdx)
		if inAddr != "" && p.watch.Contains(inAddr) &&
			selectEventFamily(fromTransfer, transferKeys, blockKey(storage.DirectionIn, inAddr, amt)) {
			p.out.BlockEvents = append(p.out.BlockEvents, BlockEventEntry{
				EventIndex: base, Direction: storage.DirectionIn, Address: inAddr,
				CounterpartySet: counter, AmountBaseUnits: amt,
				Reason: "block-scoped transfer activity to watched address (no transaction association)",
			})
		}
		if outAddr != "" && p.watch.Contains(outAddr) &&
			selectEventFamily(fromTransfer, transferKeys, blockKey(storage.DirectionOut, outAddr, amt)) {
			p.out.BlockEvents = append(p.out.BlockEvents, BlockEventEntry{
				EventIndex: base + 1, Direction: storage.DirectionOut, Address: outAddr,
				CounterpartySet: counter, AmountBaseUnits: amt,
				Reason: "block-scoped outflow from watched address (no transaction association)",
			})
		}
	}
}

// selectEventFamily keeps every canonical transfer occurrence and suppresses
// only its less-informative coin_received/coin_spent aliases. When no transfer
// view exists, every coin event is retained because it may represent a
// distinct real movement.
func selectEventFamily(fromTransfer bool, transferKeys map[string]bool, key string) bool {
	return fromTransfer || !transferKeys[key]
}

// reviewKey identifies one tx-scoped movement view for event-family dedup.
func reviewKey(msgIdx uint32, dir storage.LedgerDirection, addr string, amt sdkmath.Int) string {
	return strconv.FormatUint(uint64(msgIdx), 10) + "|" + string(dir) + "|" + addr + "|" + amt.String()
}

// blockKey identifies one block-scoped movement view for event-family dedup
// (no msg_index on finalize_block_events).
func blockKey(dir storage.LedgerDirection, addr string, amt sdkmath.Int) string {
	return string(dir) + "|" + addr + "|" + amt.String()
}

// transferEventParties extracts the receiving / spending addresses from the
// bank transfer-event family.
func transferEventParties(ev client.Event) (inAddr, outAddr string, counterparty []string) {
	switch ev.Type {
	case eventTypeTransfer:
		inAddr, outAddr = attr(ev, attrRecipient), attr(ev, attrSender)
		if outAddr != "" {
			counterparty = append(counterparty, outAddr)
		} else if inAddr != "" {
			counterparty = append(counterparty, inAddr)
		}
	case eventTypeCoinReceived:
		inAddr = attr(ev, attrReceiver)
	case eventTypeCoinSpent:
		outAddr = attr(ev, attrSpender)
	}
	return inAddr, outAddr, counterparty
}

func findFeeEvent(events []client.Event) (fee, payer string, present bool) {
	for _, ev := range events {
		if ev.Type != eventTypeTx {
			continue
		}
		var hasFee bool
		var f, p string
		for _, a := range ev.Attributes {
			switch a.Key {
			case attrFee:
				hasFee = true
				f = a.Value
			case attrFeePayer:
				p = a.Value
			}
		}
		if hasFee {
			return f, p, true
		}
	}
	return "", "", false
}

func findFeeGrantUse(events []client.Event) (granter string, used bool) {
	for _, ev := range events {
		if ev.Type == eventTypeUseFeeGrant {
			return attr(ev, attrGranter), true
		}
	}
	return "", false
}

func attr(ev client.Event, key string) string {
	for _, a := range ev.Attributes {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

func attrUint32(ev client.Event, key string) (uint32, bool) {
	v := attr(ev, key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// usovrAmount extracts the usovr component of a Coins string ("5usovr" /
// "5usovr,2foo"); zero on absence or parse failure (integer-only path).
func usovrAmount(coins string) sdkmath.Int {
	if coins == "" {
		return sdkmath.ZeroInt()
	}
	parsed, err := sdk.ParseCoinsNormalized(coins)
	if err != nil {
		return sdkmath.ZeroInt()
	}
	return parsed.AmountOf(storage.BaseDenom)
}
