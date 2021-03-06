package paxos

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	mdbs "github.com/msackman/gomdb/server"
	"goshawkdb.io/common"
	msgs "goshawkdb.io/common/capnp"
	"goshawkdb.io/server"
	"goshawkdb.io/server/db"
	"log"
)

type Acceptor struct {
	txnId           *common.TxnId
	acceptorManager *AcceptorManager
	currentState    acceptorStateMachineComponent
	acceptorReceiveBallots
	acceptorWriteToDisk
	acceptorAwaitLocallyComplete
	acceptorDeleteFromDisk
}

func NewAcceptor(txnId *common.TxnId, txn *msgs.Txn, am *AcceptorManager) *Acceptor {
	a := &Acceptor{
		txnId:           txnId,
		acceptorManager: am,
	}
	a.init(txn)
	return a
}

func AcceptorFromData(txnId *common.TxnId, txn *msgs.Txn, outcome *msgs.Outcome, sendToAll bool, instances *msgs.InstancesForVar_List, am *AcceptorManager) *Acceptor {
	outcomeEqualId := (*outcomeEqualId)(outcome)
	a := NewAcceptor(txnId, txn, am)
	a.ballotAccumulator = BallotAccumulatorFromData(txnId, txn, outcomeEqualId, instances)
	a.outcome = outcomeEqualId
	a.sendToAll = sendToAll
	a.sendToAllOnDisk = sendToAll
	a.outcomeOnDisk = outcomeEqualId
	return a
}

func (a *Acceptor) init(txn *msgs.Txn) {
	a.acceptorReceiveBallots.init(a, txn)
	a.acceptorWriteToDisk.init(a, txn)
	a.acceptorAwaitLocallyComplete.init(a, txn)
	a.acceptorDeleteFromDisk.init(a, txn)
}

func (a *Acceptor) Start() {
	if a.currentState != nil {
		return
	}
	if a.outcomeOnDisk == nil {
		a.currentState = &a.acceptorReceiveBallots
	} else {
		a.currentState = &a.acceptorAwaitLocallyComplete
	}
	a.currentState.start()
}

func (a *Acceptor) Status(sc *server.StatusConsumer) {
	sc.Emit(fmt.Sprintf("Acceptor for %v", a.txnId))
	sc.Emit(fmt.Sprintf("- Current State: %v", a.currentState))
	sc.Emit(fmt.Sprintf("- Outcome determined? %v", a.outcome != nil))
	sc.Emit(fmt.Sprintf("- Pending TLC: %v", a.pendingTLC))
	a.ballotAccumulator.Status(sc.Fork())
	sc.Join()
}

func (a *Acceptor) nextState(requestedState acceptorStateMachineComponent) {
	if requestedState == nil {
		switch a.currentState {
		case &a.acceptorReceiveBallots:
			a.currentState = &a.acceptorWriteToDisk
		case &a.acceptorWriteToDisk:
			a.currentState = &a.acceptorAwaitLocallyComplete
		case &a.acceptorAwaitLocallyComplete:
			a.currentState = &a.acceptorDeleteFromDisk
		case &a.acceptorDeleteFromDisk:
			a.currentState = nil
			return
		}

	} else {
		a.currentState = requestedState
	}

	a.currentState.start()
}

type acceptorStateMachineComponent interface {
	init(*Acceptor, *msgs.Txn)
	start()
	acceptorStateMachineComponentWitness()
}

// receive ballots

type acceptorReceiveBallots struct {
	*Acceptor
	ballotAccumulator *BallotAccumulator
	outcome           *outcomeEqualId
}

func (arb *acceptorReceiveBallots) init(a *Acceptor, txn *msgs.Txn) {
	arb.Acceptor = a
	arb.ballotAccumulator = NewBallotAccumulator(arb.txnId, txn)
}

func (arb *acceptorReceiveBallots) start()                                {}
func (arb *acceptorReceiveBallots) acceptorStateMachineComponentWitness() {}
func (arb *acceptorReceiveBallots) String() string {
	return "acceptorReceiveBallots"
}

