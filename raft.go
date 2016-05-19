package raftwrapper

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/cenk/backoff"

	"github.com/coreos/etcd/etcdserver/stats"
	"github.com/coreos/etcd/pkg/types"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/rafthttp"
	"github.com/coreos/etcd/snap"
	"github.com/coreos/etcd/wal"
)

type LogData []byte

// because WAL and Snap look to see if ANY files exist in the dir
// for confirmation. Meaning that if one or the other is enabled
// but not the other, then checks will fail
var walDirExtension = "/wal"
var snapDirExtension = "/snap"

type Node struct {
	node           raft.Node
	raftStorage    *raft.MemoryStorage
	transport      *rafthttp.Transport
	bootstrapPeers []string
	bootstrapNode  bool
	peerMap        map[uint64]confChangeNodeContext
	id             uint64
	cid            uint64
	raftPort       int

	apiPort int

	raftConfig *raft.Config

	started     bool
	initialized bool
	running     bool

	proposeC chan string
	fsm      FSM

	observers     map[uint64]*Observer
	observersLock sync.RWMutex

	initBackoffArgs *InitializationBackoffArgs
	snapshotConfig  *SnapshotConfig

	dataDir string
	ss      *snap.Snapshotter
	wal     *wal.WAL

	lastConfState *raftpb.ConfState

	stopc chan struct{}
}

type NodeConfig struct {
	// If not specified or 0, will autogenerate a new UUID
	ID uint64

	// If not specified 0x100 will be used
	ClusterID uint64

	FSM            FSM
	RaftPort       int
	APIPort        int
	BootstrapPeers []string
	BootstrapNode  bool

	// DataDir is where your data will be persisted to disk
	// for use when either you need to restart a node, or
	// it goes offline and needs to be restarted
	DataDir string

	InitBackoff *InitializationBackoffArgs
	// if nil, then default to no snapshotting
	SnapshotConfig *SnapshotConfig
}

type SnapshotConfig struct {

	// How often do you want to Snapshot and compact logs?
	Interval time.Duration

	// If the interval ticks but not enough logs have been commited then ignore
	// the snapshot this interval
	MinCommittedLogs uint64

	// If the interval hasn't ticked but we've gone over a commited log threshold then snapshot
	// Note: Use this with care. Snapshotting is a fairly expenseive process.
	// Interval is suggested best method for triggering snapshots
	MaxCommittedLogs uint64
}

var DefaultSnapshotConfig = &SnapshotConfig{
	Interval:         -1 * time.Second,
	MinCommittedLogs: 0,
	MaxCommittedLogs: 0,
}

type InitializationBackoffArgs struct {
	InitialInterval     time.Duration
	Multiplier          float64
	MaxInterval         time.Duration
	MaxElapsedTime      time.Duration
	RandomizationFactor float64
}

var DefaultInitializationBackoffArgs = &InitializationBackoffArgs{
	InitialInterval:     500 * time.Millisecond,
	RandomizationFactor: .5,
	Multiplier:          2,
	MaxInterval:         5 * time.Second,
	MaxElapsedTime:      2 * time.Minute,
}

// note: peers is only for asking to join the cluster.
// It will not be able to connect if the peers don't respond to cluster node add request
// This is because each node defines it's own uuid at startup. We must be told this UUID
// by another node.
// TODO: Look into which config options we want others to specify. For now hardcoded
// TODO: Allow user to specify KV pairs of known nodes, and bypass the http discovery
// NOTE: Peers are used EXCLUSIVELY to round-robin to other nodes and attempt to add
//		ourselves to an existing cluster or bootstrap node
func NewNode(args *NodeConfig) (*Node, error) {
	rn, err := nonInitNode(args)
	if err != nil {
		return nil, err
	}

	return rn, nil
}

// TODO: Don't panic on these. Return an err if they happen
func (rn *Node) Start() error {
	if rn.started {
		return nil
	}
	rn.stopc = make(chan struct{})

	if err := rn.restoreRaft(); err != nil {
		return err
	}

	if err := rn.attachTransport(); err != nil {
		return err
	}

	if err := rn.transport.Start(); err != nil {
		return err
	}

	go rn.scanReady()

	// Start config http service
	go func(rn *Node) {
		if err := rn.serveHTTP(); err != nil {
			panic(err)
		}
	}(rn)

	// start raft
	go func(rn *Node) {
		if err := rn.serveRaft(); err != nil {
			panic(err)
		}
	}(rn)
	rn.started = true

	// TODO: Make new endpoint to find members.
	// Then only use addSelfToCluster when needed.
	// Call members endpoint when restoring
	if err := rn.addSelfToCluster(); err != nil {
		return err
	}
	// final step to mark node as initialized
	// TODO: Find a better place to mark initialized
	rn.initialized = true
	rn.running = true
	return nil
}

