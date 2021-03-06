// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"bytes"
	"errors"
	"github.com/PlatONnetwork/PlatON-Go/consensus/cbft"
	"github.com/PlatONnetwork/PlatON-Go/p2p/discover"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PlatONnetwork/PlatON-Go/common"
	"github.com/PlatONnetwork/PlatON-Go/consensus"
	"github.com/PlatONnetwork/PlatON-Go/consensus/misc"
	"github.com/PlatONnetwork/PlatON-Go/core"
	"github.com/PlatONnetwork/PlatON-Go/core/cbfttypes"
	"github.com/PlatONnetwork/PlatON-Go/core/state"
	"github.com/PlatONnetwork/PlatON-Go/core/types"
	"github.com/PlatONnetwork/PlatON-Go/core/vm"
	"github.com/PlatONnetwork/PlatON-Go/event"
	"github.com/PlatONnetwork/PlatON-Go/log"
	"github.com/PlatONnetwork/PlatON-Go/params"
	"github.com/deckarep/golang-set"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// miningLogAtDepth is the number of confirmations before logging successful mining.
	miningLogAtDepth = 7

	// minRecommitInterval is the minimal time interval to recreate the mining block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// maxRecommitInterval is the maximum time interval to recreate the mining block with
	// any newly arrived transactions.
	maxRecommitInterval = 15 * time.Second

	// intervalAdjustRatio is the impact a single interval adjustment has on sealing work
	// resubmitting interval.
	intervalAdjustRatio = 0.1

	// intervalAdjustBias is applied during the new resubmit interval calculation in favor of
	// increasing upper limit or decreasing lower limit so that the limit can be reachable.
	intervalAdjustBias = 200 * 1000.0 * 1000.0

	// staleThreshold is the maximum depth of the acceptable stale block.
	staleThreshold = 7

	defaultCommitRatio = 0.95
)

type addConsensusPeerFn func(nodes []*discover.Node) error

// environment is the worker's current environment and holds all of the current state information.
type environment struct {
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors mapset.Set     // ancestor set (used for checking uncle parent validity)
	family    mapset.Set     // family set (used for checking uncle invalidity)
	uncles    mapset.Set     // uncle set
	tcount    int            // tx count in cycle
	gasPool   *core.GasPool  // available gas used to pack transactions

	header         *types.Header
	txs            []*types.Transaction
	receipts       []*types.Receipt
	consensusNodes []discover.NodeID
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts       []*types.Receipt
	state          *state.StateDB
	block          *types.Block
	createdAt      time.Time
	consensusNodes []discover.NodeID
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
)

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interrupt   *int32
	noempty     bool
	timestamp   int64
	commitBlock *types.Block
}

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

type commitWorkEnv struct {
	baseLock            sync.RWMutex
	commitBaseBlock     *types.Block
	commitTime          int64
	highestLock         sync.RWMutex
	highestLogicalBlock *types.Block
}

func (e *commitWorkEnv) getHighestLogicalBlock() *types.Block {
	e.highestLock.RLock()
	defer e.highestLock.RUnlock()
	return e.highestLogicalBlock
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	config *params.ChainConfig
	engine consensus.Engine
	eth    Backend
	chain  *core.BlockChain

	gasFloor uint64
	gasCeil  uint64

	// Subscriptions
	mux          *event.TypeMux
	txsCh        chan core.NewTxsEvent
	txsSub       event.Subscription
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription
	chainSideCh  chan core.ChainSideEvent
	chainSideSub event.Subscription

	// Channels
	newWorkCh             chan *newWorkReq
	taskCh                chan *task
	resultCh              chan *types.Block
	prepareResultCh       chan *types.Block
	blockSignatureCh      chan *cbfttypes.BlockSignature // signature
	cbftResultCh          chan *cbfttypes.CbftResult     // Channel output after Seal is released
	highestLogicalBlockCh chan *types.Block
	startCh               chan struct{}
	exitCh                chan struct{}
	resubmitIntervalCh    chan time.Duration
	resubmitAdjustCh      chan *intervalAdjust

	current      *environment                 // An environment for current running cycle.
	localUncles  map[common.Hash]*types.Block // A set of side blocks generated locally as the possible uncle blocks.
	remoteUncles map[common.Hash]*types.Block // A set of side blocks as the possible uncle blocks.
	unconfirmed  *unconfirmedBlocks           // A set of locally mined blocks pending canonicalness confirmations.

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	snapshotMu    sync.RWMutex // The lock used to protect the block snapshot and state snapshot
	snapshotBlock *types.Block
	snapshotState *state.StateDB

	// atomic status counters
	running int32 // The indicator whether the consensus engine is running or not.
	newTxs  int32 // New arrival transaction count since last sealing work submitting.

	// External functions
	isLocalBlock func(block *types.Block) bool // Function used to determine whether the specified block is mined by local miner.

	blockChainCache    *core.BlockChainCache
	commitWorkEnv      *commitWorkEnv
	recommit           time.Duration
	commitDuration     int64 //in Millisecond
	addConsensusPeerFn addConsensusPeerFn

	// Test hooks
	newTaskHook  func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook func()                             // Method to call before pushing the full sealing task.
	resubmitHook func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.
}

