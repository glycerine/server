package client

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	"goshawkdb.io/common"
	msgs "goshawkdb.io/common/capnp"
	"goshawkdb.io/server"
	ch "goshawkdb.io/server/consistenthash"
	"goshawkdb.io/server/paxos"
	"math/rand"
	"sort"
	"time"
)

type SimpleTxnSubmitter struct {
	rmId                common.RMId
	bootCount           uint32
	disabledHashCodes   map[common.RMId]server.EmptyStruct
	connections         map[common.RMId]paxos.Connection
	connectionManager   paxos.ConnectionManager
	outcomeConsumers    map[common.TxnId]txnOutcomeConsumer
	onShutdown          map[*func(bool)]server.EmptyStruct
	resolver            *ch.Resolver
	hashCache           *ch.ConsistentHashCache
	topology            *server.Topology
	rng                 *rand.Rand
	bufferedSubmissions []func()
}

var AbortRollError = fmt.Errorf("Not leading hashcode")

type txnOutcomeConsumer func(common.RMId, *common.TxnId, *msgs.Outcome)
type TxnCompletionConsumer func(*common.TxnId, *msgs.Outcome)

func NewSimpleTxnSubmitter(rmId common.RMId, bootCount uint32, topology *server.Topology, cm paxos.ConnectionManager) *SimpleTxnSubmitter {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	resolver := ch.NewResolver(rng, topology.AllRMs)
	disabled := make(map[common.RMId]server.EmptyStruct, len(topology.AllRMs))
	for _, rmId := range topology.AllRMs {
		disabled[rmId] = server.EmptyStructVal
	}

	cache := ch.NewCache(resolver, topology.AllRMs.NonEmptyLen(), rng)
	if topology.RootVarUUId != nil {
		cache.AddPosition(topology.RootVarUUId, topology.RootPositions)
	}

	sts := &SimpleTxnSubmitter{
		rmId:              rmId,
		bootCount:         bootCount,
		disabledHashCodes: disabled,
		connections:       nil,
		connectionManager: cm,
		outcomeConsumers:  make(map[common.TxnId]txnOutcomeConsumer),
		onShutdown:        make(map[*func(bool)]server.EmptyStruct),
		resolver:          resolver,
		hashCache:         cache,
		topology:          topology,
		rng:               rng,
	}
	return sts
}

func (sts *SimpleTxnSubmitter) Status(sc *server.StatusConsumer) {
	txnIds := make([]common.TxnId, 0, len(sts.outcomeConsumers))
	for txnId := range sts.outcomeConsumers {
		txnIds = append(txnIds, txnId)
	}
	sc.Emit(fmt.Sprintf("SimpleTxnSubmitter: live TxnIds: %v", txnIds))
	sc.Join()
}

func (sts *SimpleTxnSubmitter) EnsurePositions(varPosMap map[common.VarUUId]*common.Positions) {
	for vUUId, pos := range varPosMap {
		sts.hashCache.AddPosition(&vUUId, pos)
	}
}

func (sts *SimpleTxnSubmitter) SubmissionOutcomeReceived(sender common.RMId, txnId *common.TxnId, outcome *msgs.Outcome) {
	if consumer, found := sts.outcomeConsumers[*txnId]; found {
		consumer(sender, txnId, outcome)
	} else {
		paxos.NewOneShotSender(paxos.MakeTxnSubmissionCompleteMsg(txnId), sts.connectionManager, sender)
	}
}