func (rn *Node) IsRunning() bool {
	return rn.running
}

func (rn *Node) Stop() error {
	close(rn.stopc)
	rn.transport.Stop()
	// TODO: Don't poll stuff here
	for rn.running {
		time.Sleep(200 * time.Millisecond)
	}
	rn.started = false
	rn.initialized = false
	return nil
}

// Destroy is a HARD stop. It first reconfigures the raft cluster
// to remove itself(ONLY do this if you are intending to permenantly leave the cluster and know consequences around consensus) - read the raft paper's reconfiguration section before using this
// It then halts all running goroutines
//
// WARNING! - Destroy will recursively remove everything under <DataDir>/snap and <DataDir>/wal
func (rn *Node) Destroy() error {
	if err := rn.removeSelfFromCluster(); err != nil {
		return err
	}
	close(rn.stopc)
	rn.transport.Stop()
	// TODO: Have a stopped chan for triggering this action
	for rn.running {
		time.Sleep(200 * time.Millisecond)
	}
	rn.deletePersistentData()
	rn.running = false
	rn.started = false
	rn.initialized = false
	return nil
}

func (rn *Node) removeSelfFromCluster() error {
	notify := func(err error, t time.Duration) {
		log.Printf("Couldn't remove self from cluster: %s Trying again in %v", err.Error(), t)
	}

	expBackoff := backoff.NewExponentialBackOff()

	expBackoff.InitialInterval = rn.initBackoffArgs.InitialInterval
	expBackoff.RandomizationFactor = rn.initBackoffArgs.RandomizationFactor
	expBackoff.Multiplier = rn.initBackoffArgs.Multiplier
	expBackoff.MaxInterval = rn.initBackoffArgs.MaxInterval
	expBackoff.MaxElapsedTime = rn.initBackoffArgs.MaxElapsedTime

	op := func() error {
		return rn.requestSelfDeletion()
	}

	return backoff.RetryNotify(op, expBackoff, notify)
}

func (rn *Node) addSelfToCluster() error {
	notify := func(err error, t time.Duration) {
		log.Printf("Couldn't add self to cluster: %s Trying again in %v", err.Error(), t)
	}

	expBackoff := backoff.NewExponentialBackOff()
	expBackoff.InitialInterval = rn.initBackoffArgs.InitialInterval
	expBackoff.RandomizationFactor = rn.initBackoffArgs.RandomizationFactor
	expBackoff.Multiplier = rn.initBackoffArgs.Multiplier
	expBackoff.MaxInterval = rn.initBackoffArgs.MaxInterval
	expBackoff.MaxElapsedTime = rn.initBackoffArgs.MaxElapsedTime

	op := func() error {
		return rn.requestSelfAddition()
	}

	return backoff.RetryNotify(op, expBackoff, notify)
}

func nonInitNode(args *NodeConfig) (*Node, error) {
	if args.BootstrapNode {
		args.BootstrapPeers = nil
	}

	if args.InitBackoff == nil {
		args.InitBackoff = DefaultInitializationBackoffArgs
	}

	if args.SnapshotConfig == nil {
		args.SnapshotConfig = DefaultSnapshotConfig
	}

	rn := &Node{
		proposeC:        make(chan string),
		raftStorage:     raft.NewMemoryStorage(),
		bootstrapPeers:  args.BootstrapPeers,
		bootstrapNode:   args.BootstrapNode,
		id:              args.ID,
		cid:             args.ClusterID,
		raftPort:        args.RaftPort,
		apiPort:         args.APIPort,
		fsm:             args.FSM,
		initialized:     false,
		observers:       make(map[uint64]*Observer),
		peerMap:         make(map[uint64]confChangeNodeContext),
		initBackoffArgs: args.InitBackoff,
		snapshotConfig:  args.SnapshotConfig,
		dataDir:         args.DataDir,
	}

	if rn.id == 0 {
		rn.id = Uint64UUID()
	}
	if rn.cid == 0 {
		rn.cid = 0x100
	}

	//TODO: Fix these magix numbers with user-specifiable config
	rn.raftConfig = &raft.Config{
		ID:              rn.id,
		ElectionTick:    10,
		HeartbeatTick:   1,
		Storage:         rn.raftStorage,
		MaxSizePerMsg:   1024 * 1024,
		MaxInflightMsgs: 256,
		CheckQuorum:     true,
	}

	return rn, nil
}