func newWorker(config *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, recommit time.Duration, gasFloor, gasCeil uint64, isLocalBlock func(*types.Block) bool,
	blockSignatureCh chan *cbfttypes.BlockSignature, cbftResultCh chan *cbfttypes.CbftResult, highestLogicalBlockCh chan *types.Block, blockChainCache *core.BlockChainCache) *worker {
	worker := &worker{
		config:                config,
		engine:                engine,
		eth:                   eth,
		mux:                   mux,
		chain:                 eth.BlockChain(),
		gasFloor:              gasFloor,
		gasCeil:               gasCeil,
		isLocalBlock:          isLocalBlock,
		localUncles:           make(map[common.Hash]*types.Block),
		remoteUncles:          make(map[common.Hash]*types.Block),
		unconfirmed:           newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth),
		pendingTasks:          make(map[common.Hash]*task),
		txsCh:                 make(chan core.NewTxsEvent, txChanSize),
		chainHeadCh:           make(chan core.ChainHeadEvent, chainHeadChanSize),
		chainSideCh:           make(chan core.ChainSideEvent, chainSideChanSize),
		newWorkCh:             make(chan *newWorkReq),
		taskCh:                make(chan *task),
		resultCh:              make(chan *types.Block, resultQueueSize),
		prepareResultCh:       make(chan *types.Block, resultQueueSize),
		exitCh:                make(chan struct{}),
		startCh:               make(chan struct{}, 1),
		resubmitIntervalCh:    make(chan time.Duration),
		resubmitAdjustCh:      make(chan *intervalAdjust, resubmitAdjustChanSize),
		blockSignatureCh:      blockSignatureCh,
		cbftResultCh:          cbftResultCh,
		highestLogicalBlockCh: highestLogicalBlockCh,
		blockChainCache:       blockChainCache,
		commitWorkEnv:         &commitWorkEnv{},
	}
	// Subscribe NewTxsEvent for tx pool
	// worker.txsSub = eth.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh)

	// Sanitize recommit interval if the user-specified one is too short.
	if config.Cbft != nil {
		recommit = time.Duration(config.Cbft.Period) * time.Second
	}
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	worker.recommit = recommit
	worker.commitDuration = int64((float64)(recommit.Nanoseconds()/1e6) * defaultCommitRatio)
	log.Info("commitDuration in Millisecond", "commitDuration", worker.commitDuration)

	go worker.mainLoop()
	go worker.newWorkLoop(recommit)
	go worker.resultLoop()
	go worker.taskLoop()

	// Submit first work to initialize pending state.
	worker.startCh <- struct{}{}

	return worker
}

func (w *worker) InitConsensusPeerFn(addFn addConsensusPeerFn) {
	w.addConsensusPeerFn = addFn
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	w.resubmitIntervalCh <- interval
}

// pending returns the pending state and corresponding block.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	// return a snapshot to avoid contention on currentMu mutex
	if _, ok := w.engine.(consensus.Bft); ok {
		return w.makePending()
	} else {
		w.snapshotMu.RLock()
		defer w.snapshotMu.RUnlock()
		if w.snapshotState == nil {
			return nil, nil
		}
		return w.snapshotBlock, w.snapshotState.Copy()
	}
}

// pendingBlock returns pending block.
func (w *worker) pendingBlock() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	if _, ok := w.engine.(consensus.Bft); ok {
		pendingBlock, _ := w.makePending()
		return pendingBlock
	} else {
		w.snapshotMu.RLock()
		defer w.snapshotMu.RUnlock()
		return w.snapshotBlock
	}
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	atomic.StoreInt32(&w.running, 1)
	w.startCh <- struct{}{}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	atomic.StoreInt32(&w.running, 0)
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	close(w.exitCh)
}