func (sts *SimpleTxnSubmitter) SubmitTransaction(txnCap *msgs.Txn, activeRMs []common.RMId, continuation TxnCompletionConsumer, delay time.Duration) {
	seg := capn.NewBuffer(nil)
	msg := msgs.NewRootMessage(seg)
	msg.SetTxnSubmission(*txnCap)

	txnId := common.MakeTxnId(txnCap.Id())
	server.Log(txnId, "Submitting txn")
	txnSender := paxos.NewRepeatingSender(server.SegToBytes(seg), activeRMs...)
	if delay == 0 {
		sts.connectionManager.AddSender(txnSender)
	} else {
		go func() {
			// fmt.Printf("%v ", delay)
			time.Sleep(delay)
			sts.connectionManager.AddSender(txnSender)
		}()
	}
	acceptors := paxos.GetAcceptorsFromTxn(txnCap)

	shutdownFun := func(shutdown bool) {
		delete(sts.outcomeConsumers, *txnId)
		// fmt.Printf("sts%v ", len(sts.outcomeConsumers))
		sts.connectionManager.RemoveSenderAsync(txnSender)
		paxos.NewOneShotSender(paxos.MakeTxnSubmissionCompleteMsg(txnId), sts.connectionManager, acceptors...)
		if shutdown {
			if txnCap.Retry() {
				paxos.NewOneShotSender(paxos.MakeTxnSubmissionAbortMsg(txnId), sts.connectionManager, activeRMs...)
			}
			continuation(txnId, nil)
		}
	}
	shutdownFunPtr := &shutdownFun
	sts.onShutdown[shutdownFunPtr] = server.EmptyStructVal

	outcomeAccumulator := paxos.NewOutcomeAccumulator(int(txnCap.FInc()), acceptors)
	consumer := func(sender common.RMId, txnId *common.TxnId, outcome *msgs.Outcome) {
		if outcome, _ = outcomeAccumulator.BallotOutcomeReceived(sender, outcome); outcome != nil {
			delete(sts.onShutdown, shutdownFunPtr)
			shutdownFun(false)
			continuation(txnId, outcome)
		}
	}
	sts.outcomeConsumers[*txnId] = consumer
	// fmt.Printf("sts%v ", len(sts.outcomeConsumers))
}

func (sts *SimpleTxnSubmitter) SubmitClientTransaction(ctxnCap *msgs.ClientTxn, continuation TxnCompletionConsumer, delay time.Duration) error {
	if sts.topology.Equal(server.BlankTopology) {
		fun := func() { sts.SubmitClientTransaction(ctxnCap, continuation, delay) }
		if sts.bufferedSubmissions == nil {
			sts.bufferedSubmissions = []func(){fun}
		} else {
			sts.bufferedSubmissions = append(sts.bufferedSubmissions, fun)
		}
		return nil
	}
	txnCap, activeRMs, _, err := sts.clientToServerTxn(ctxnCap)
	if err != nil {
		return err
	}
	sts.SubmitTransaction(txnCap, activeRMs, continuation, delay)
	return nil
}

func (sts *SimpleTxnSubmitter) TopologyChange(topology *server.Topology, servers map[common.RMId]paxos.Connection) {
	if topology != nil {
		server.Log("TM setting topology to", topology)
		sts.topology = topology
		sts.resolver = ch.NewResolver(sts.rng, topology.AllRMs)
		sts.hashCache.SetResolverDesiredLen(sts.resolver, topology.AllRMs.NonEmptyLen())
		if topology.RootVarUUId != nil {
			sts.hashCache.AddPosition(topology.RootVarUUId, topology.RootPositions)
		}

		if !topology.Equal(server.BlankTopology) && sts.bufferedSubmissions != nil {
			funcs := sts.bufferedSubmissions
			sts.bufferedSubmissions = nil
			for _, fun := range funcs {
				fun()
			}
		}
	}
	if servers != nil {
		sts.disabledHashCodes = make(map[common.RMId]server.EmptyStruct, len(sts.topology.AllRMs))
		for _, rmId := range sts.topology.AllRMs {
			if _, found := servers[rmId]; !found {
				sts.disabledHashCodes[rmId] = server.EmptyStructVal
			}
		}
		sts.connections = servers
		server.Log("TM disabled hash codes", sts.disabledHashCodes)
	}
}

func (sts *SimpleTxnSubmitter) Shutdown() {
	for fun := range sts.onShutdown {
		(*fun)(true)
	}
}

func (sts *SimpleTxnSubmitter) clientToServerTxn(clientTxnCap *msgs.ClientTxn) (*msgs.Txn, []common.RMId, []common.RMId, error) {
	outgoingSeg := capn.NewBuffer(nil)
	txnCap := msgs.NewTxn(outgoingSeg)

	txnCap.SetId(clientTxnCap.Id())
	txnCap.SetRetry(clientTxnCap.Retry())
	txnCap.SetSubmitter(uint32(sts.rmId))
	txnCap.SetSubmitterBootCount(sts.bootCount)
	txnCap.SetFInc(sts.topology.FInc)
	txnCap.SetTopologyVersion(sts.topology.Version)

	clientActions := clientTxnCap.Actions()
	actions := msgs.NewActionList(outgoingSeg, clientActions.Len())
	txnCap.SetActions(actions)
	picker := ch.NewCombinationPicker(int(sts.topology.FInc), sts.disabledHashCodes)

	rmIdToActionIndices, err := sts.translateActions(outgoingSeg, picker, &actions, &clientActions)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Error translating actions: %v", err)
	}

	// NB: we're guaranteed that activeRMs and passiveRMs are
	// disjoint. Thus there is no RM that has some active and some
	// passive actions.
	activeRMs, passiveRMs, err := picker.Choose()
	if err != nil {
		return nil, nil, nil, err
	}
	allocations := msgs.NewAllocationList(outgoingSeg, len(activeRMs)+len(passiveRMs))
	txnCap.SetAllocations(allocations)
	sts.setAllocations(0, rmIdToActionIndices, &allocations, outgoingSeg, true, activeRMs)
	sts.setAllocations(len(activeRMs), rmIdToActionIndices, &allocations, outgoingSeg, false, passiveRMs)
	return &txnCap, activeRMs, passiveRMs, nil
}