func (arb *acceptorReceiveBallots) BallotAccepted(instanceRMId common.RMId, inst *instance, vUUId *common.VarUUId, txn *msgs.Txn) {
	// We can accept a ballot from instanceRMId at any point up until
	// we've received a TLC from instanceRMId (see notes in ALC re
	// retry). Note an acceptor can change it's mind!
	if arb.currentState == &arb.acceptorDeleteFromDisk {
		log.Printf("Error: %v received ballot for instance %v after all TLCs received.", arb.txnId, instanceRMId)
	}
	outcome := arb.ballotAccumulator.BallotReceived(instanceRMId, inst, vUUId, txn)
	if outcome != nil && !outcome.Equal(arb.outcome) {
		arb.outcome = outcome
		arb.nextState(&arb.acceptorWriteToDisk)
	}
}

// write to disk

type acceptorWriteToDisk struct {
	*Acceptor
	outcomeOnDisk   *outcomeEqualId
	sendToAll       bool
	sendToAllOnDisk bool
}

func (awtd *acceptorWriteToDisk) init(a *Acceptor, txn *msgs.Txn) {
	awtd.Acceptor = a
}

func (awtd *acceptorWriteToDisk) start() {
	outcome := awtd.outcome
	outcomeCap := (*msgs.Outcome)(outcome)
	awtd.sendToAll = awtd.sendToAll || outcomeCap.Which() == msgs.OUTCOME_COMMIT
	sendToAll := awtd.sendToAll
	stateSeg := capn.NewBuffer(nil)
	state := msgs.NewRootAcceptorState(stateSeg)
	state.SetTxn(*awtd.ballotAccumulator.Txn)
	state.SetOutcome(*outcomeCap)
	state.SetSendToAll(awtd.sendToAll)
	state.SetInstances(awtd.ballotAccumulator.AddInstancesToSeg(stateSeg))

	data := server.SegToBytes(stateSeg)

	// to ensure correct order of writes, schedule the write from
	// the current go-routine...
	server.Log(awtd.txnId, "Writing 2B to disk...")
	future := awtd.acceptorManager.Disk.ReadWriteTransaction(false, func(rwtxn *mdbs.RWTxn) (interface{}, error) {
		return nil, rwtxn.Put(db.DB.BallotOutcomes, awtd.txnId[:], data, 0)
	})
	go func() {
		// ... but process the result in a new go-routine to avoid blocking the executor.
		if _, err := future.ResultError(); err != nil {
			log.Printf("Error: %v Acceptor Write error: %v", awtd.txnId, err)
			return
		}
		server.Log(awtd.txnId, "Writing 2B to disk...done.")
		awtd.acceptorManager.Exe.Enqueue(func() { awtd.writeDone(outcome, sendToAll) })
	}()
}

func (awtd *acceptorWriteToDisk) acceptorStateMachineComponentWitness() {}
func (awtd *acceptorWriteToDisk) String() string {
	return "acceptorWriteToDisk"
}

func (awtd *acceptorWriteToDisk) writeDone(outcome *outcomeEqualId, sendToAll bool) {
	// There could have been a number a outcomes determined in quick
	// succession. We only "won" if we got here and our outcome is
	// still the right one.
	if awtd.outcome == outcome && awtd.currentState == awtd {
		awtd.outcomeOnDisk = outcome
		awtd.sendToAllOnDisk = sendToAll
		awtd.nextState(nil)
	}
}

// await locally complete

type acceptorAwaitLocallyComplete struct {
	*Acceptor
	pendingTLC    map[common.RMId]server.EmptyStruct
	tlcsReceived  map[common.RMId]server.EmptyStruct
	tgcRecipients common.RMIds
	tscReceived   bool
	twoBSender    *twoBTxnVotesSender
}

func (aalc *acceptorAwaitLocallyComplete) init(a *Acceptor, txn *msgs.Txn) {
	aalc.Acceptor = a
	aalc.tlcsReceived = make(map[common.RMId]server.EmptyStruct, aalc.ballotAccumulator.Txn.Allocations().Len())
}