// newWorkLoop is a standalone goroutine to submit new mining work upon received events.
func (w *worker) newWorkLoop(recommit time.Duration) {
	var (
		interrupt   *int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of mining in Millisecond.
	)

	timer := time.NewTimer(0)
	<-timer.C // discard the initial tick

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	commit := func(noempty bool, s int32, baseBlock *types.Block) {
		if interrupt != nil {
			atomic.StoreInt32(interrupt, s)
		}
		interrupt = new(int32)
		w.newWorkCh <- &newWorkReq{interrupt: interrupt, noempty: noempty, timestamp: timestamp, commitBlock: baseBlock}
		timer.Reset(recommit)
		atomic.StoreInt32(&w.newTxs, 0)
	}
	// recalcRecommit recalculates the resubmitting interval upon feedback.
	recalcRecommit := func(target float64, inc bool) {
		var (
			prev = float64(recommit.Nanoseconds())
			next float64
		)
		if inc {
			next = prev*(1-intervalAdjustRatio) + intervalAdjustRatio*(target+intervalAdjustBias)
			// Recap if interval is larger than the maximum time interval
			if next > float64(maxRecommitInterval.Nanoseconds()) {
				next = float64(maxRecommitInterval.Nanoseconds())
			}
		} else {
			next = prev*(1-intervalAdjustRatio) + intervalAdjustRatio*(target-intervalAdjustBias)
			// Recap if interval is less than the user specified minimum
			if next < float64(minRecommit.Nanoseconds()) {
				next = float64(minRecommit.Nanoseconds())
			}
		}
		recommit = time.Duration(int64(next))
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			clearPending(w.chain.CurrentBlock().NumberU64())
			timestamp = time.Now().UnixNano() / 1e6
			if _, ok := w.engine.(consensus.Bft); !ok {
				commit(false, commitInterruptNewHead, nil)
			} else {
				//w.makePending()
				timer.Reset(100 * time.Millisecond)
			}

		case head := <-w.chainHeadCh:
			clearPending(head.Block.NumberU64())
			timestamp = time.Now().UnixNano() / 1e6
			//commit(false, commitInterruptNewHead)
			// clear consensus cache
			log.Debug("received a event of ChainHeadEvent", "hash", head.Block.Hash().Hex(), "number", head.Block.NumberU64(), "parentHash", head.Block.ParentHash().Hex())
			w.blockChainCache.ClearCache(head.Block)

			if cbft, ok := w.engine.(consensus.Bft); ok {
				cbft.OnBlockSynced()
			}

		case highestLogicalBlock := <-w.highestLogicalBlockCh:
			log.Debug("received a notify for new highest logical", "number", highestLogicalBlock.NumberU64(), "hash", highestLogicalBlock.Hash().Hex())
			w.commitWorkEnv.highestLock.Lock()
			w.commitWorkEnv.highestLogicalBlock = highestLogicalBlock
			w.commitWorkEnv.highestLock.Unlock()

			if w.isRunning() {
				if shouldCommit, commitBlock := w.shouldCommit(time.Now().UnixNano() / 1e6); shouldCommit {
					log.Debug("--------------HighestLogicalBlock change,and the commitNewWork task is not executed at intervals of " + recommit.String() + ",Packing Start--------------")
					timestamp = time.Now().UnixNano() / 1e6
					commit(false, commitInterruptResubmit, commitBlock)
				}
			}

		case <-timer.C:
			// If mining is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if w.isRunning() {
				log.Debug("Interval " + recommit.String() + ",attempt to commitNewWork")
				if _, ok := w.engine.(consensus.Bft); ok {
					if shouldCommit, commitBlock := w.shouldCommit(time.Now().UnixNano() / 1e6); shouldCommit {
						log.Debug("node inTurn,Packing Start", "baseBlockNumber", commitBlock.NumberU64(), "baseBlockHash", commitBlock.Hash())
						timestamp = time.Now().UnixNano() / 1e6
						commit(false, commitInterruptResubmit, commitBlock)
						continue
					}
					//timer.Reset(recommit)
					timer.Reset(100 * time.Millisecond)
				} else if w.config.Clique == nil || w.config.Clique.Period > 0 {
					// Short circuit if no new transaction arrives.
					if atomic.LoadInt32(&w.newTxs) == 0 {
						timer.Reset(recommit)
						continue
					}
					timestamp = time.Now().UnixNano() / 1e6
					commit(true, commitInterruptResubmit, nil)
				}
			}

		case interval := <-w.resubmitIntervalCh:
			if _, ok := w.engine.(consensus.Bft); !ok {
				// Adjust resubmit interval explicitly by user.
				if interval < minRecommitInterval {
					log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
					interval = minRecommitInterval
				}
				log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
				minRecommit, recommit = interval, interval

				if w.resubmitHook != nil {
					w.resubmitHook(minRecommit, recommit)
				}
			}

		case adjust := <-w.resubmitAdjustCh:
			if _, ok := w.engine.(consensus.Bft); !ok {
				// Adjust resubmit interval by feedback.
				if adjust.inc {
					before := recommit
					recalcRecommit(float64(recommit.Nanoseconds())/adjust.ratio, true)
					log.Trace("Increase miner recommit interval", "from", before, "to", recommit)
				} else {
					before := recommit
					recalcRecommit(float64(minRecommit.Nanoseconds()), false)
					log.Trace("Decrease miner recommit interval", "from", before, "to", recommit)
				}

				if w.resubmitHook != nil {
					w.resubmitHook(minRecommit, recommit)
				}
			}

		case <-w.exitCh:
			return
		}
	}
}