func (sts *SimpleTxnSubmitter) setAllocations(allocIdx int, rmIdToActionIndices map[common.RMId]*[]int, allocations *msgs.Allocation_List, seg *capn.Segment, active bool, rmIds []common.RMId) {
	for _, rmId := range rmIds {
		actionIndices := *(rmIdToActionIndices[rmId])
		sort.Ints(actionIndices)
		allocation := allocations.At(allocIdx)
		allocation.SetRmId(uint32(rmId))
		actionIndicesCap := seg.NewUInt16List(len(actionIndices))
		allocation.SetActionIndices(actionIndicesCap)
		for k, v := range actionIndices {
			actionIndicesCap.Set(k, uint16(v))
		}
		if active {
			allocation.SetActive(sts.connections[rmId].BootCount())
		} else {
			allocation.SetActive(0)
		}
		allocIdx++
	}
}

func (sts *SimpleTxnSubmitter) translateActions(outgoingSeg *capn.Segment, picker *ch.CombinationPicker, actions *msgs.Action_List, clientActions *msgs.ClientAction_List) (map[common.RMId]*[]int, error) {

	referencesInNeedOfPositions := []*msgs.VarIdPos{}
	rmIdToActionIndices := make(map[common.RMId]*[]int)
	createdPositions := make(map[common.VarUUId]*common.Positions)

	for idx, l := 0, clientActions.Len(); idx < l; idx++ {
		clientAction := clientActions.At(idx)
		action := actions.At(idx)
		action.SetVarId(clientAction.VarId())

		var err error
		var hashCodes []common.RMId

		switch clientAction.Which() {
		case msgs.CLIENTACTION_READ:
			sts.translateRead(&action, &clientAction)

		case msgs.CLIENTACTION_WRITE:
			sts.translateWrite(outgoingSeg, &referencesInNeedOfPositions, &action, &clientAction)

		case msgs.CLIENTACTION_READWRITE:
			sts.translateReadWrite(outgoingSeg, &referencesInNeedOfPositions, &action, &clientAction)

		case msgs.CLIENTACTION_CREATE:
			var positions *common.Positions
			positions, hashCodes, err = sts.translateCreate(outgoingSeg, &referencesInNeedOfPositions, &action, &clientAction)
			if err != nil {
				return nil, err
			}
			vUUId := common.MakeVarUUId(clientAction.VarId())
			createdPositions[*vUUId] = positions

		case msgs.CLIENTACTION_ROLL:
			sts.translateRoll(outgoingSeg, &referencesInNeedOfPositions, &action, &clientAction)

		default:
			panic(fmt.Sprintf("Unexpected action type: %v", clientAction.Which()))
		}

		if hashCodes == nil {
			hashCodes, err = sts.hashCache.GetHashCodes(common.MakeVarUUId(action.VarId()))
			if err != nil {
				return nil, err
			}

			if clientAction.Which() == msgs.CLIENTACTION_ROLL && hashCodes[0] != sts.rmId {
				return nil, AbortRollError
			}
		}
		hashCodes = hashCodes[:sts.topology.TwoFInc]
		picker.AddPermutation(hashCodes)
		for _, rmId := range hashCodes {
			if listPtr, found := rmIdToActionIndices[rmId]; found {
				*listPtr = append(*listPtr, idx)
			} else {
				// Use of l for capacity guarantees an upper bound: there
				// are only l actions in total, so each RM can't possibly
				// be involved in > l actions. May waste a tiny amount of
				// memory, but minimises mallocs and copying.
				list := make([]int, 1, l)
				list[0] = idx
				rmIdToActionIndices[rmId] = &list
			}
		}
	}

	// Some of the references may be to vars that are being
	// created. Consequently, we must process all of the actions first
	// to make sure all the positions actually exist before adding the
	// positions into the references.
	for _, vUUIdPos := range referencesInNeedOfPositions {
		vUUId := common.MakeVarUUId(vUUIdPos.Id())
		positions, found := createdPositions[*vUUId]
		if !found {
			positions = sts.hashCache.GetPositions(vUUId)
		}
		if positions == nil {
			return nil, fmt.Errorf("Txn contains reference to unknown var %v", vUUId)
		}
		vUUIdPos.SetPositions((capn.UInt8List)(*positions))
	}
	return rmIdToActionIndices, nil
}