func (aalc *acceptorAwaitLocallyComplete) start() {
	if aalc.twoBSender != nil {
		aalc.acceptorManager.ConnectionManager.RemoveSenderSync(aalc.twoBSender)
		aalc.twoBSender = nil
	}

	// If our outcome changes, it may look here like we're throwing
	// away TLCs received from proposers/learners. However,
	// proposers/learners wait until all acceptors have given the same
	// answer before issuing any TLCs, so if we are here, we cannot
	// have received any TLCs from anyone... unless we're a retry!  If
	// the txn is a retry then proposers start as soon as they have any
	// ballot, and the ballot accumulator will return a result
	// immediately. However, other ballots can continue to arrive even
	// after a proposer has received F+1 equal outcomes from
	// acceptors. In that case, the acceptor can be here, waiting for
	// TLCs, and can even have received some TLCs when it now receives
	// another ballot. It cannot ignore that ballot because to do so
	// opens the possibility that the acceptors do not arrive at the
	// same outcome and the txn will block.

	allocs := aalc.ballotAccumulator.Txn.Allocations()
	aalc.pendingTLC = make(map[common.RMId]server.EmptyStruct, allocs.Len())
	aalc.tgcRecipients = make([]common.RMId, 0, allocs.Len())
	twoBRecipients := make([]common.RMId, 0, allocs.Len())
	aborted := (*msgs.Outcome)(aalc.outcomeOnDisk).Which() == msgs.OUTCOME_ABORT
	for idx, l := 0, allocs.Len(); idx < l; idx++ {
		alloc := allocs.At(idx)
		active := alloc.Active() != 0
		rmId := common.RMId(alloc.RmId())
		if aalc.sendToAllOnDisk || active {
			twoBRecipients = append(twoBRecipients, rmId)
			if _, found := aalc.tlcsReceived[rmId]; !found {
				aalc.pendingTLC[rmId] = server.EmptyStructVal
			}
		}
		if !aborted || active {
			aalc.tgcRecipients = append(aalc.tgcRecipients, rmId)
		}
	}

	if len(aalc.pendingTLC) == 0 && aalc.tscReceived {
		aalc.maybeDelete()

	} else {
		server.Log(aalc.txnId, "Adding sender for 2B")
		submitter := common.RMId(aalc.ballotAccumulator.Txn.Submitter())
		aalc.twoBSender = newTwoBTxnVotesSender((*msgs.Outcome)(aalc.outcomeOnDisk), aalc.txnId, submitter, twoBRecipients...)
		aalc.acceptorManager.ConnectionManager.AddSender(aalc.twoBSender)
	}
}

func (aalc *acceptorAwaitLocallyComplete) acceptorStateMachineComponentWitness() {}
func (aalc *acceptorAwaitLocallyComplete) String() string {
	return "acceptorAwaitLocallyComplete"
}

func (aalc *acceptorAwaitLocallyComplete) TxnLocallyCompleteReceived(sender common.RMId) {
	aalc.tlcsReceived[sender] = server.EmptyStructVal
	if aalc.currentState == aalc {
		delete(aalc.pendingTLC, sender)
		aalc.maybeDelete()
	}
}

func (aalc *acceptorAwaitLocallyComplete) TxnSubmissionCompleteReceived(sender common.RMId) {
	// Submitter will issues TSCs after FInc outcomes so we can receive this early, which is fine.
	if !aalc.tscReceived {
		aalc.tscReceived = true
		aalc.maybeDelete()
	}
}

func (aalc *acceptorAwaitLocallyComplete) maybeDelete() {
	if aalc.currentState == aalc && aalc.tscReceived && len(aalc.pendingTLC) == 0 {
		aalc.nextState(nil)
	}
}

// delete from disk

type acceptorDeleteFromDisk struct {
	*Acceptor
}