// mainLoop is a standalone goroutine to regenerate the sealing task based on the received event.
func (w *worker) mainLoop() {
	// defer w.txsSub.Unsubscribe()
	defer w.chainHeadSub.Unsubscribe()
	defer w.chainSideSub.Unsubscribe()

	for {
		select {
		case req := <-w.newWorkCh:
			w.commitNewWork(req.interrupt, req.noempty, req.timestamp, req.commitBlock)

		case ev := <-w.chainSideCh:
			// Short circuit for duplicate side blocks
			if _, exist := w.localUncles[ev.Block.Hash()]; exist {
				continue
			}
			if _, exist := w.remoteUncles[ev.Block.Hash()]; exist {
				continue
			}
			// Add side block to possible uncle block set depending on the author.
			if w.isLocalBlock != nil && w.isLocalBlock(ev.Block) {
				w.localUncles[ev.Block.Hash()] = ev.Block
			} else {
				w.remoteUncles[ev.Block.Hash()] = ev.Block
			}
			// If our mining block contains less than 2 uncle blocks,
			// add the new uncle block if valid and regenerate a mining block.
			if w.isRunning() && w.current != nil && w.current.uncles.Cardinality() < 2 {
				start := time.Now()
				if err := w.commitUncle(w.current, ev.Block.Header()); err == nil {
					var uncles []*types.Header
					w.current.uncles.Each(func(item interface{}) bool {
						hash, ok := item.(common.Hash)
						if !ok {
							return false
						}
						uncle, exist := w.localUncles[hash]
						if !exist {
							uncle, exist = w.remoteUncles[hash]
						}
						if !exist {
							return false
						}
						uncles = append(uncles, uncle.Header())
						return false
					})
					w.commit(uncles, nil, true, start, nil)
				}
			}
		// removed by PlatON
		/*
			case  <-w.txsCh:

				// Apply transactions to the pending state if we're not mining.
				// Note all transactions received may not be continuous with transactions
				// already included in the current mining block. These transactions will
				// be automatically eliminated.
				if !w.isRunning() && w.current != nil {
					w.mu.RLock()
					coinbase := w.coinbase
					w.mu.RUnlock()

					txs := make(map[common.Address]types.Transactions)
					for _, tx := range ev.Txs {
						acc, _ := types.Sender(w.current.signer, tx)
						txs[acc] = append(txs[acc], tx)
					}
					txset := types.NewTransactionsByPriceAndNonce(w.current.signer, txs)
					w.commitTransactions(txset, coinbase, nil, 0)
					w.updateSnapshot()
				} else {
					// If we're mining, but nothing is being processed, wake on new transactions
					if w.config.Clique != nil && w.config.Clique.Period == 0 {
						w.commitNewWork(nil, false, time.Now().Unix(), nil)
					}
				}
				atomic.AddInt32(&w.newTxs, int32(len(ev.Txs)))
		*/

		// System stopped
		case <-w.exitCh:
			return

			/*
				case <-w.txsSub.Err():
					return
			*/

		case <-w.chainHeadSub.Err():
			return
		case <-w.chainSideSub.Err():
			return

		case block := <-w.prepareResultCh:
			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}

			// Broadcast the block and announce chain insertion event
			log.Debug("Post PrepareMinedBlockEvent", "blockNumber", block.NumberU64(), "blockHash", block.Hash(), "consensusNodes", task.consensusNodes)
			w.mux.Post(core.PrepareMinedBlockEvent{Block: block, ConsensusNodes: task.consensusNodes})

		case blockSignature := <-w.blockSignatureCh:
			log.Debug("to receive blockSign from cbft", "blockSignature", blockSignature)
			if blockSignature != nil {
				// send blockSignatureMsg to consensus node peer
				blockNumber := blockSignature.Number
				parentNumber := new(big.Int).Sub(blockNumber, common.Big1)
				consensusNodes := w.engine.(consensus.Bft).ConsensusNodes(parentNumber, blockSignature.ParentHash, blockNumber)
				if consensusNodes != nil {
					log.Debug("Post BlockSignatureEvent", "consensusNodes", consensusNodes)
					w.mux.Post(core.BlockSignatureEvent{BlockSignature: blockSignature, ConsensusNodes: consensusNodes})
				}
			}
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	var (
		stopCh chan struct{}
		prev   common.Hash
	)

	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				continue
			}
			// Interrupt previous sealing operation
			interrupt()
			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				continue
			}
			w.pendingMu.Lock()
			w.pendingTasks[sealHash] = task
			w.pendingMu.Unlock()

			if cbftEngine, ok := w.engine.(consensus.Bft); ok {
				// Save stateDB, receipts to blockChainCache
				w.blockChainCache.WriteStateDB(sealHash, task.state, task.block.NumberU64())
				w.blockChainCache.WriteReceipts(sealHash, task.receipts, task.block.NumberU64())
				//w.blockChainCache.MarkBlockHash(task.block.Hash())
				if err := cbftEngine.Seal(w.chain, task.block, w.prepareResultCh, stopCh); err != nil {
					log.Warn("【Bft engine】Block sealing failed", "err", err)
				}
				continue
			}

			if err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
				log.Warn("Block sealing failed", "err", err)
			}
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	for {
		select {
		case block := <-w.resultCh:
			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}
			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				receipts = make([]*types.Receipt, len(task.receipts))
				logs     []*types.Log
			)
			for i, receipt := range task.receipts {
				receipts[i] = new(types.Receipt)
				*receipts[i] = *receipt
				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				for _, log := range receipt.Logs {
					log.BlockHash = hash
				}
				logs = append(logs, receipt.Logs...)
			}
			// Commit block and state to database.
			stat, err := w.chain.WriteBlockWithState(block, receipts, task.state)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
				"elapsed", common.PrettyDuration(time.Since(task.createdAt)))

			// Broadcast the block and announce chain insertion event
			w.mux.Post(core.NewMinedBlockEvent{Block: block})

			var events []interface{}
			switch stat {
			case core.CanonStatTy:
				log.Debug("Prepare Events, WriteStatus=CanonStatTy")
				events = append(events, core.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
				events = append(events, core.ChainHeadEvent{Block: block})
			case core.SideStatTy:
				log.Debug("Prepare Events, WriteStatus=SideStatTy")
				events = append(events, core.ChainSideEvent{Block: block})
			}
			w.chain.PostChainEvents(events, logs)

			// Insert the block into the set of pending ones to resultLoop for confirmations
			w.unconfirmed.Insert(block.NumberU64(), block.Hash())

		case cbftResult := <-w.cbftResultCh:
			block := cbftResult.Block
			blockConfirmSigns := cbftResult.BlockConfirmSigns
			// Short circuit when receiving empty result.
			if block == nil {
				log.Error("cbft result error, block is nil")
				continue
			}

			if blockConfirmSigns == nil || len(blockConfirmSigns) == 0 {
				log.Error("cbft result error, blockConfirmSigns is nil")
				continue
			}
			var (
				hash     = block.Hash()
				sealhash = w.engine.SealHash(block.Header())
				number   = block.NumberU64()
			)
			// Short circuit when receiving duplicated cbft result caused by resubmitting or P2P sync.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				log.Warn("duplicated cbft result caused by resubmitting or P2P sync.", "hash", hash, "number", number)
				continue
			}

			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			var _receipts []*types.Receipt
			var _state *state.StateDB
			if exist && cbft.IsSignedBySelf(sealhash, block.Extra()[32:]) {
				_receipts = task.receipts
				_state = task.state
				stateIsNil := _state == nil
				log.Debug("block is packaged by local", "hash", hash, "number", number, "len(Receipts)", len(_receipts), "stateIsNil", stateIsNil)
			} else {
				_receipts = w.blockChainCache.ReadReceipts(sealhash)
				_state = w.blockChainCache.ReadStateDB(sealhash)
				stateIsNil := _state == nil
				log.Debug("block is packaged by other", "hash", hash, "number", number, "len(Receipts)", len(_receipts), "blockRoot", block.Root(), "stateIsNil", stateIsNil)
			}

			if _state == nil {
				log.Warn("handle cbft result error, state is nil, maybe block is synced from other peer", "hash", hash, "number", number)
				continue
			} else if len(block.Transactions()) > 0 && len(_receipts) == 0 {
				log.Warn("handle cbft result error, block has transactions but receipts is nil, maybe block is synced from other peer", "hash", hash, "number", number)
				continue
			}

			log.Debug("cbft consensus successful", "hash", hash, "number", number, "timestamp", time.Now().UnixNano()/1e6)

			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				receipts = make([]*types.Receipt, len(_receipts))
				logs     []*types.Log
			)
			for i, receipt := range _receipts {
				receipts[i] = new(types.Receipt)
				*receipts[i] = *receipt
				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				for _, log := range receipt.Logs {
					log.BlockHash = hash
				}
				logs = append(logs, receipt.Logs...)
			}
			// Commit block and state to database.
			block.ConfirmSigns = blockConfirmSigns
			stat, err := w.chain.WriteBlockWithState(block, receipts, _state)
			if err != nil {
				log.Error("Failed writing block to chain", "hash", block.Hash(), "number", block.NumberU64(), "err", err)
				continue
			}
			log.Info("Successfully write new block", "hash", block.Hash(), "number", block.NumberU64())

			// Broadcast the block and announce chain insertion event
			w.mux.Post(core.NewMinedBlockEvent{Block: block})

			w.attemptAddConsensusPeer(block.Number(), _state)

			var events []interface{}
			switch stat {
			case core.CanonStatTy:
				events = append(events, core.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
				events = append(events, core.ChainHeadEvent{Block: block})
			case core.SideStatTy:
				events = append(events, core.ChainSideEvent{Block: block})
			}
			w.chain.PostChainEvents(events, logs)

		case <-w.exitCh:
			return
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
func (w *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	var (
		state *state.StateDB
		err   error
	)
	if _, ok := w.engine.(consensus.Bft); ok {
		log.Debug("【makeCurrent】New state by MakeStateDB ...")
		state, err = w.blockChainCache.MakeStateDB(parent)
	} else {
		log.Debug("【makeCurrent】New state by chain StateAt ...")
		state, err = w.chain.StateAt(parent.Root(), parent.Number(), parent.Hash())
	}
	log.Info("-----------Build statedb---------", "blockNumber", header.Number.Uint64(), "parentNumber", parent.NumberU64(), "parentStateRoot", parent.Root())
	if err != nil {
		return err
	}

	env := &environment{
		signer:    types.NewEIP155Signer(w.config.ChainID),
		state:     state,
		ancestors: mapset.NewSet(),
		family:    mapset.NewSet(),
		uncles:    mapset.NewSet(),
		header:    header,
	}
	if cbftEngine, ok := w.engine.(consensus.Bft); ok {
		nodes := cbftEngine.ConsensusNodes(parent.Number(), parent.Hash(), header.Number)
		if nodes == nil || len(nodes) <= 0 {
			return errors.New("Failed to load consensus nodes")
		}
		env.consensusNodes = nodes
	}

	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range w.chain.GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			env.family.Add(uncle.Hash())
		}
		env.family.Add(ancestor.Hash())
		env.ancestors.Add(ancestor.Hash())
	}

	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0
	w.current = env
	return nil
}

