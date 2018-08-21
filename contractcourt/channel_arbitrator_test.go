package contractcourt

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

type mockArbitratorLog struct {
	state     ArbitratorState
	newStates chan ArbitratorState
}

// A compile time check to ensure mockArbitratorLog meets the ArbitratorLog
// interface.
var _ ArbitratorLog = (*mockArbitratorLog)(nil)

func (b *mockArbitratorLog) CurrentState() (ArbitratorState, error) {
	return b.state, nil
}

func (b *mockArbitratorLog) CommitState(s ArbitratorState) error {
	b.state = s
	b.newStates <- s
	return nil
}

func (b *mockArbitratorLog) FetchUnresolvedContracts() ([]ContractResolver, error) {
	var contracts []ContractResolver
	return contracts, nil
}

func (b *mockArbitratorLog) InsertUnresolvedContracts(resolvers ...ContractResolver) error {
	return nil
}

func (b *mockArbitratorLog) SwapContract(oldContract, newContract ContractResolver) error {
	return nil
}

func (b *mockArbitratorLog) ResolveContract(res ContractResolver) error {
	return nil
}

func (b *mockArbitratorLog) LogContractResolutions(c *ContractResolutions) error {
	return nil
}

func (b *mockArbitratorLog) FetchContractResolutions() (*ContractResolutions, error) {
	c := &ContractResolutions{}

	return c, nil
}

func (b *mockArbitratorLog) LogChainActions(actions ChainActionMap) error {
	return nil
}

func (b *mockArbitratorLog) FetchChainActions() (ChainActionMap, error) {
	actionsMap := make(ChainActionMap)
	return actionsMap, nil
}

func (b *mockArbitratorLog) WipeHistory() error {
	return nil
}

type mockChainIO struct{}

func (*mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return nil, 0, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint, _ []byte,
	heightHint uint32) (*wire.TxOut, error) {
	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, nil
}

func createTestChannelArbitrator(log ArbitratorLog) (*ChannelArbitrator,
	chan struct{}, error) {
	blockEpoch := &chainntnfs.BlockEpochEvent{
		Cancel: func() {},
	}

	chanPoint := wire.OutPoint{}
	shortChanID := lnwire.ShortChannelID{}
	chanEvents := &ChainEventSubscription{
		RemoteUnilateralClosure: make(chan *lnwallet.UnilateralCloseSummary, 1),
		LocalUnilateralClosure:  make(chan *LocalUnilateralCloseInfo, 1),
		CooperativeClosure:      make(chan *CooperativeCloseInfo, 1),
		ContractBreach:          make(chan *lnwallet.BreachRetribution, 1),
	}

	chainIO := &mockChainIO{}
	chainArbCfg := ChainArbitratorConfig{
		ChainIO: chainIO,
		PublishTx: func(*wire.MsgTx) error {
			return nil
		},
	}

	// We'll use the resolvedChan to synchronize on call to
	// MarkChannelResolved.
	resolvedChan := make(chan struct{}, 1)

	// Next we'll create the matching configuration struct that contains
	// all interfaces and methods the arbitrator needs to do its job.
	arbCfg := ChannelArbitratorConfig{
		ChanPoint:   chanPoint,
		ShortChanID: shortChanID,
		BlockEpochs: blockEpoch,
		MarkChannelResolved: func() error {
			resolvedChan <- struct{}{}
			return nil
		},
		ForceCloseChan: func() (*lnwallet.LocalForceCloseSummary, error) {
			summary := &lnwallet.LocalForceCloseSummary{
				CloseTx:         &wire.MsgTx{},
				HtlcResolutions: &lnwallet.HtlcResolutions{},
			}
			return summary, nil
		},
		MarkCommitmentBroadcasted: func() error {
			return nil
		},
		MarkChannelClosed: func(*channeldb.ChannelCloseSummary) error {
			return nil
		},
		ChainArbitratorConfig: chainArbCfg,
		ChainEvents:           chanEvents,
	}

	return NewChannelArbitrator(arbCfg, nil, log), resolvedChan, nil
}

// assertState checks that the ChannelArbitrator is in the state we expect it
// to be.
func assertState(t *testing.T, c *ChannelArbitrator, expected ArbitratorState) {
	if c.state != expected {
		t.Fatalf("expected state %v, was %v", expected, c.state)
	}
}