func (rn *Node) attachTransport() error {
	ss := &stats.ServerStats{}
	ss.Initialize()

	rn.transport = &rafthttp.Transport{
		ID:          types.ID(rn.id),
		ClusterID:   types.ID(rn.cid),
		Raft:        rn,
		Snapshotter: rn.ss,
		ServerStats: ss,
		LeaderStats: stats.NewLeaderStats(strconv.FormatUint(rn.id, 10)),
		ErrorC:      make(chan error),
	}

	return nil
}

func (rn *Node) proposePeerAddition(addReq *raftpb.ConfChange, async bool) error {
	addReq.Type = raftpb.ConfChangeAddNode

	observChan := make(chan Observation)
	// setup listener for node addition
	// before asking for node addition
	if !async {
		filterFn := func(o Observation) bool {

			switch o.(type) {
			case raftpb.Entry:
				entry := o.(raftpb.Entry)
				switch entry.Type {
				case raftpb.EntryConfChange:
					var cc raftpb.ConfChange
					cc.Unmarshal(entry.Data)
					rn.node.ApplyConfChange(cc)
					switch cc.Type {
					case raftpb.ConfChangeAddNode:
						// wait until we get a matching node id
						return addReq.NodeID == cc.NodeID
					default:
						return false
					}
				default:
					return false
				}
			default:
				return false
			}
		}

		observer := NewObserver(observChan, filterFn)
		rn.RegisterObserver(observer)
		defer rn.UnregisterObserver(observer)
	}

	if err := rn.node.ProposeConfChange(context.TODO(), *addReq); err != nil {
		return err
	}

	if async {
		return nil
	}

	select {
	case <-observChan:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("Timed out waiting for config change")
	}
}

func (rn *Node) proposePeerDeletion(delReq *raftpb.ConfChange, async bool) error {
	delReq.Type = raftpb.ConfChangeRemoveNode

	observChan := make(chan Observation)
	// setup listener for node addition
	// before asking for node addition
	if !async {
		filterFn := func(o Observation) bool {
			switch o.(type) {
			case raftpb.Entry:
				entry := o.(raftpb.Entry)
				switch entry.Type {
				case raftpb.EntryConfChange:
					var cc raftpb.ConfChange
					cc.Unmarshal(entry.Data)
					rn.node.ApplyConfChange(cc)
					switch cc.Type {
					case raftpb.ConfChangeRemoveNode:
						// wait until we get a matching node id
						return delReq.NodeID == cc.NodeID
					default:
						return false
					}
				default:
					return false
				}
			default:
				return false
			}
		}

		observer := NewObserver(observChan, filterFn)
		rn.RegisterObserver(observer)
		defer rn.UnregisterObserver(observer)
	}

	if err := rn.node.ProposeConfChange(context.TODO(), *delReq); err != nil {
		return err
	}

	if async {
		return nil
	}

	select {
	case <-observChan:
		return nil
	case <-time.After(10 * time.Second):
		return rn.proposePeerDeletion(delReq, async)

	}
}

func (rn *Node) canAlterPeer() bool {
	return rn.isHealthy() && rn.initialized
}

// TODO: Define healthy
func (rn *Node) isHealthy() bool {
	return rn.running
}

func (rn *Node) scanReady() {
	defer rn.wal.Close()
	defer func(rn *Node) {
		rn.running = false
	}(rn)

	var snapTicker *time.Ticker

	// if non-interval based then create a ticker which will never post to a chan
	if rn.snapshotConfig.Interval <= 0 {
		snapTicker = time.NewTicker(1 * time.Second)
		snapTicker.Stop()
	} else {
		snapTicker = time.NewTicker(rn.snapshotConfig.Interval)
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-rn.stopc:
			return
		case <-ticker.C:
			rn.node.Tick()
		case <-snapTicker.C:
			if err := rn.createSnapAndCompact(); err != nil {
				return
			}
		case rd := <-rn.node.Ready():
			if rn.wal != nil {
				rn.wal.Save(rd.HardState, rd.Entries)
			}
			rn.raftStorage.Append(rd.Entries)
			rn.transport.Send(rd.Messages)

			if !raft.IsEmptySnap(rd.Snapshot) {
				if err := rn.processSnapshot(rd.Snapshot); err != nil {
					return
				}
			}

			if ok := rn.publishEntries(rd.CommittedEntries); !ok {
				return
			}

			rn.node.Advance()

		}
	}
}