// commitUncle adds the given block to uncle block set, returns error if failed to add.
func (w *worker) commitUncle(env *environment, uncle *types.Header) error {
	hash := uncle.Hash()
	if env.uncles.Contains(hash) {
		return errors.New("uncle not unique")
	}
	if env.header.ParentHash == uncle.ParentHash {
		return errors.New("uncle is sibling")
	}
	if !env.ancestors.Contains(uncle.ParentHash) {
		return errors.New("uncle's parent unknown")
	}
	if env.family.Contains(hash) {
		return errors.New("uncle already included")
	}
	env.uncles.Add(uncle.Hash())
	return nil
}

// updateSnapshot updates pending snapshot block and state.
// Note this function assumes the current variable is thread safe.
func (w *worker) updateSnapshot() {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	var uncles []*types.Header
	w.current.uncles.Each(func(item interface{}) bool {
		hash, ok := item.(common.Hash)
		if !ok {
			return false
		}
		uncle, exist := w.localUncles[hash]
		if !exist {
			uncle, exist = w.remoteUncles[hash]
		}
		if !exist {
			return false
		}
		uncles = append(uncles, uncle.Header())
		return false
	})

	w.snapshotBlock = types.NewBlock(
		w.current.header,
		w.current.txs,
		uncles,
		w.current.receipts,
	)

	w.snapshotState = w.current.state.Copy()
}