func (sts *SimpleTxnSubmitter) translateRead(action *msgs.Action, clientAction *msgs.ClientAction) {
	action.SetRead()
	clientRead := clientAction.Read()
	read := action.Read()
	read.SetVersion(clientRead.Version())
}

func (sts *SimpleTxnSubmitter) translateWrite(outgoingSeg *capn.Segment, referencesInNeedOfPositions *[]*msgs.VarIdPos, action *msgs.Action, clientAction *msgs.ClientAction) {
	action.SetWrite()
	clientWrite := clientAction.Write()
	write := action.Write()
	write.SetValue(clientWrite.Value())
	clientReferences := clientWrite.References()
	references := msgs.NewVarIdPosList(outgoingSeg, clientReferences.Len())
	write.SetReferences(references)
	copyReferences(&clientReferences, &references, referencesInNeedOfPositions)
}

func (sts *SimpleTxnSubmitter) translateReadWrite(outgoingSeg *capn.Segment, referencesInNeedOfPositions *[]*msgs.VarIdPos, action *msgs.Action, clientAction *msgs.ClientAction) {
	action.SetReadwrite()
	clientReadWrite := clientAction.Readwrite()
	readWrite := action.Readwrite()
	readWrite.SetVersion(clientReadWrite.Version())
	readWrite.SetValue(clientReadWrite.Value())
	clientReferences := clientReadWrite.References()
	references := msgs.NewVarIdPosList(outgoingSeg, clientReferences.Len())
	readWrite.SetReferences(references)
	copyReferences(&clientReferences, &references, referencesInNeedOfPositions)
}

func (sts *SimpleTxnSubmitter) translateCreate(outgoingSeg *capn.Segment, referencesInNeedOfPositions *[]*msgs.VarIdPos, action *msgs.Action, clientAction *msgs.ClientAction) (*common.Positions, []common.RMId, error) {
	action.SetCreate()
	clientCreate := clientAction.Create()
	create := action.Create()
	create.SetValue(clientCreate.Value())
	vUUId := common.MakeVarUUId(clientAction.VarId())
	positions, hashCodes, err := sts.hashCache.CreatePositions(vUUId, int(sts.topology.MaxRMCount))
	if err != nil {
		return nil, nil, err
	}
	create.SetPositions((capn.UInt8List)(*positions))
	clientReferences := clientCreate.References()
	references := msgs.NewVarIdPosList(outgoingSeg, clientReferences.Len())
	create.SetReferences(references)
	copyReferences(&clientReferences, &references, referencesInNeedOfPositions)
	return positions, hashCodes, nil
}

func (sts *SimpleTxnSubmitter) translateRoll(outgoingSeg *capn.Segment, referencesInNeedOfPositions *[]*msgs.VarIdPos, action *msgs.Action, clientAction *msgs.ClientAction) {
	action.SetRoll()
	clientRoll := clientAction.Roll()
	roll := action.Roll()
	roll.SetVersion(clientRoll.Version())
	roll.SetValue(clientRoll.Value())
	clientReferences := clientRoll.References()
	references := msgs.NewVarIdPosList(outgoingSeg, clientReferences.Len())
	roll.SetReferences(references)
	copyReferences(&clientReferences, &references, referencesInNeedOfPositions)
}

func copyReferences(clientReferences *capn.DataList, references *msgs.VarIdPos_List, referencesInNeedOfPositions *[]*msgs.VarIdPos) {
	for idx, l := 0, clientReferences.Len(); idx < l; idx++ {
		vUUIdPos := references.At(idx)
		vUUId := clientReferences.At(idx)
		vUUIdPos.SetId(vUUId)
		*referencesInNeedOfPositions = append(*referencesInNeedOfPositions, &vUUIdPos)
	}
}