func (rn *Node) restoreFSMFromSnapshot(raftSnap raftpb.Snapshot) error {
	if raft.IsEmptySnap(raftSnap) {
		return nil
	}

	if err := rn.fsm.Restore(SnapshotData(raftSnap.Data)); err != nil {
		return err
	}

	return nil
}

func (rn *Node) processSnapshot(raftSnap raftpb.Snapshot) error {
	if err := rn.restoreFSMFromSnapshot(raftSnap); err != nil {
		return err
	}

	if err := rn.persistSnapshot(raftSnap); err != nil {
		return err
	}
	if err := rn.raftStorage.ApplySnapshot(raftSnap); err != nil {
		return err
	}

	rn.ReportSnapshot(rn.id, raft.SnapshotFinish)

	return nil
}

func (rn *Node) createSnapAndCompact() error {
	index := rn.node.Status().Applied

	data, err := rn.fsm.Snapshot()
	if err != nil {
		return err
	}

	raftSnap, err := rn.raftStorage.CreateSnapshot(index, rn.lastConfState, []byte(data))
	if err != nil {
		return err
	}

	if err = rn.raftStorage.Compact(raftSnap.Metadata.Index - 1); err != nil {
		return err
	}

	if err = rn.persistSnapshot(raftSnap); err != nil {
		return err
	}

	return nil
}

func (rn *Node) commitsSinceLastSnap() uint64 {
	raftSnap, err := rn.raftStorage.Snapshot()
	if err != nil {
		// this should NEVER err
		panic(err)
	}
	curIndex, err := rn.raftStorage.LastIndex()
	if err != nil {
		// this should NEVER err
		panic(err)
	}
	return curIndex - raftSnap.Metadata.Index
}

type confChangeNodeContext struct {
	IP       string
	RaftPort int
	APIPort  int
}

func (rn *Node) publishEntries(ents []raftpb.Entry) bool {
	for _, entry := range ents {
		switch entry.Type {
		case raftpb.EntryNormal:
			if len(entry.Data) == 0 {
				break
			}
			// Yes, this is probably a blocking call
			// An FSM should be responsible for being efficient
			// for high-load situations
			if err := rn.fsm.Apply(LogData(entry.Data)); err != nil {
				return false
			}

		case raftpb.EntryConfChange:
			var cc raftpb.ConfChange
			cc.Unmarshal(entry.Data)
			confState := rn.node.ApplyConfChange(cc)
			rn.lastConfState = confState

			switch cc.Type {
			case raftpb.ConfChangeAddNode:
				if len(cc.Context) > 0 {
					var ctxData confChangeNodeContext
					if err := json.Unmarshal(cc.Context, &ctxData); err != nil {
						return false
					}

					raftURL := fmt.Sprintf("http://%s:%d", ctxData.IP, ctxData.RaftPort)

					rn.transport.AddPeer(types.ID(cc.NodeID), []string{raftURL})
					fmt.Printf("Adding peer from conf change: %x\n", cc.NodeID)
					rn.peerMap[cc.NodeID] = ctxData
				}
			case raftpb.ConfChangeRemoveNode:
				if cc.NodeID == uint64(rn.id) {
					fmt.Println("I have been removed!")
					return false
				}
				rn.transport.RemovePeer(types.ID(cc.NodeID))
				delete(rn.peerMap, cc.NodeID)
			}

		}
		rn.observe(entry)
	}
	return true
}

func (rn *Node) Propose(data []byte) error {
	return rn.node.Propose(context.TODO(), data)
}

func (rn *Node) Process(ctx context.Context, m raftpb.Message) error {
	return rn.node.Step(ctx, m)
}
func (rn *Node) IsIDRemoved(id uint64) bool {
	return false
}
func (rn *Node) ReportUnreachable(id uint64)                          {}
func (rn *Node) ReportSnapshot(id uint64, status raft.SnapshotStatus) {}