func (w *worker) commitTransaction(tx *types.Transaction, coinbase common.Address) ([]*types.Log, error) {
	snap := w.current.state.Snapshot()

	receipt, _, err := core.ApplyTransaction(w.config, w.chain, &coinbase, w.current.gasPool, w.current.state, w.current.header, tx, &w.current.header.GasUsed, vm.Config{})
	if err != nil {
		w.current.state.RevertToSnapshot(snap)
		return nil, err
	}
	w.current.txs = append(w.current.txs, tx)
	w.current.receipts = append(w.current.receipts, receipt)

	return receipt.Logs, nil
}
func (w *worker) commitTransactionsWithHeader(header *types.Header, txs *types.TransactionsByPriceAndNonce, coinbase common.Address, interrupt *int32, timestamp int64) (bool, bool) {
	// Short circuit if current is nil
	timeout := false

	if w.current == nil {
		return true, timeout
	}

	if w.current.gasPool == nil {
		w.current.gasPool = new(core.GasPool).AddGas(w.current.header.GasLimit)
	}

	var coalescedLogs []*types.Log
	var bftEngine = w.config.Cbft != nil

	for {
		if bftEngine && (time.Now().UnixNano()/1e6-timestamp >= w.commitDuration) {
			log.Warn("interrupt current tx-executing", "now", time.Now().UnixNano()/1e6, "timestamp", timestamp, "commitDuration", w.commitDuration)
			//log.Warn("interrupt current tx-executing cause timeout, and continue the remainder package process", "timeout", w.commitDuration, "txCount", w.current.tcount)
			timeout = true
			break
		}
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the mining block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(w.current.header.GasLimit-w.current.gasPool.Gas()) / float64(w.current.header.GasLimit)
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead, timeout
		}
		// If we don't have enough gas for any further transactions then we're done
		if w.current.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", w.current.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(w.current.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.config.IsEIP155(w.current.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.config.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		w.current.state.Prepare(tx.Hash(), common.Hash{}, w.current.tcount)

		logs, err := w.commitTransaction(tx, coinbase)

		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Warn("Gas limit exceeded for current block", "blockNumber", header.Number, "blockParentHash", header.ParentHash, "tx.hash", tx.Hash(), "sender", from, w.current.state)
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Warn("Skipping transaction with low nonce", "blockNumber", header.Number, "blockParentHash", header.ParentHash, "tx.hash", tx.Hash(), "sender", from, "senderCurNonce", w.current.state.GetNonce(from), "tx.nonce", tx.Nonce())
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Warn("Skipping account with hight nonce", "blockNumber", header.Number, "blockParentHash", header.ParentHash, "tx.hash", tx.Hash(), "sender", from, "senderCurNonce", w.current.state.GetNonce(from), "tx.nonce", tx.Nonce())
			txs.Pop()

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			w.current.tcount++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Warn("Transaction failed, account skipped", "blockNumber", header.Number, "blockParentHash", header.ParentHash, "hash", tx.Hash(), "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are mining. The reason is that
		// when we are mining, the worker will regenerate a mining block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		go w.mux.Post(core.PendingLogsEvent{Logs: cpy})
	}
	// Notify resubmit loop to decrease resubmitting interval if current interval is larger
	// than the user-specified one.
	if interrupt != nil {
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	}
	return false, timeout
}

func (w *worker) commitTransactions(txs *types.TransactionsByPriceAndNonce, coinbase common.Address, interrupt *int32, timestamp int64) bool {
	// Short circuit if current is nil
	if w.current == nil {
		return true
	}

	if w.current.gasPool == nil {
		w.current.gasPool = new(core.GasPool).AddGas(w.current.header.GasLimit)
	}

	var coalescedLogs []*types.Log
	var bftEngine = w.config.Cbft != nil

	for {
		if bftEngine && (time.Now().UnixNano()/1e6-timestamp >= w.commitDuration) {
			log.Warn("interrupt current tx-executing cause timeout, and continue the remainder package process", "timeout", w.commitDuration, "txCount", w.current.tcount)
			break
		}
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the mining block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(w.current.header.GasLimit-w.current.gasPool.Gas()) / float64(w.current.header.GasLimit)
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead
		}
		// If we don't have enough gas for any further transactions then we're done
		if w.current.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", w.current.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(w.current.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.config.IsEIP155(w.current.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.config.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		w.current.state.Prepare(tx.Hash(), common.Hash{}, w.current.tcount)
		log.Debug("commit transaction", "hash", tx.Hash(), "sender", from, "nonce", tx.Nonce())

		logs, err := w.commitTransaction(tx, coinbase)
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Warn("Gas limit exceeded for current block", "hash", tx.Hash(), "sender", from, w.current.state)
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Warn("Skipping transaction with low nonce", "hash", tx.Hash(), "sender", from, "senderCurNonce", w.current.state.GetNonce(from), "txNonce", tx.Nonce())
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Warn("Skipping account with hight nonce", "hash", tx.Hash(), "sender", from, "senderCurNonce", w.current.state.GetNonce(from), "txNonce", tx.Nonce())
			txs.Pop()

		case nil:
			log.Debug("commit transaction success", "hash", tx.Hash(), "sender", from, "senderCurNonce", w.current.state.GetNonce(from), "txNonce", tx.Nonce())

			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			w.current.tcount++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are mining. The reason is that
		// when we are mining, the worker will regenerate a mining block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		go w.mux.Post(core.PendingLogsEvent{Logs: cpy})
	}
	// Notify resubmit loop to decrease resubmitting interval if current interval is larger
	// than the user-specified one.
	if interrupt != nil {
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	}
	return false
}