// TestChannelArbitratorCooperativeClose tests that the ChannelArbitertor
// correctly marks the channel resolved in case a cooperative close is
// confirmed.
func TestChannelArbitratorCooperativeClose(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// We set up a channel to detect when MarkChannelClosed is called.
	closeInfos := make(chan *channeldb.ChannelCloseSummary)
	chanArb.cfg.MarkChannelClosed = func(
		closeInfo *channeldb.ChannelCloseSummary) error {
		closeInfos <- closeInfo
		return nil
	}

	// Cooperative close should do trigger a MarkChannelClosed +
	// MarkChannelResolved.
	closeInfo := &CooperativeCloseInfo{
		&channeldb.ChannelCloseSummary{},
	}
	chanArb.cfg.ChainEvents.CooperativeClosure <- closeInfo

	select {
	case c := <-closeInfos:
		if c.CloseType != channeldb.CooperativeClose {
			t.Fatalf("expected cooperative close, got %v", c.CloseType)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for channel close")
	}

	// It should mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

func assertStateTransitions(t *testing.T, newStates <-chan ArbitratorState,
	expectedStates ...ArbitratorState) {
	t.Helper()

	for _, exp := range expectedStates {
		var state ArbitratorState
		select {
		case state = <-newStates:
		case <-time.After(5 * time.Second):
			t.Fatalf("new state not received")
		}

		if state != exp {
			t.Fatalf("expected new state %v, got %v", exp, state)
		}
	}
}

// TestChannelArbitratorRemoteForceClose checks that the ChannelArbitrator goes
// through the expected states if a remote force close is observed in the
// chain.
func TestChannelArbitratorRemoteForceClose(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// Send a remote force close event.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}

	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// It should transition StateDefault -> StateContractClosed ->
	// StateFullyResolved.
	assertStateTransitions(
		t, log.newStates, StateContractClosed, StateFullyResolved,
	)

	// It should alos mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorLocalForceClose tests that the ChannelArbitrator goes
// through the expected states in case we request it to force close the channel,
// and the local force close event is observed in chain.
func TestChannelArbitratorLocalForceClose(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// We create a channel we can use to pause the ChannelArbitrator at the
	// point where it broadcasts the close tx, and check its state.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx) error {
		// When the force close tx is being broadcasted, check that the
		// state is correct at that point.
		select {
		case stateChan <- chanArb.state:
		case <-chanArb.quit:
			return fmt.Errorf("exiting")
		}
		return nil
	}

	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	// With the channel found, and the request crafted, we'll send over a
	// force close request to the arbitrator that watches this channel.
	chanArb.forceCloseReqs <- &forceCloseReq{
		errResp: errChan,
		closeTx: respChan,
	}

	// It should transition to StateBroadcastCommit.
	assertStateTransitions(t, log.newStates, StateBroadcastCommit)

	// When it is broadcasting the force close, its state should be
	// StateBroadcastCommit.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("did not get state update")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	assertStateTransitions(t, log.newStates, StateCommitmentBroadcasted)

	select {
	case <-respChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	// After broadcasting the close tx, it should be in state
	// StateCommitmentBroadcasted.
	assertState(t, chanArb, StateCommitmentBroadcasted)

	// Now notify about the local force close getting confirmed.
	chanArb.cfg.ChainEvents.LocalUnilateralClosure <- &LocalUnilateralCloseInfo{
		&chainntnfs.SpendDetail{},
		&lnwallet.LocalForceCloseSummary{
			CloseTx:         &wire.MsgTx{},
			HtlcResolutions: &lnwallet.HtlcResolutions{},
		},
		&channeldb.ChannelCloseSummary{},
	}

	// It should transition StateContractClosed -> StateFullyResolved.
	assertStateTransitions(t, log.newStates, StateContractClosed,
		StateFullyResolved)

	// It should also mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorLocalForceCloseRemoteConfiremd tests that the
// ChannelArbitrator behaves as expected in the case where we request a local
// force close, but a remote commitment ends up being confirmed in chain.
func TestChannelArbitratorLocalForceCloseRemoteConfirmed(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// Create a channel we can use to assert the state when it publishes
	// the close tx.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx) error {
		// When the force close tx is being broadcasted, check that the
		// state is correct at that point.
		select {
		case stateChan <- chanArb.state:
		case <-chanArb.quit:
			return fmt.Errorf("exiting")
		}
		return nil
	}

	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	// With the channel found, and the request crafted, we'll send over a
	// force close request to the arbitrator that watches this channel.
	chanArb.forceCloseReqs <- &forceCloseReq{
		errResp: errChan,
		closeTx: respChan,
	}

	// It should transition to StateBroadcastCommit.
	assertStateTransitions(t, log.newStates, StateBroadcastCommit)

	// We expect it to be in state StateBroadcastCommit when publishing
	// the force close.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no state update received")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	assertStateTransitions(t, log.newStates, StateCommitmentBroadcasted)

	// Wait for a response to the force close.
	select {
	case <-respChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	// The state should be StateCommitmentBroadcasted.
	assertState(t, chanArb, StateCommitmentBroadcasted)

	// Now notify about the _REMOTE_ commitment getting confirmed.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}
	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// It should transition StateContractClosed -> StateFullyResolved.
	assertStateTransitions(t, log.newStates, StateContractClosed,
		StateFullyResolved)

	// It should resolve.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(15 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorLocalForceCloseDoubleSpend tests that the
// ChannelArbitrator behaves as expected in the case where we request a local
// force close, but we fail broadcasting our commitment because a remote
// commitment has already been published.
func TestChannelArbitratorLocalForceDoubleSpend(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// Return ErrDoubleSpend when attempting to publish the tx.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx) error {
		// When the force close tx is being broadcasted, check that the
		// state is correct at that point.
		select {
		case stateChan <- chanArb.state:
		case <-chanArb.quit:
			return fmt.Errorf("exiting")
		}
		return lnwallet.ErrDoubleSpend
	}

	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	// With the channel found, and the request crafted, we'll send over a
	// force close request to the arbitrator that watches this channel.
	chanArb.forceCloseReqs <- &forceCloseReq{
		errResp: errChan,
		closeTx: respChan,
	}

	// It should transition to StateBroadcastCommit.
	assertStateTransitions(t, log.newStates, StateBroadcastCommit)

	// We expect it to be in state StateBroadcastCommit when publishing
	// the force close.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no state update received")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	assertStateTransitions(t, log.newStates, StateCommitmentBroadcasted)

	// Wait for a response to the force close.
	select {
	case <-respChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	// The state should be StateCommitmentBroadcasted.
	assertState(t, chanArb, StateCommitmentBroadcasted)

	// Now notify about the _REMOTE_ commitment getting confirmed.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}
	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// It should transition StateContractClosed -> StateFullyResolved.
	assertStateTransitions(t, log.newStates, StateContractClosed,
		StateFullyResolved)

	// It should resolve.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(15 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}