func (adfd *acceptorDeleteFromDisk) init(a *Acceptor, txn *msgs.Txn) {
	adfd.Acceptor = a
}

func (adfd *acceptorDeleteFromDisk) start() {
	if adfd.twoBSender != nil {
		adfd.acceptorManager.ConnectionManager.RemoveSenderSync(adfd.twoBSender)
		adfd.twoBSender = nil
	}
	future := adfd.acceptorManager.Disk.ReadWriteTransaction(false, func(rwtxn *mdbs.RWTxn) (interface{}, error) {
		return nil, rwtxn.Del(db.DB.BallotOutcomes, adfd.txnId[:], nil)
	})
	go func() {
		if _, err := future.ResultError(); err != nil {
			log.Printf("Error: %v Acceptor Deletion error: %v", adfd.txnId, err)
			return
		}
		server.Log(adfd.txnId, "Deleted 2B from disk...done.")
		adfd.acceptorManager.Exe.Enqueue(adfd.deletionDone)
	}()
}

func (adfd *acceptorDeleteFromDisk) acceptorStateMachineComponentWitness() {}
func (adfd *acceptorDeleteFromDisk) String() string {
	return "acceptorDeleteFromDisk"
}

func (adfd *acceptorDeleteFromDisk) deletionDone() {
	if adfd.currentState == adfd {
		adfd.nextState(nil)
		adfd.acceptorManager.AcceptorFinished(adfd.txnId)

		seg := capn.NewBuffer(nil)
		msg := msgs.NewRootMessage(seg)
		tgc := msgs.NewTxnGloballyComplete(seg)
		msg.SetTxnGloballyComplete(tgc)
		tgc.SetTxnId(adfd.txnId[:])
		server.Log(adfd.txnId, "Sending TGC to", adfd.tgcRecipients)
		NewOneShotSender(server.SegToBytes(seg), adfd.acceptorManager.ConnectionManager, adfd.tgcRecipients...)
	}
}

// 2B Sender

type twoBTxnVotesSender struct {
	msg          []byte
	recipients   []common.RMId
	submitterMsg []byte
	submitter    common.RMId
}

func newTwoBTxnVotesSender(outcome *msgs.Outcome, txnId *common.TxnId, submitter common.RMId, recipients ...common.RMId) *twoBTxnVotesSender {
	submitterSeg := capn.NewBuffer(nil)
	submitterMsg := msgs.NewRootMessage(submitterSeg)
	submitterMsg.SetSubmissionOutcome(*outcome)

	if outcome.Which() == msgs.OUTCOME_ABORT {
		abort := outcome.Abort()
		abort.SetResubmit() // nuke out the updates as proposers don't need them.
	}

	seg := capn.NewBuffer(nil)
	msg := msgs.NewRootMessage(seg)
	twoB := msgs.NewTwoBTxnVotes(seg)
	msg.SetTwoBTxnVotes(twoB)
	twoB.SetOutcome(*outcome)

	server.Log(txnId, "Sending 2B to", recipients)

	return &twoBTxnVotesSender{
		msg:          server.SegToBytes(seg),
		recipients:   recipients,
		submitterMsg: server.SegToBytes(submitterSeg),
		submitter:    submitter,
	}
}

func (s *twoBTxnVotesSender) ConnectedRMs(conns map[common.RMId]Connection) {
	if conn, found := conns[s.submitter]; found {
		conn.Send(s.submitterMsg)
	}
	for _, rmId := range s.recipients {
		if conn, found := conns[rmId]; found {
			conn.Send(s.msg)
		}
	}
}

func (s *twoBTxnVotesSender) ConnectionLost(common.RMId, map[common.RMId]Connection) {}

func (s *twoBTxnVotesSender) ConnectionEstablished(rmId common.RMId, conn Connection, conns map[common.RMId]Connection) {
	if s.submitter == rmId {
		conn.Send(s.submitterMsg)
	}
	for _, recipient := range s.recipients {
		if recipient == rmId {
			conn.Send(s.msg)
			break
		}
	}
}