// commitNewWork generates several new sealing tasks based on the parent block.
func (w *worker) commitNewWork(interrupt *int32, noempty bool, timestamp int64, commitBlock *types.Block) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	tstart := time.Now()

	var parent *types.Block
	if _, ok := w.engine.(consensus.Bft); ok {
		parent = commitBlock
		//timestamp = time.Now().UnixNano() / 1e6
	} else {
		parent = w.chain.CurrentBlock()
		if parent.Time().Cmp(new(big.Int).SetInt64(timestamp)) >= 0 {
			timestamp = parent.Time().Int64() + 1
		}
		// this will ensure we're not going off too far in the future
		if now := time.Now().Unix(); timestamp > now+1 {
			wait := time.Duration(timestamp-now) * time.Second
			log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
			time.Sleep(wait)
		}
	}

	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, w.gasFloor, w.gasCeil),
		Extra:      w.extra,
		Time:       big.NewInt(timestamp),
	}
	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	if w.isRunning() {
		if w.coinbase == (common.Address{}) {
			log.Error("Refusing to mine without etherbase")
			return
		}
		header.Coinbase = w.coinbase
	}

	log.Debug("cbft begin to consensus for new block", "number", header.Number, "gasLimit", header.GasLimit, "parentHash", parent.Hash(), "parentNumber", parent.NumberU64(), "parentStateRoot", parent.Root(), "timestamp", time.Now().UnixNano()/1e6)
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		return
	}
	// If we are care about TheDAO hard-fork check whether to override the extra-data or not
	if daoBlock := w.config.DAOForkBlock; daoBlock != nil {
		// Check whether the block is among the fork extra-override range
		limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
		if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
			// Depending whether we support or oppose the fork, override differently
			if w.config.DAOForkSupport {
				header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
			} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
				header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
			}
		}
	}
	// Could potentially happen if starting to mine in an odd state.
	err := w.makeCurrent(parent, header)
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		return
	}
	// Create the current work task and check any fork transitions needed
	env := w.current
	if w.config.DAOForkSupport && w.config.DAOForkBlock != nil && w.config.DAOForkBlock.Cmp(header.Number) == 0 {
		misc.ApplyDAOHardFork(env.state)
	}
	// Accumulate the uncles for the current block
	uncles := make([]*types.Header, 0, 2)
	commitUncles := func(blocks map[common.Hash]*types.Block) {
		// Clean up stale uncle blocks first
		for hash, uncle := range blocks {
			if uncle.NumberU64()+staleThreshold <= header.Number.Uint64() {
				delete(blocks, hash)
			}
		}
		for hash, uncle := range blocks {
			if len(uncles) == 2 {
				break
			}
			if err := w.commitUncle(env, uncle.Header()); err != nil {
				log.Trace("Possible uncle rejected", "hash", hash, "reason", err)
			} else {
				log.Debug("Committing new uncle to block", "hash", hash)
				uncles = append(uncles, uncle.Header())
			}
		}
	}
	// Prefer to locally generated uncle
	commitUncles(w.localUncles)
	commitUncles(w.remoteUncles)

	if !noempty {
		// Create an empty block based on temporary copied state for sealing in advance without waiting block
		// execution finished.
		if _, ok := w.engine.(consensus.Bft); !ok {
			// TODO
			//w.forEachStorage(w.current.state, "【The Consensus packaging】,Before executing the transaction")
			w.commit(uncles, nil, false, tstart, nil)
		}
	}

	// Fill the block with all available pending transactions.
	startTime := time.Now()
	pending, err := w.eth.TxPool().PendingLimited()

	if err != nil {
		log.Error("Failed to fetch pending transactions", "time", common.PrettyDuration(time.Since(startTime)), "err", err)
		return
	}

	log.Debug("Fetch pending transactions success", "pendingLength", len(pending), "time", common.PrettyDuration(time.Since(startTime)))

	// Short circuit if there is no available pending transactions
	if len(pending) == 0 {
		if _, ok := w.engine.(consensus.Bft); ok {
			w.commit(uncles, nil, true, tstart, header)
		} else {
			w.updateSnapshot()
		}
		return
	}

	txsCount := 0
	for _, accTxs := range pending {
		txsCount = txsCount + len(accTxs)
	}
	// Split the pending transactions into locals and remotes
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), pending
	for _, account := range w.eth.TxPool().Locals() {
		if txs := remoteTxs[account]; len(txs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = txs
		}
	}

	startTime = time.Now()
	var localTimeout = false
	if len(localTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(w.current.signer, localTxs)
		if ok, timeout := w.commitTransactionsWithHeader(header, txs, w.coinbase, interrupt, timestamp); ok {
			return
		} else {
			localTimeout = timeout
		}
	}

	commitLocalTxCount := w.current.tcount
	log.Debug("local transactions executing stat", "hash", commitBlock.Hash(), "number", commitBlock.NumberU64(), "involvedTxCount", commitLocalTxCount, "time", common.PrettyDuration(time.Since(startTime)))

	startTime = time.Now()
	if !localTimeout && len(remoteTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(w.current.signer, remoteTxs)
		if ok, _ := w.commitTransactionsWithHeader(header, txs, w.coinbase, interrupt, timestamp); ok {
			return
		}
	}
	commitRemoteTxCount := w.current.tcount - commitLocalTxCount
	log.Debug("remote transactions executing stat", "hash", commitBlock.Hash(), "number", commitBlock.NumberU64(), "involvedTxCount", commitRemoteTxCount, "time", common.PrettyDuration(time.Since(startTime)))

	w.commit(uncles, w.fullTaskHook, true, tstart, header)
}

// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
func (w *worker) commit(uncles []*types.Header, interval func(), update bool, start time.Time, header *types.Header) error {
	// Deep copy receipts here to avoid interaction between different tasks.
	receipts := make([]*types.Receipt, len(w.current.receipts))
	for i, l := range w.current.receipts {
		receipts[i] = new(types.Receipt)
		*receipts[i] = *l
	}

	s := w.current.state.Copy()
	if header != nil {
		if err := w.notify(s, header.Number); err != nil {
			return errors.New("notify failure")
		}
		// Election call(if match condition)
		if electionErr := w.election(s, header.ParentHash, header.Number); electionErr != nil {
			return errors.New("election failure")
		}
		// SwitchWitness call(if match condition)
		if switchWitnessErr := w.switchWitness(s, header.Number); switchWitnessErr != nil {
			return errors.New("switchWitness failure")
		}
		// ppos Store Hash
		w.storeHash(s)
	}
	block, err := w.engine.Finalize(w.chain, w.current.header, s, w.current.txs, uncles, w.current.receipts)

	if err != nil {
		return err
	}
	if w.isRunning() {
		if interval != nil {
			interval()
		}
		select {
		case w.taskCh <- &task{receipts: receipts, state: s, block: block, createdAt: time.Now(), consensusNodes: w.current.consensusNodes}:
			w.unconfirmed.Shift(block.NumberU64() - 1)

			feesWei := new(big.Int)
			for i, tx := range block.Transactions() {
				feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), tx.GasPrice()))
			}
			feesEth := new(big.Float).Quo(new(big.Float).SetInt(feesWei), new(big.Float).SetInt(big.NewInt(params.Ether)))

			log.Debug("Commit new mining work", "number", block.Number(), "sealhash", w.engine.SealHash(block.Header()), "receiptHash", block.ReceiptHash(),
				"uncles", len(uncles), "txs", w.current.tcount, "gas", block.GasUsed(), "fees", feesEth, "elapsed", common.PrettyDuration(time.Since(start)))

		case <-w.exitCh:
			log.Info("Worker has exited")
		}
	}
	if update {
		w.updateSnapshot()
	}
	return nil
}

func (w *worker) makePending() (*types.Block, *state.StateDB) {
	var parent = w.commitWorkEnv.getHighestLogicalBlock()
	var parentChain = w.chain.CurrentBlock()

	if parentChain.NumberU64() >= parent.NumberU64() {
		parent = parentChain
	}
	log.Debug("parent in makePending", "number", parent.NumberU64(), "hash", parent.Hash())

	if parent != nil {
		state, err := w.blockChainCache.MakeStateDB(parent)
		if err == nil {
			block := types.NewBlock(
				parent.Header(),
				parent.Transactions(),
				nil,
				nil,
			)

			return block, state
		}
	}
	return nil, nil
}

func (w *worker) shouldCommit(timestamp int64) (bool, *types.Block) {
	w.commitWorkEnv.baseLock.Lock()
	defer w.commitWorkEnv.baseLock.Unlock()

	baseBlock, commitTime := w.commitWorkEnv.commitBaseBlock, w.commitWorkEnv.commitTime
	highestLogicalBlock := w.commitWorkEnv.getHighestLogicalBlock()
	if baseBlock != nil {
		log.Debug("baseBlock", "number", baseBlock.NumberU64(), "hash", baseBlock.Hash().Hex())
		log.Debug("commitTime", "commitTime", commitTime, "timestamp", timestamp)
		log.Debug("highestLogicalBlock", "number", highestLogicalBlock.NumberU64(), "hash", highestLogicalBlock.Hash().Hex())
	}

	shouldCommit := baseBlock == nil || baseBlock.Hash() != highestLogicalBlock.Hash()
	if shouldCommit && timestamp != 0 {
		shouldCommit = shouldCommit && (timestamp-commitTime >= w.recommit.Nanoseconds()/1e6)
	}

	if shouldCommit {
		shouldSeal := false
		num := highestLogicalBlock.Number()
		shouldSeal = w.engine.(consensus.Bft).ShouldSeal(num, highestLogicalBlock.Hash(), new(big.Int).Add(num, common.Big1))
		shouldCommit = shouldCommit && shouldSeal
	}

	if shouldCommit {
		w.commitWorkEnv.commitBaseBlock = highestLogicalBlock
		w.commitWorkEnv.commitTime = time.Now().UnixNano() / 1e6
	}
	return shouldCommit, highestLogicalBlock
}
