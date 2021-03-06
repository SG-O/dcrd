// Copyright (c) 2013-2014 The btcsuite developers
// Copyright (c) 2015 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"container/list"
	"encoding/gob"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/dcrd/blockchain"
	"github.com/decred/dcrd/blockchain/stake"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/chaincfg/chainhash"
	dcrdb "github.com/decred/dcrd/database"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrutil"
)

const (
	chanBufferSize = 50

	// minInFlightBlocks is the minimum number of blocks that should be
	// in the request queue for headers-first mode before requesting
	// more.
	minInFlightBlocks = 10

	// blockDbNamePrefix is the prefix for the block database name.  The
	// database type is appended to this value to form the full block
	// database name.
	blockDbNamePrefix = "blocks"

	// maxResendLimit is the maximum number of times a node can resend a
	// block or transaction before it is dropped.
	maxResendLimit = 3
)

// newPeerMsg signifies a newly connected peer to the block handler.
type newPeerMsg struct {
	peer *peer
}

// blockMsg packages a decred block message and the peer it came from together
// so the block handler has access to that information.
type blockMsg struct {
	block *dcrutil.Block
	peer  *peer
}

// invMsg packages a decred inv message and the peer it came from together
// so the block handler has access to that information.
type invMsg struct {
	inv  *wire.MsgInv
	peer *peer
}

// headersMsg packages a decred headers message and the peer it came from
// together so the block handler has access to that information.
type headersMsg struct {
	headers *wire.MsgHeaders
	peer    *peer
}

// donePeerMsg signifies a newly disconnected peer to the block handler.
type donePeerMsg struct {
	peer *peer
}

// txMsg packages a decred tx message and the peer it came from together
// so the block handler has access to that information.
type txMsg struct {
	tx   *dcrutil.Tx
	peer *peer
}

// getSyncPeerMsg is a message type to be sent across the message channel for
// retrieving the current sync peer.
type getSyncPeerMsg struct {
	reply chan *peer
}

// requestFromPeerMsg is a message type to be sent across the message channel
// for requesting either blocks or transactions from a given peer. It routes
// this through the block manager so the block manager doesn't ban the peer
// when it sends this information back.
type requestFromPeerMsg struct {
	peer   *peer
	blocks []*chainhash.Hash
	txs    []*chainhash.Hash
	reply  chan requestFromPeerResponse
}

// requestFromPeerRespons eis a response sent to the reply channel of a
// requestFromPeerMsg query.
type requestFromPeerResponse struct {
	err error
}

// checkConnectBlockMsg is a message type to be sent across the message channel
// for requesting chain to check if a block connects to the end of the current
// main chain.
type checkConnectBlockMsg struct {
	block *dcrutil.Block
	reply chan error
}

// calcNextReqDifficultyResponse is a response sent to the reply channel of a
// calcNextReqDifficultyMsg query.
type calcNextReqDifficultyResponse struct {
	difficulty uint32
	err        error
}

// calcNextReqDifficultyMsg is a message type to be sent across the message
// channel for requesting the required difficulty of the next block.
type calcNextReqDifficultyMsg struct {
	timestamp time.Time
	reply     chan calcNextReqDifficultyResponse
}

// calcNextReqDiffNodeResponse is a response sent to the reply channel of a
// calcNextReqDiffNodeMsg query.
type calcNextReqDiffNodeResponse struct {
	difficulty uint32
	err        error
}

// calcNextReqDiffNodeMsg is a message type to be sent across the message
// channel for requesting the required difficulty for some block building on
// the given block hash.
type calcNextReqDiffNodeMsg struct {
	hash      *chainhash.Hash
	timestamp time.Time
	reply     chan calcNextReqDifficultyResponse
}

// calcNextReqStakeDifficultyResponse is a response sent to the reply channel of a
// calcNextReqStakeDifficultyMsg query.
type calcNextReqStakeDifficultyResponse struct {
	stakeDifficulty int64
	err             error
}

// calcNextReqStakeDifficultyMsg is a message type to be sent across the message
// channel for requesting the required stake difficulty of the next block.
type calcNextReqStakeDifficultyMsg struct {
	reply chan calcNextReqStakeDifficultyResponse
}

// getBlockFromHashResponse is a response sent to the reply channel of a
// getBlockFromHashMsg query.
type getBlockFromHashResponse struct {
	block *dcrutil.Block
	err   error
}

// getBlockFromHashMsg is a message type to be sent across the message
// channel for requesting the required a given block from the block manager.
type getBlockFromHashMsg struct {
	hash  chainhash.Hash
	reply chan getBlockFromHashResponse
}

// getGenerationResponse is a response sent to the reply channel of a
// getGenerationMsg query.
type getGenerationResponse struct {
	hashes []chainhash.Hash
	err    error
}

// getGenerationMsg is a message type to be sent across the message
// channel for requesting the required the entire generation of a
// block node.
type getGenerationMsg struct {
	hash  chainhash.Hash
	reply chan getGenerationResponse
}

// forceReorganizationResponse is a response sent to the reply channel of a
// forceReorganizationMsg query.
type forceReorganizationResponse struct {
	err error
}

// forceReorganizationMsg is a message type to be sent across the message
// channel for requesting that the block on head be reorganized to one of its
// adjacent orphans.
type forceReorganizationMsg struct {
	formerBest chainhash.Hash
	newBest    chainhash.Hash
	reply      chan forceReorganizationResponse
}

// getLotterDataResponse is a response sent to the reply channel of a
// getLotteryDataMsg query.
type getLotterDataResponse struct {
	finalState     [6]byte
	poolSize       uint32
	winningTickets []chainhash.Hash
	err            error
}

// getLotteryDataMsg is a message type to be sent across the message
// channel for requesting lottery data about some block.
type getLotteryDataMsg struct {
	hash  chainhash.Hash
	reply chan getLotterDataResponse
}

// checkMissedTicketsResponse is a response sent to the reply channel of a
// checkMissedTicketsMsg query.
type checkMissedTicketsResponse struct {
	missedTickets map[chainhash.Hash]bool
}

// checkMissedTicketsMsg is a message type to be sent across the message
// channel used for checking whether or not a list of tickets has been missed.
type checkMissedTicketsMsg struct {
	tickets []chainhash.Hash
	reply   chan checkMissedTicketsResponse
}

// getTopBlockResponse is a response to the request for the block at HEAD of the
// blockchain. We need to be able to obtain this from blockChain for mining
// purposes.
type getTopBlockResponse struct {
	block dcrutil.Block
	err   error
}

// calcNextReqStakeDifficultyMsg is a message type to be sent across the message
// channel for requesting the required stake difficulty of the next block.
type getTopBlockMsg struct {
	reply chan getTopBlockResponse
}

// processBlockResponse is a response sent to the reply channel of a
// processBlockMsg.
type processBlockResponse struct {
	isOrphan bool
	err      error
}

// processBlockMsg is a message type to be sent across the message channel
// for requested a block is processed.  Note this call differs from blockMsg
// above in that blockMsg is intended for blocks that came from peers and have
// extra handling whereas this message essentially is just a concurrent safe
// way to call ProcessBlock on the internal block chain instance.
type processBlockMsg struct {
	block *dcrutil.Block
	flags blockchain.BehaviorFlags
	reply chan processBlockResponse
}

// processTransactionResponse is a response sent to the reply channel of a
// processTransactionMsg.
type processTransactionResponse struct {
	err error
}

// processTransactionMsg is a message type to be sent across the message
// channel for requesting a transaction to be processed through the block
// manager.
type processTransactionMsg struct {
	tx           *dcrutil.Tx
	allowOrphans bool
	rateLimit    bool
	reply        chan processTransactionResponse
}

// fetchTransactionStoreResponse is a response sent to the reply channel of a
// fetchTransactionStoreMsg.
type fetchTransactionStoreResponse struct {
	TxStore blockchain.TxStore
	err     error
}

// fetchTransactionStoreMsg is a message type to be sent across the message
// channel fetching the tx input store for some Tx.
type fetchTransactionStoreMsg struct {
	tx          *dcrutil.Tx
	isTreeValid bool
	reply       chan fetchTransactionStoreResponse
}

// isCurrentMsg is a message type to be sent across the message channel for
// requesting whether or not the block manager believes it is synced with
// the currently connected peers.
type isCurrentMsg struct {
	reply chan bool
}

// missedTicketsMsg handles a request for the list of currently missed tickets
// from the ticket database.
type missedTicketsMsg struct {
	reply chan missedTicketsResponse
}

// missedTicketsResponse is a response sent to the reply channel of a
// ticketBucketsMsg.
type missedTicketsResponse struct {
	Tickets stake.SStxMemMap
	err     error
}

// pauseMsg is a message type to be sent across the message channel for
// pausing the block manager.  This effectively provides the caller with
// exclusive access over the manager until a receive is performed on the
// unpause channel.
type pauseMsg struct {
	unpause <-chan struct{}
}

// ticketsForAddressMsg handles a request for obtaining all the current
// tickets corresponding to some address.
type ticketsForAddressMsg struct {
	Address dcrutil.Address
	reply   chan ticketsForAddressResponse
}

// ticketsForAddressResponse is a response to the reply channel of a
// ticketsForAddressMsg.
type ticketsForAddressResponse struct {
	Tickets []chainhash.Hash
	err     error
}

// existsLiveTicketMsg handles a request for obtaining whether or not a
// ticket exists in the live tickets map of the blockchain stake database.
type existsLiveTicketMsg struct {
	hash  *chainhash.Hash
	reply chan existsLiveTicketResponse
}

// existsLiveTicketResponse is a response to the reply channel of a
// existsLiveTicketMsg.
type existsLiveTicketResponse struct {
	Exists bool
	err    error
}

// existsLiveTicketsMsg handles a request for obtaining whether or not a ticket
// from a slice of tickets exists in the live tickets map of the blockchain stake
// database.
type existsLiveTicketsMsg struct {
	hashes []*chainhash.Hash
	reply  chan existsLiveTicketsResponse
}

// existsLiveTicketsResponse is a response to the reply channel of a
// existsLiveTicketsMsg.
type existsLiveTicketsResponse struct {
	Exists []bool
	err    error
}

// getCurrentTemplateMsg handles a request for the current mining block template.
type getCurrentTemplateMsg struct {
	reply chan getCurrentTemplateResponse
}

// getCurrentTemplateResponse is a response sent to the reply channel of a
// getCurrentTemplateMsg.
type getCurrentTemplateResponse struct {
	Template *BlockTemplate
}

// setCurrentTemplateMsg handles a request to change the current mining block
// template.
type setCurrentTemplateMsg struct {
	Template *BlockTemplate
	reply    chan setCurrentTemplateResponse
}

// setCurrentTemplateResponse is a response sent to the reply channel of a
// setCurrentTemplateMsg.
type setCurrentTemplateResponse struct {
}

// getParentTemplateMsg handles a request for the current parent mining block
// template.
type getParentTemplateMsg struct {
	reply chan getParentTemplateResponse
}

// getParentTemplateResponse is a response sent to the reply channel of a
// getParentTemplateMsg.
type getParentTemplateResponse struct {
	Template *BlockTemplate
}

// setParentTemplateMsg handles a request to change the parent mining block
// template.
type setParentTemplateMsg struct {
	Template *BlockTemplate
	reply    chan setParentTemplateResponse
}

// setParentTemplateResponse is a response sent to the reply channel of a
// setParentTemplateMsg.
type setParentTemplateResponse struct {
}

// headerNode is used as a node in a list of headers that are linked together
// between checkpoints.
type headerNode struct {
	height int64
	sha    *chainhash.Hash
}

// chainState tracks the state of the best chain as blocks are inserted.  This
// is done because blockchain is currently not safe for concurrent access and the
// block manager is typically quite busy processing block and inventory.
// Therefore, requesting this information from chain through the block manager
// would not be anywhere near as efficient as simply updating it as each block
// is inserted and protecting it with a mutex.
type chainState struct {
	sync.Mutex
	newestHash        *chainhash.Hash
	newestHeight      int64
	nextFinalState    [6]byte
	nextPoolSize      uint32
	winningTickets    []chainhash.Hash
	missedTickets     []chainhash.Hash
	curBlockHeader    *wire.BlockHeader
	pastMedianTime    time.Time
	pastMedianTimeErr error
}

// Best returns the block hash and height known for the tip of the best known
// chain.
//
// This function is safe for concurrent access.
func (c *chainState) Best() (*chainhash.Hash, int64) {
	c.Lock()
	defer c.Unlock()

	return c.newestHash, c.newestHeight
}

// NextWPO returns next winner, potential, and overflow for the current top block
// of the blockchain.
//
// This function is safe for concurrent access.
func (c *chainState) NextFinalState() [6]byte {
	c.Lock()
	defer c.Unlock()

	return c.nextFinalState
}

func (c *chainState) NextPoolSize() uint32 {
	c.Lock()
	defer c.Unlock()

	return c.nextPoolSize
}

// NextWinners returns the eligible SStx hashes to vote on the
// next block as inputs for SSGen.
//
// This function is safe for concurrent access.
func (c *chainState) NextWinners() []chainhash.Hash {
	c.Lock()
	defer c.Unlock()

	return c.winningTickets
}

// CurrentlyMissed returns the eligible SStx hashes that can be revoked.
//
// This function is safe for concurrent access.
func (c *chainState) CurrentlyMissed() []chainhash.Hash {
	c.Lock()
	defer c.Unlock()

	return c.missedTickets
}

// CurrentlyMissed returns the eligible SStx hashes to vote on the
// next block as inputs for SSGen.
//
// This function is safe for concurrent access.
func (c *chainState) GetTopBlockHeader() *wire.BlockHeader {
	c.Lock()
	defer c.Unlock()

	return c.curBlockHeader
}

// BlockLotteryData refers to cached data that is generated when a block
// is inserted, so that it doesn't later need to be recalculated.
type BlockLotteryData struct {
	ntfnData   *WinningTicketsNtfnData
	poolSize   uint32
	finalState [6]byte
}

// blockManager provides a concurrency safe block manager for handling all
// incoming blocks.
type blockManager struct {
	server              *server
	started             int32
	shutdown            int32
	blockChain          *blockchain.BlockChain
	requestedTxns       map[chainhash.Hash]struct{}
	requestedEverTxns   map[chainhash.Hash]uint8
	requestedBlocks     map[chainhash.Hash]struct{}
	requestedEverBlocks map[chainhash.Hash]uint8
	progressLogger      *blockProgressLogger
	receivedLogBlocks   int64
	receivedLogTx       int64
	lastBlockLogTime    time.Time
	processingReqs      bool
	syncPeer            *peer
	msgChan             chan interface{}
	chainState          chainState
	wg                  sync.WaitGroup
	quit                chan struct{}

	blockLotteryDataCache      map[chainhash.Hash]*BlockLotteryData
	blockLotteryDataCacheMutex *sync.Mutex

	// The following fields are used for headers-first mode.
	headersFirstMode bool
	headerList       *list.List
	startHeader      *list.Element
	nextCheckpoint   *chaincfg.Checkpoint

	cachedCurrentTemplate *BlockTemplate
	cachedParentTemplate  *BlockTemplate
	AggressiveMining      bool
}

// resetHeaderState sets the headers-first mode state to values appropriate for
// syncing from a new peer.
func (b *blockManager) resetHeaderState(newestHash *chainhash.Hash, newestHeight int64) {
	b.headersFirstMode = false
	b.headerList.Init()
	b.startHeader = nil

	// When there is a next checkpoint, add an entry for the latest known
	// block into the header pool.  This allows the next downloaded header
	// to prove it links to the chain properly.
	if b.nextCheckpoint != nil {
		node := headerNode{height: newestHeight, sha: newestHash}
		b.headerList.PushBack(&node)
	}
}

// updateChainState updates the chain state associated with the block manager.
// This allows fast access to chain information since blockchain is currently not
// safe for concurrent access and the block manager is typically quite busy
// processing block and inventory.
func (b *blockManager) updateChainState(newestHash *chainhash.Hash,
	newestHeight int64,
	finalState [6]byte,
	poolSize uint32,
	winningTickets []chainhash.Hash,
	missedTickets []chainhash.Hash,
	curBlockHeader *wire.BlockHeader) {

	b.chainState.Lock()
	defer b.chainState.Unlock()

	b.chainState.newestHash = newestHash
	b.chainState.newestHeight = newestHeight
	medianTime, err := b.blockChain.CalcPastMedianTime()
	if err != nil {
		b.chainState.pastMedianTimeErr = err
	} else {
		b.chainState.pastMedianTime = medianTime
	}

	b.chainState.nextFinalState = finalState
	b.chainState.nextPoolSize = poolSize
	b.chainState.winningTickets = winningTickets
	b.chainState.missedTickets = missedTickets
	b.chainState.curBlockHeader = curBlockHeader
}

// findNextHeaderCheckpoint returns the next checkpoint after the passed height.
// It returns nil when there is not one either because the height is already
// later than the final checkpoint or some other reason such as disabled
// checkpoints.
func (b *blockManager) findNextHeaderCheckpoint(height int64) *chaincfg.Checkpoint {
	// There is no next checkpoint if checkpoints are disabled or there are
	// none for this current network.
	if cfg.DisableCheckpoints {
		return nil
	}
	checkpoints := b.server.chainParams.Checkpoints
	if len(checkpoints) == 0 {
		return nil
	}

	// There is no next checkpoint if the height is already after the final
	// checkpoint.
	finalCheckpoint := &checkpoints[len(checkpoints)-1]
	if height >= finalCheckpoint.Height {
		return nil
	}

	// Find the next checkpoint.
	nextCheckpoint := finalCheckpoint
	for i := len(checkpoints) - 2; i >= 0; i-- {
		if height >= checkpoints[i].Height {
			break
		}
		nextCheckpoint = &checkpoints[i]
	}
	return nextCheckpoint
}

// startSync will choose the best peer among the available candidate peers to
// download/sync the blockchain from.  When syncing is already running, it
// simply returns.  It also examines the candidates for any which are no longer
// candidates and removes them as needed.
func (b *blockManager) startSync(peers *list.List) {
	// Return now if we're already syncing.
	if b.syncPeer != nil {
		return
	}

	// Find the height of the current known best block.
	_, height, err := b.server.db.NewestSha()
	if err != nil {
		bmgrLog.Errorf("%v", err)
		return
	}

	var bestPeer *peer
	var enext *list.Element
	for e := peers.Front(); e != nil; e = enext {
		enext = e.Next()
		p := e.Value.(*peer)

		// Remove sync candidate peers that are no longer candidates due
		// to passing their latest known block.  NOTE: The < is
		// intentional as opposed to <=.  While techcnically the peer
		// doesn't have a later block when it's equal, it will likely
		// have one soon so it is a reasonable choice.  It also allows
		// the case where both are at 0 such as during regression test.
		if p.lastBlock < int32(height) {
			peers.Remove(e)
			continue
		}

		// TODO(davec): Use a better algorithm to choose the best peer.
		// For now, just pick the first available candidate.
		bestPeer = p
	}

	// Start syncing from the best peer if one was selected.
	if bestPeer != nil {
		locator, err := b.blockChain.LatestBlockLocator()
		if err != nil {
			bmgrLog.Errorf("Failed to get block locator for the "+
				"latest block: %v", err)
			return
		}

		bmgrLog.Infof("Syncing to block height %d from peer %v",
			bestPeer.lastBlock, bestPeer.addr)

		// When the current height is less than a known checkpoint we
		// can use block headers to learn about which blocks comprise
		// the chain up to the checkpoint and perform less validation
		// for them.  This is possible since each header contains the
		// hash of the previous header and a merkle root.  Therefore if
		// we validate all of the received headers link together
		// properly and the checkpoint hashes match, we can be sure the
		// hashes for the blocks in between are accurate.  Further, once
		// the full blocks are downloaded, the merkle root is computed
		// and compared against the value in the header which proves the
		// full block hasn't been tampered with.
		//
		// Once we have passed the final checkpoint, or checkpoints are
		// disabled, use standard inv messages learn about the blocks
		// and fully validate them.  Finally, regression test mode does
		// not support the headers-first approach so do normal block
		// downloads when in regression test mode.
		if b.nextCheckpoint != nil && height < b.nextCheckpoint.Height &&
			!cfg.DisableCheckpoints {

			bestPeer.PushGetHeadersMsg(locator, b.nextCheckpoint.Hash)
			b.headersFirstMode = true
			bmgrLog.Infof("Downloading headers for blocks %d to "+
				"%d from peer %s", height+1,
				b.nextCheckpoint.Height, bestPeer.addr)
		} else {
			bestPeer.PushGetBlocksMsg(locator, &zeroHash)
		}
		b.syncPeer = bestPeer
	} else {
		bmgrLog.Warnf("No sync peer candidates available")
	}
}

// isSyncCandidate returns whether or not the peer is a candidate to consider
// syncing from.
func (b *blockManager) isSyncCandidate(p *peer) bool {
	// The peer is not a candidate for sync if it's not a full node.
	if p.services&wire.SFNodeNetwork != wire.SFNodeNetwork {
		return false
	}

	// Candidate if all checks passed.
	return true
}

// syncMiningStateAfterSync polls the blockMananger for the current sync
// state; if the mananger is synced, it executes a call to the peer to
// sync the mining state to the network.
func (b *blockManager) syncMiningStateAfterSync(p *peer) {
	ticker := time.NewTicker(time.Second * 3)
	go func() {
		for {
			select {
			case <-ticker.C:
				if b.IsCurrent() {
					p.PushGetMiningStateMsg()
					return
				}
			}
		}
	}()
}

// handleNewPeerMsg deals with new peers that have signalled they may
// be considered as a sync peer (they have already successfully negotiated).  It
// also starts syncing if needed.  It is invoked from the syncHandler goroutine.
func (b *blockManager) handleNewPeerMsg(peers *list.List, p *peer) {
	// Ignore if in the process of shutting down.
	if atomic.LoadInt32(&b.shutdown) != 0 {
		return
	}

	bmgrLog.Infof("New valid peer %s (%s)", p, p.userAgent)

	// Ignore the peer if it's not a sync candidate.
	if !b.isSyncCandidate(p) {
		return
	}

	// Add the peer as a candidate to sync from.
	peers.PushBack(p)

	// Start syncing by choosing the best candidate if needed.
	b.startSync(peers)

	// Grab the mining state from this peer after we're synced.
	if !cfg.NoMiningStateSync {
		b.syncMiningStateAfterSync(p)
	}
}

// handleDonePeerMsg deals with peers that have signalled they are done.  It
// removes the peer as a candidate for syncing and in the case where it was
// the current sync peer, attempts to select a new best peer to sync from.  It
// is invoked from the syncHandler goroutine.
func (b *blockManager) handleDonePeerMsg(peers *list.List, p *peer) {
	// Remove the peer from the list of candidate peers.
	for e := peers.Front(); e != nil; e = e.Next() {
		if e.Value == p {
			peers.Remove(e)
			break
		}
	}

	bmgrLog.Infof("Lost peer %s", p)

	// Remove requested transactions from the global map so that they will
	// be fetched from elsewhere next time we get an inv.
	for k := range p.requestedTxns {
		delete(b.requestedTxns, k)
	}

	// Remove requested blocks from the global map so that they will be
	// fetched from elsewhere next time we get an inv.
	// TODO(oga) we could possibly here check which peers have these blocks
	// and request them now to speed things up a little.
	for k := range p.requestedBlocks {
		delete(b.requestedBlocks, k)
	}

	// Attempt to find a new peer to sync from if the quitting peer is the
	// sync peer.  Also, reset the headers-first state if in headers-first
	// mode so
	if b.syncPeer != nil && b.syncPeer == p {
		b.syncPeer = nil
		if b.headersFirstMode {
			// This really shouldn't fail.  We have a fairly
			// unrecoverable database issue if it does.
			newestHash, height, err := b.server.db.NewestSha()
			if err != nil {
				bmgrLog.Warnf("Unable to obtain latest "+
					"block information from the database: "+
					"%v", err)
				return
			}
			b.resetHeaderState(newestHash, height)
		}
		b.startSync(peers)
	}
}

// logBlockHeight logs a new block height as an information message to show
// progress to the user.  In order to prevent spam, it limits logging to one
// message every 10 seconds with duration and totals included.
func (b *blockManager) logBlockHeight(block *dcrutil.Block) {
	b.receivedLogBlocks++
	b.receivedLogTx += int64(len(block.MsgBlock().Transactions))

	now := time.Now()
	duration := now.Sub(b.lastBlockLogTime)
	if duration < time.Second*10 {
		return
	}

	// Truncate the duration to 10s of milliseconds.
	durationMillis := int64(duration / time.Millisecond)
	tDuration := 10 * time.Millisecond * time.Duration(durationMillis/10)

	// Log information about new block height.
	blockStr := "blocks"
	if b.receivedLogBlocks == 1 {
		blockStr = "block"
	}
	txStr := "transactions"
	if b.receivedLogTx == 1 {
		txStr = "transaction"
	}
	bmgrLog.Infof("Processed %d %s in the last %s (%d %s, height %d, %s)",
		b.receivedLogBlocks, blockStr, tDuration, b.receivedLogTx,
		txStr, block.Height(), block.MsgBlock().Header.Timestamp)

	b.receivedLogBlocks = 0
	b.receivedLogTx = 0
	b.lastBlockLogTime = now
}

// handleTxMsg handles transaction messages from all peers.
func (b *blockManager) handleTxMsg(tmsg *txMsg) {
	// NOTE:  BitcoinJ, and possibly other wallets, don't follow the spec of
	// sending an inventory message and allowing the remote peer to decide
	// whether or not they want to request the transaction via a getdata
	// message.  Unfortunately, the reference implementation permits
	// unrequested data, so it has allowed wallets that don't follow the
	// spec to proliferate.  While this is not ideal, there is no check here
	// to disconnect peers for sending unsolicited transactions to provide
	// interoperability.

	// Process the transaction to include validation, insertion in the
	// memory pool, orphan handling, etc.
	allowOrphans := cfg.MaxOrphanTxs > 0
	err := tmsg.peer.server.txMemPool.ProcessTransaction(tmsg.tx,
		allowOrphans, true)

	// Remove transaction from request maps. Either the mempool/chain
	// already knows about it and as such we shouldn't have any more
	// instances of trying to fetch it, or we failed to insert and thus
	// we'll retry next time we get an inv.
	txHash := tmsg.tx.Sha()
	delete(tmsg.peer.requestedTxns, *txHash)
	delete(b.requestedTxns, *txHash)

	if err != nil {
		// When the error is a rule error, it means the transaction was
		// simply rejected as opposed to something actually going wrong,
		// so log it as such.  Otherwise, something really did go wrong,
		// so log it as an actual error.
		if _, ok := err.(RuleError); ok {
			bmgrLog.Debugf("Rejected transaction %v from %s: %v",
				txHash, tmsg.peer, err)
		} else {
			bmgrLog.Errorf("Failed to process transaction %v: %v",
				txHash, err)
		}

		// Convert the error into an appropriate reject message and
		// send it.
		code, reason := errToRejectErr(err)
		tmsg.peer.PushRejectMsg(wire.CmdTx, code, reason, txHash,
			false)
		return
	}
}

// current returns true if we believe we are synced with our peers, false if we
// still have blocks to check
func (b *blockManager) current() bool {
	if !cfg.TestNet {
		if !b.blockChain.IsCurrent(b.server.timeSource) {
			return false
		}
	}

	// if blockChain thinks we are current and we have no syncPeer it
	// is probably right.
	if b.syncPeer == nil {
		return true
	}

	_, height, err := b.server.db.NewestSha()
	// No matter what chain thinks, if we are below the block we are
	// syncing to we are not current.
	// TODO(oga) we can get chain to return the height of each block when we
	// parse an orphan, which would allow us to update the height of peers
	// from what it was at initial handshake.
	if err != nil || height < int64(b.syncPeer.startingHeight) {
		return false
	}

	return true
}

// checkBlockForHiddenVotes checks to see if a newly added block contains
// any votes that were previously unknown to our daemon. If it does, it
// adds these votes to the cached parent block template.
//
// This is UNSAFE for concurrent access.
func (b *blockManager) checkBlockForHiddenVotes(block *dcrutil.Block) {
	var votesFromBlock []*dcrutil.Tx

	for _, stx := range block.STransactions() {
		isSSGen, _ := stake.IsSSGen(stx)
		if isSSGen {
			votesFromBlock = append(votesFromBlock, stx)
		}
	}

	// Identify the cached parent template; it's possible that
	// the parent template hasn't yet been updated, so we may
	// need to use the current template.
	var template *BlockTemplate

	if b.cachedCurrentTemplate != nil {
		if b.cachedCurrentTemplate.height ==
			block.Height() {
			template = b.cachedCurrentTemplate
		}
	}
	if template == nil &&
		b.cachedParentTemplate != nil {
		if b.cachedParentTemplate.height ==
			block.Height() {
			template = b.cachedParentTemplate
		}
	}

	// Now that we have the template, grab the votes and compare
	// them with those found in the newly added block. If we don't
	// the votes, they will need to be added to our block template.
	var updatedTxTreeStake []*dcrutil.Tx
	numVotes := 0
	if template != nil {
		var newVotes []*dcrutil.Tx

		templateBlock := dcrutil.NewBlock(template.block)
		templateBlock.SetHeight(template.height)
		for _, vote := range votesFromBlock {
			haveIt := false

			for _, stx := range templateBlock.STransactions() {
				isSSGen, _ := stake.IsSSGen(stx)
				if isSSGen {
					if vote.Sha().IsEqual(stx.Sha()) {
						haveIt = true
						numVotes++
						break
					}
				}
			}

			if !haveIt {
				// Jam it directly into the block.
				template.block.AddSTransaction(vote.MsgTx())
				newVotes = append(newVotes, vote)
				numVotes++
			}
		}

		// We have the list of new votes now; append it to the
		// list of template stake transactions.
		updatedTxTreeStake = append(templateBlock.STransactions(),
			newVotes...)
	} else {
		// We have no template, so nothing to update.
		return
	}

	// Create a new coinbase.
	random, err := wire.RandomUint64()
	if err != nil {
		return
	}
	height := block.MsgBlock().Header.Height
	opReturnPkScript, err := standardCoinbaseOpReturn(height,
		[]uint64{0, 0, 0, random})
	if err != nil {
		bmgrLog.Warnf("failed to create coinbase OP_RETURN while generating " +
			"block with extra found voters")
		return
	}
	coinbase, err := createCoinbaseTx(
		template.block.Transactions[0].TxIn[0].SignatureScript,
		opReturnPkScript,
		int64(template.block.Header.Height),
		cfg.miningAddrs[rand.Intn(len(cfg.miningAddrs))],
		uint16(numVotes),
		b.server.chainParams)
	if err != nil {
		bmgrLog.Warnf("failed to create coinbase while generating " +
			"block with extra found voters")
		return
	}
	template.block.Transactions[0] = coinbase.MsgTx()

	// Patch the header. First, reconstruct the merkle trees, then
	// correct the number of voters, and finally recalculate the size.
	var updatedTxTreeRegular []*dcrutil.Tx
	updatedTxTreeRegular = append(updatedTxTreeRegular, coinbase)
	for i, mtx := range template.block.Transactions {
		// Coinbase
		if i == 0 {
			continue
		}
		tx := dcrutil.NewTx(mtx)
		updatedTxTreeRegular = append(updatedTxTreeRegular, tx)
	}
	merkles := blockchain.BuildMerkleTreeStore(updatedTxTreeRegular)
	template.block.Header.StakeRoot = *merkles[len(merkles)-1]
	smerkles := blockchain.BuildMerkleTreeStore(updatedTxTreeStake)
	template.block.Header.Voters = uint16(numVotes)
	template.block.Header.StakeRoot = *smerkles[len(smerkles)-1]
	template.block.Header.Size = uint32(template.block.SerializeSize())

	return
}

// handleBlockMsg handles block messages from all peers.
func (b *blockManager) handleBlockMsg(bmsg *blockMsg) {
	// If we didn't ask for this block then the peer is misbehaving.
	blockSha := bmsg.block.Sha()
	if _, ok := bmsg.peer.requestedBlocks[*blockSha]; !ok {
		// Check to see if we ever requested this block, since it may
		// have been accidentally sent in duplicate. If it was,
		// increment the counter in the ever requested map and make
		// sure that the node isn't spamming us with these blocks.
		received, ok := b.requestedEverBlocks[*blockSha]
		if ok {
			if received > maxResendLimit {
				bmgrLog.Warnf("Got duplicate block %v from %s -- "+
					"too many times, disconnecting", blockSha,
					bmsg.peer.addr)
				bmsg.peer.Disconnect()
				return
			}
			b.requestedEverBlocks[*blockSha]++
		} else {
			bmgrLog.Warnf("Got unrequested block %v from %s -- "+
				"disconnecting", blockSha, bmsg.peer.addr)
			bmsg.peer.Disconnect()
			return
		}
	}

	// When in headers-first mode, if the block matches the hash of the
	// first header in the list of headers that are being fetched, it's
	// eligible for less validation since the headers have already been
	// verified to link together and are valid up to the next checkpoint.
	// Also, remove the list entry for all blocks except the checkpoint
	// since it is needed to verify the next round of headers links
	// properly.
	isCheckpointBlock := false
	behaviorFlags := blockchain.BFNone
	if b.headersFirstMode {
		firstNodeEl := b.headerList.Front()
		if firstNodeEl != nil {
			firstNode := firstNodeEl.Value.(*headerNode)
			if blockSha.IsEqual(firstNode.sha) {
				behaviorFlags |= blockchain.BFFastAdd
				if firstNode.sha.IsEqual(b.nextCheckpoint.Hash) {
					isCheckpointBlock = true
				} else {
					b.headerList.Remove(firstNodeEl)
				}
			}
		}
	}

	// Remove block from request maps. Either chain will know about it and
	// so we shouldn't have any more instances of trying to fetch it, or we
	// will fail the insert and thus we'll retry next time we get an inv.
	delete(bmsg.peer.requestedBlocks, *blockSha)
	delete(b.requestedBlocks, *blockSha)

	// Process the block to include validation, best chain selection, orphan
	// handling, etc.
	onMainChain, isOrphan, err := b.blockChain.ProcessBlock(bmsg.block,
		b.server.timeSource, behaviorFlags)

	if err != nil {
		// When the error is a rule error, it means the block was simply
		// rejected as opposed to something actually going wrong, so log
		// it as such.  Otherwise, something really did go wrong, so log
		// it as an actual error.
		if _, ok := err.(blockchain.RuleError); ok {
			bmgrLog.Infof("Rejected block %v from %s: %v", blockSha,
				bmsg.peer, err)
		} else {
			bmgrLog.Errorf("Failed to process block %v: %v",
				blockSha, err)
		}

		// Convert the error into an appropriate reject message and
		// send it.
		code, reason := errToRejectErr(err)
		bmsg.peer.PushRejectMsg(wire.CmdBlock, code, reason,
			blockSha, false)
		return
	}

	// Meta-data about the new block this peer is reporting. We use this
	// below to update this peer's lastest block height and the heights of
	// other peers based on their last announced block sha. This allows us
	// to dynamically update the block heights of peers, avoiding stale heights
	// when looking for a new sync peer. Upon acceptance of a block or
	// recognition of an orphan, we also use this information to update
	// the block heights over other peers who's invs may have been ignored
	// if we are actively syncing while the chain is not yet current or
	// who may have lost the lock announcment race.
	var heightUpdate int32
	var blkShaUpdate *chainhash.Hash

	// Request the parents for the orphan block from the peer that sent it.
	if isOrphan {
		// We've just received an orphan block from a peer. In order
		// to update the height of the peer, we try to extract the
		// block height from the scriptSig of the coinbase transaction.
		// Extraction is only attempted if the block's version is
		// high enough (ver 2+).
		header := &bmsg.block.MsgBlock().Header
		cbHeight := header.Height
		heightUpdate = int32(cbHeight)
		blkShaUpdate = blockSha

		orphanRoot := b.blockChain.GetOrphanRoot(blockSha)
		locator, err := b.blockChain.LatestBlockLocator()
		if err != nil {
			bmgrLog.Warnf("Failed to get block locator for the "+
				"latest block: %v", err)
		} else {
			bmsg.peer.PushGetBlocksMsg(locator, orphanRoot)
		}
	} else {
		// When the block is not an orphan, log information about it and
		// update the chain state.
		b.progressLogger.LogBlockHeight(bmsg.block)
		r := b.server.rpcServer

		// Query the DB for the winning SStx for the next top block if we've
		// reached stake validation height. Broadcast them if this is the first
		// time determining them.
		b.blockLotteryDataCacheMutex.Lock()
		broadcastWinners := false
		lotteryData := new(BlockLotteryData)

		_, exists := b.blockLotteryDataCache[*blockSha]
		if !exists {
			winningTickets, poolSize, finalState, err :=
				b.blockChain.GetWinningTickets(*blockSha)
			if err != nil && int64(bmsg.block.MsgBlock().Header.Height) >=
				b.server.chainParams.StakeValidationHeight-1 {
				bmgrLog.Errorf("Failed to get next winning tickets: %v", err)

				code, reason := errToRejectErr(err)
				bmsg.peer.PushRejectMsg(wire.CmdBlock, code, reason,
					blockSha, false)
				b.blockLotteryDataCacheMutex.Unlock()
				return
			}

			winningTicketsNtfn := &WinningTicketsNtfnData{
				*blockSha,
				int64(bmsg.block.MsgBlock().Header.Height),
				winningTickets}
			lotteryData = &BlockLotteryData{
				winningTicketsNtfn,
				uint32(poolSize),
				finalState,
			}
			b.blockLotteryDataCache[*blockSha] = lotteryData
			broadcastWinners = true
			b.blockLotteryDataCacheMutex.Unlock()
		} else {
			lotteryData, _ = b.blockLotteryDataCache[*blockSha]
			b.blockLotteryDataCacheMutex.Unlock()
		}
		if r != nil && broadcastWinners {
			// Rebroadcast the existing data to WS clients.
			r.ntfnMgr.NotifyWinningTickets(lotteryData.ntfnData)
		}

		if onMainChain {
			// A new block is connected, however, this new block may have
			// votes in it that were hidden from the network and which
			// validate our parent block. We should bolt these new votes
			// into the tx tree stake of the old block template on parent.
			svl := b.server.chainParams.StakeValidationHeight
			if b.AggressiveMining && bmsg.block.Height() >= svl {
				b.checkBlockForHiddenVotes(bmsg.block)
			}

			// Query the db for the latest best block since the block
			// that was processed could be on a side chain or have caused
			// a reorg.
			newestSha, newestHeight, _ := b.server.db.NewestSha()

			// Query the DB for the missed tickets for the next top block.
			missedTickets := b.blockChain.GetMissedTickets()

			// Retrieve the current block header.
			curBlockHeader := b.blockChain.GetCurrentBlockHeader()

			if r != nil {
				// Update registered websocket clients on the
				// current stake difficulty.
				nextStakeDiff, err :=
					b.blockChain.CalcNextRequiredStakeDifficulty()
				if err != nil {
					bmgrLog.Warnf("Failed to get next stake difficulty "+
						"calculation: %v", err)

				} else {
					r.ntfnMgr.NotifyStakeDifficulty(
						&StakeDifficultyNtfnData{
							*newestSha,
							newestHeight,
							nextStakeDiff,
						})
					b.server.txMemPool.PruneStakeTx(nextStakeDiff,
						b.chainState.newestHeight)
					b.server.txMemPool.PruneExpiredTx(b.chainState.newestHeight)
				}
			}

			b.updateChainState(newestSha,
				newestHeight,
				lotteryData.finalState,
				lotteryData.poolSize,
				lotteryData.ntfnData.Tickets,
				missedTickets,
				curBlockHeader)

			// Update this peer's latest block height, for future
			// potential sync node candidancy.
			heightUpdate = int32(newestHeight)
			blkShaUpdate = newestSha

			// Allow any clients performing long polling via the
			// getblocktemplate RPC to be notified when the new block causes
			// their old block template to become stale.
			rpcServer := b.server.rpcServer
			if rpcServer != nil {
				rpcServer.gbtWorkState.NotifyBlockConnected(blockSha)
			}
		}
	}

	// Update the block height for this peer. But only send a message to
	// the server for updating peer heights if this is an orphan or our
	// chain is "current". This avoid sending a spammy amount of messages
	// if we're syncing the chain from scratch.
	if blkShaUpdate != nil && heightUpdate != 0 {
		bmsg.peer.UpdateLastBlockHeight(heightUpdate)
		if isOrphan || b.current() {
			go b.server.UpdatePeerHeights(blkShaUpdate, int32(heightUpdate),
				bmsg.peer)
		}
	}
	// Sync the db to disk.
	b.server.db.Sync()

	// Nothing more to do if we aren't in headers-first mode.
	if !b.headersFirstMode {
		return
	}

	// This is headers-first mode, so if the block is not a checkpoint
	// request more blocks using the header list when the request queue is
	// getting short.
	if !isCheckpointBlock {
		if b.startHeader != nil &&
			len(bmsg.peer.requestedBlocks) < minInFlightBlocks {
			b.fetchHeaderBlocks()
		}
		return
	}

	// This is headers-first mode and the block is a checkpoint.  When
	// there is a next checkpoint, get the next round of headers by asking
	// for headers starting from the block after this one up to the next
	// checkpoint.
	prevHeight := b.nextCheckpoint.Height
	prevHash := b.nextCheckpoint.Hash
	b.nextCheckpoint = b.findNextHeaderCheckpoint(prevHeight)
	if b.nextCheckpoint != nil {
		locator := blockchain.BlockLocator([]*chainhash.Hash{prevHash})
		err := bmsg.peer.PushGetHeadersMsg(locator, b.nextCheckpoint.Hash)
		if err != nil {
			bmgrLog.Warnf("Failed to send getheaders message to "+
				"peer %s: %v", bmsg.peer.addr, err)
			return
		}
		bmgrLog.Infof("Downloading headers for blocks %d to %d from "+
			"peer %s", prevHeight+1, b.nextCheckpoint.Height,
			b.syncPeer.addr)
		return
	}

	// This is headers-first mode, the block is a checkpoint, and there are
	// no more checkpoints, so switch to normal mode by requesting blocks
	// from the block after this one up to the end of the chain (zero hash).
	b.headersFirstMode = false
	b.headerList.Init()
	bmgrLog.Infof("Reached the final checkpoint -- switching to normal mode")
	locator := blockchain.BlockLocator([]*chainhash.Hash{blockSha})
	err = bmsg.peer.PushGetBlocksMsg(locator, &zeroHash)
	if err != nil {
		bmgrLog.Warnf("Failed to send getblocks message to peer %s: %v",
			bmsg.peer.addr, err)
		return
	}
}

// fetchHeaderBlocks creates and sends a request to the syncPeer for the next
// list of blocks to be downloaded based on the current list of headers.
func (b *blockManager) fetchHeaderBlocks() {
	// Nothing to do if there is no start header.
	if b.startHeader == nil {
		bmgrLog.Warnf("fetchHeaderBlocks called with no start header")
		return
	}

	// Build up a getdata request for the list of blocks the headers
	// describe.  The size hint will be limited to wire.MaxInvPerMsg by
	// the function, so no need to double check it here.
	gdmsg := wire.NewMsgGetDataSizeHint(uint(b.headerList.Len()))
	numRequested := 0
	for e := b.startHeader; e != nil; e = e.Next() {
		node, ok := e.Value.(*headerNode)
		if !ok {
			bmgrLog.Warn("Header list node type is not a headerNode")
			continue
		}

		iv := wire.NewInvVect(wire.InvTypeBlock, node.sha)
		haveInv, err := b.haveInventory(iv)
		if err != nil {
			bmgrLog.Warnf("Unexpected failure when checking for "+
				"existing inventory during header block "+
				"fetch: %v", err)
		}
		if !haveInv {
			b.requestedBlocks[*node.sha] = struct{}{}
			b.requestedEverBlocks[*node.sha] = 0
			b.syncPeer.requestedBlocks[*node.sha] = struct{}{}
			gdmsg.AddInvVect(iv)
			numRequested++
		}
		b.startHeader = e.Next()
		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	if len(gdmsg.InvList) > 0 {
		b.syncPeer.QueueMessage(gdmsg, nil)
	}
}

// handleHeadersMsghandles headers messages from all peers.
func (b *blockManager) handleHeadersMsg(hmsg *headersMsg) {
	// The remote peer is misbehaving if we didn't request headers.
	msg := hmsg.headers
	numHeaders := len(msg.Headers)
	if !b.headersFirstMode {
		bmgrLog.Warnf("Got %d unrequested headers from %s -- "+
			"disconnecting", numHeaders, hmsg.peer.addr)
		hmsg.peer.Disconnect()
		return
	}

	// Nothing to do for an empty headers message.
	if numHeaders == 0 {
		return
	}

	// Process all of the received headers ensuring each one connects to the
	// previous and that checkpoints match.
	receivedCheckpoint := false
	var finalHash *chainhash.Hash
	for _, blockHeader := range msg.Headers {
		blockHash := blockHeader.BlockSha()
		finalHash = &blockHash

		// Ensure there is a previous header to compare against.
		prevNodeEl := b.headerList.Back()
		if prevNodeEl == nil {
			bmgrLog.Warnf("Header list does not contain a previous" +
				"element as expected -- disconnecting peer")
			hmsg.peer.Disconnect()
			return
		}

		// Ensure the header properly connects to the previous one and
		// add it to the list of headers.
		node := headerNode{sha: &blockHash}
		prevNode := prevNodeEl.Value.(*headerNode)
		if prevNode.sha.IsEqual(&blockHeader.PrevBlock) {
			node.height = prevNode.height + 1
			e := b.headerList.PushBack(&node)
			if b.startHeader == nil {
				b.startHeader = e
			}
		} else {
			bmgrLog.Warnf("Received block header that does not "+
				"properly connect to the chain from peer %s "+
				"-- disconnecting", hmsg.peer.addr)
			hmsg.peer.Disconnect()
			return
		}

		// Verify the header at the next checkpoint height matches.
		if node.height == b.nextCheckpoint.Height {
			if node.sha.IsEqual(b.nextCheckpoint.Hash) {
				receivedCheckpoint = true
				bmgrLog.Infof("Verified downloaded block "+
					"header against checkpoint at height "+
					"%d/hash %s", node.height, node.sha)
			} else {
				bmgrLog.Warnf("Block header at height %d/hash "+
					"%s from peer %s does NOT match "+
					"expected checkpoint hash of %s -- "+
					"disconnecting", node.height,
					node.sha, hmsg.peer.addr,
					b.nextCheckpoint.Hash)
				hmsg.peer.Disconnect()
				return
			}
			break
		}
	}

	// When this header is a checkpoint, switch to fetching the blocks for
	// all of the headers since the last checkpoint.
	if receivedCheckpoint {
		// Since the first entry of the list is always the final block
		// that is already in the database and is only used to ensure
		// the next header links properly, it must be removed before
		// fetching the blocks.
		b.headerList.Remove(b.headerList.Front())
		bmgrLog.Infof("Received %v block headers: Fetching blocks",
			b.headerList.Len())
		b.progressLogger.SetLastLogTime(time.Now())
		b.fetchHeaderBlocks()
		return
	}

	// This header is not a checkpoint, so request the next batch of
	// headers starting from the latest known header and ending with the
	// next checkpoint.
	locator := blockchain.BlockLocator([]*chainhash.Hash{finalHash})
	err := hmsg.peer.PushGetHeadersMsg(locator, b.nextCheckpoint.Hash)
	if err != nil {
		bmgrLog.Warnf("Failed to send getheaders message to "+
			"peer %s: %v", hmsg.peer.addr, err)
		return
	}
}

// haveInventory returns whether or not the inventory represented by the passed
// inventory vector is known.  This includes checking all of the various places
// inventory can be when it is in different states such as blocks that are part
// of the main chain, on a side chain, in the orphan pool, and transactions that
// are in the memory pool (either the main pool or orphan pool).
func (b *blockManager) haveInventory(invVect *wire.InvVect) (bool, error) {
	switch invVect.Type {
	case wire.InvTypeBlock:
		// Ask chain if the block is known to it in any form (main
		// chain, side chain, or orphan).
		return b.blockChain.HaveBlock(&invVect.Hash)

	case wire.InvTypeTx:
		// Ask the transaction memory pool if the transaction is known
		// to it in any form (main pool or orphan).
		if b.server.txMemPool.HaveTransaction(&invVect.Hash) {
			return true, nil
		}

		// Check if the transaction exists from the point of view of the
		// end of the main chain.
		return b.server.db.ExistsTxSha(&invVect.Hash)
	}

	// The requested inventory is is an unsupported type, so just claim
	// it is known to avoid requesting it.
	return true, nil
}

// handleInvMsg handles inv messages from all peers.
// We examine the inventory advertised by the remote peer and act accordingly.
func (b *blockManager) handleInvMsg(imsg *invMsg) {
	// Attempt to find the final block in the inventory list.  There may
	// not be one.
	lastBlock := -1
	invVects := imsg.inv.InvList
	for i := len(invVects) - 1; i >= 0; i-- {
		if invVects[i].Type == wire.InvTypeBlock {
			lastBlock = i
			break
		}
	}

	// If this inv contains a block annoucement, and this isn't coming from
	// our current sync peer or we're current, then update the last
	// announced block for this peer. We'll use this information later to
	// update the heights of peers based on blocks we've accepted that they
	// previously announced.
	if lastBlock != -1 && (imsg.peer != b.syncPeer || b.current()) {
		imsg.peer.UpdateLastAnnouncedBlock(&invVects[lastBlock].Hash)
	}

	// Ignore invs from peers that aren't the sync if we are not current.
	// Helps prevent fetching a mass of orphans.
	if imsg.peer != b.syncPeer && !b.current() {
		return
	}

	// If our chain is current and a peer announces a block we already
	// know of, then update their current block height.
	if lastBlock != -1 && b.current() {
		exists, err := b.server.db.ExistsSha(&invVects[lastBlock].Hash)
		if err == nil && exists {
			blkHeight, err := b.server.db.FetchBlockHeightBySha(
				&invVects[lastBlock].Hash)
			if err != nil {
				bmgrLog.Warnf("Unable to fetch block height for block "+
					"(sha: %v), %v",
					&invVects[lastBlock].Hash, err)
			} else {
				imsg.peer.UpdateLastBlockHeight(int32(blkHeight))
			}
		}
	}

	// Request the advertised inventory if we don't already have it.  Also,
	// request parent blocks of orphans if we receive one we already have.
	// Finally, attempt to detect potential stalls due to long side chains
	// we already have and request more blocks to prevent them.
	chain := b.blockChain
	for i, iv := range invVects {
		// Ignore unsupported inventory types.
		if iv.Type != wire.InvTypeBlock && iv.Type != wire.InvTypeTx {
			continue
		}

		// Add the inventory to the cache of known inventory
		// for the peer.
		imsg.peer.AddKnownInventory(iv)

		// Ignore inventory when we're in headers-first mode.
		if b.headersFirstMode {
			continue
		}

		// Request the inventory if we don't already have it.
		haveInv, err := b.haveInventory(iv)
		if err != nil {
			bmgrLog.Warnf("Unexpected failure when checking for "+
				"existing inventory during inv message "+
				"processing: %v", err)
			continue
		}
		if !haveInv {
			// Add it to the request queue.
			imsg.peer.requestQueue = append(imsg.peer.requestQueue, iv)
			continue
		}

		if iv.Type == wire.InvTypeBlock {
			// The block is an orphan block that we already have.
			// When the existing orphan was processed, it requested
			// the missing parent blocks.  When this scenario
			// happens, it means there were more blocks missing
			// than are allowed into a single inventory message.  As
			// a result, once this peer requested the final
			// advertised block, the remote peer noticed and is now
			// resending the orphan block as an available block
			// to signal there are more missing blocks that need to
			// be requested.
			if chain.IsKnownOrphan(&iv.Hash) {
				// Request blocks starting at the latest known
				// up to the root of the orphan that just came
				// in.
				orphanRoot := chain.GetOrphanRoot(&iv.Hash)
				locator, err := chain.LatestBlockLocator()
				if err != nil {
					bmgrLog.Errorf("PEER: Failed to get block "+
						"locator for the latest block: "+
						"%v", err)
					continue
				}
				imsg.peer.PushGetBlocksMsg(locator, orphanRoot)
				continue
			}

			// We already have the final block advertised by this
			// inventory message, so force a request for more.  This
			// should only happen if we're on a really long side
			// chain.
			if i == lastBlock {
				// Request blocks after this one up to the
				// final one the remote peer knows about (zero
				// stop hash).
				locator := chain.BlockLocatorFromHash(&iv.Hash)
				imsg.peer.PushGetBlocksMsg(locator, &zeroHash)
			}
		}
	}

	// Request as much as possible at once.  Anything that won't fit into
	// the request will be requested on the next inv message.
	numRequested := 0
	gdmsg := wire.NewMsgGetData()
	requestQueue := imsg.peer.requestQueue
	for len(requestQueue) != 0 {
		iv := requestQueue[0]
		requestQueue[0] = nil
		requestQueue = requestQueue[1:]

		switch iv.Type {
		case wire.InvTypeBlock:
			// Request the block if there is not already a pending
			// request.
			if _, exists := b.requestedBlocks[iv.Hash]; !exists {
				b.requestedBlocks[iv.Hash] = struct{}{}
				b.requestedEverBlocks[iv.Hash] = 0
				imsg.peer.requestedBlocks[iv.Hash] = struct{}{}
				gdmsg.AddInvVect(iv)
				numRequested++
			}

		case wire.InvTypeTx:
			// Request the transaction if there is not already a
			// pending request.
			if _, exists := b.requestedTxns[iv.Hash]; !exists {
				b.requestedTxns[iv.Hash] = struct{}{}
				b.requestedEverTxns[iv.Hash] = 0
				imsg.peer.requestedTxns[iv.Hash] = struct{}{}
				gdmsg.AddInvVect(iv)
				numRequested++
			}
		}

		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	imsg.peer.requestQueue = requestQueue
	if len(gdmsg.InvList) > 0 {
		imsg.peer.QueueMessage(gdmsg, nil)
	}
}

// blockHandler is the main handler for the block manager.  It must be run
// as a goroutine.  It processes block and inv messages in a separate goroutine
// from the peer handlers so the block (MsgBlock) messages are handled by a
// single thread without needing to lock memory data structures.  This is
// important because the block manager controls which blocks are needed and how
// the fetching should proceed.
func (b *blockManager) blockHandler() {
	candidatePeers := list.New()
out:
	for {
		select {
		case m := <-b.msgChan:
			switch msg := m.(type) {
			case *newPeerMsg:
				b.handleNewPeerMsg(candidatePeers, msg.peer)

			case *txMsg:
				b.handleTxMsg(msg)
				msg.peer.txProcessed <- struct{}{}

			case *blockMsg:
				b.handleBlockMsg(msg)
				msg.peer.blockProcessed <- struct{}{}

			case *invMsg:
				b.handleInvMsg(msg)

			case *headersMsg:
				b.handleHeadersMsg(msg)

			case *donePeerMsg:
				b.handleDonePeerMsg(candidatePeers, msg.peer)

			case getSyncPeerMsg:
				msg.reply <- b.syncPeer

			case requestFromPeerMsg:
				err := b.requestFromPeer(msg.peer, msg.blocks, msg.txs)
				msg.reply <- requestFromPeerResponse{
					err: err,
				}

			case checkConnectBlockMsg:
				err := b.blockChain.CheckConnectBlock(msg.block)
				msg.reply <- err

			case calcNextReqDifficultyMsg:
				difficulty, err :=
					b.blockChain.CalcNextRequiredDifficulty(
						msg.timestamp)
				msg.reply <- calcNextReqDifficultyResponse{
					difficulty: difficulty,
					err:        err,
				}

			case calcNextReqDiffNodeMsg:
				difficulty, err :=
					b.blockChain.CalcNextRequiredDiffFromNode(msg.hash,
						msg.timestamp)
				msg.reply <- calcNextReqDifficultyResponse{
					difficulty: difficulty,
					err:        err,
				}

			case calcNextReqStakeDifficultyMsg:
				stakeDiff, err := b.blockChain.CalcNextRequiredStakeDifficulty()
				msg.reply <- calcNextReqStakeDifficultyResponse{
					stakeDifficulty: stakeDiff,
					err:             err,
				}

			case forceReorganizationMsg:
				err := b.blockChain.ForceHeadReorganization(
					msg.formerBest,
					msg.newBest,
					b.server.timeSource)

				// Reorganizing has succeeded, so we need to
				// update the chain state.
				if err == nil {
					// Query the db for the latest best block since
					// the block that was processed could be on a
					// side chain or have caused a reorg.
					newestSha, newestHeight, _ := b.server.db.NewestSha()

					// Fetch the required lottery data from the cache;
					// it must already be there.
					b.blockLotteryDataCacheMutex.Lock()
					lotteryData, exists := b.blockLotteryDataCache[*newestSha]
					if !exists {
						b.blockLotteryDataCacheMutex.Unlock()
						msg.reply <- forceReorganizationResponse{
							err: fmt.Errorf("Failed to find lottery data in "+
								"cache while attempting reorganize to block %v",
								newestSha),
						}
						continue
					}
					b.blockLotteryDataCacheMutex.Unlock()

					r := b.server.rpcServer
					if r != nil {
						// Update registered websocket clients on the
						// current stake difficulty.
						nextStakeDiff, err :=
							b.blockChain.CalcNextRequiredStakeDifficulty()
						if err != nil {
							bmgrLog.Warnf("Failed to get next stake difficulty "+
								"calculation: %v", err)
						} else {
							r.ntfnMgr.NotifyStakeDifficulty(
								&StakeDifficultyNtfnData{
									*newestSha,
									newestHeight,
									nextStakeDiff,
								})
							b.server.txMemPool.PruneStakeTx(nextStakeDiff,
								b.chainState.newestHeight)
							b.server.txMemPool.PruneExpiredTx(
								b.chainState.newestHeight)
						}
					}

					missedTickets := b.blockChain.GetMissedTickets()

					curBlockHeader := b.blockChain.GetCurrentBlockHeader()

					b.updateChainState(newestSha,
						newestHeight,
						lotteryData.finalState,
						lotteryData.poolSize,
						lotteryData.ntfnData.Tickets,
						missedTickets,
						curBlockHeader)
				}

				msg.reply <- forceReorganizationResponse{
					err: err,
				}

			case getBlockFromHashMsg:
				b, err := b.blockChain.GetBlockFromHash(&msg.hash)
				msg.reply <- getBlockFromHashResponse{
					block: b,
					err:   err,
				}

			case getGenerationMsg:
				g, err := b.blockChain.GetGeneration(msg.hash)
				msg.reply <- getGenerationResponse{
					hashes: g,
					err:    err,
				}

			case getLotteryDataMsg:
				winningTickets, poolSize, finalState, err :=
					b.blockChain.GetWinningTickets(msg.hash)
				msg.reply <- getLotterDataResponse{
					finalState:     finalState,
					poolSize:       uint32(poolSize),
					winningTickets: winningTickets,
					err:            err,
				}

			case getTopBlockMsg:
				b, err := b.blockChain.GetTopBlock()
				msg.reply <- getTopBlockResponse{
					block: b,
					err:   err,
				}

			case fetchTransactionStoreMsg:
				txStore, err := b.blockChain.FetchTransactionStore(msg.tx,
					msg.isTreeValid)
				msg.reply <- fetchTransactionStoreResponse{
					TxStore: txStore,
					err:     err,
				}

			case processBlockMsg:
				onMainChain, isOrphan, err := b.blockChain.ProcessBlock(
					msg.block, b.server.timeSource, msg.flags)
				if err != nil {
					msg.reply <- processBlockResponse{
						isOrphan: false,
						err:      err,
					}
					continue
				}

				// Get the winning tickets. If they've yet to be broadcasted,
				// broadcast them.
				b.blockLotteryDataCacheMutex.Lock()
				broadcastWinners := false
				lotteryData := new(BlockLotteryData)
				_, exists := b.blockLotteryDataCache[*msg.block.Sha()]
				if !exists {
					winningTickets, poolSize, finalState, err :=
						b.blockChain.GetWinningTickets(*msg.block.Sha())
					if err != nil && int64(msg.block.MsgBlock().Header.Height) >=
						b.server.chainParams.StakeValidationHeight-1 {
						bmgrLog.Warnf("Stake failure in lottery tickets "+
							"calculation: %v", err)
						msg.reply <- processBlockResponse{
							isOrphan: false,
							err:      err,
						}
						b.blockLotteryDataCacheMutex.Unlock()
						continue
					}

					lotteryData.poolSize = uint32(poolSize)
					lotteryData.finalState = finalState
					lotteryData.ntfnData = &WinningTicketsNtfnData{
						*msg.block.Sha(),
						int64(msg.block.MsgBlock().Header.Height),
						winningTickets}
					b.blockLotteryDataCache[*msg.block.Sha()] = lotteryData
					broadcastWinners = true
				} else {
					lotteryData, _ = b.blockLotteryDataCache[*msg.block.Sha()]
				}

				r := b.server.rpcServer
				if r != nil && !isOrphan && broadcastWinners &&
					(msg.block.Height() >=
						b.server.chainParams.StakeValidationHeight-1) {
					// Notify registered websocket clients of newly
					// eligible tickets to vote on.
					if _, is := b.blockLotteryDataCache[*msg.block.Sha()]; !is {
						r.ntfnMgr.NotifyWinningTickets(lotteryData.ntfnData)
					}
				}
				b.blockLotteryDataCacheMutex.Unlock()

				// If the block added to the main chain, then we need to
				// update the tip locally on block manager.
				if onMainChain {
					// Query the db for the latest best block since
					// the block that was processed could be on a
					// side chain or have caused a reorg.
					newestSha, newestHeight, _ := b.server.db.NewestSha()

					// Update registered websocket clients on the
					// current stake difficulty.
					nextStakeDiff, err :=
						b.blockChain.CalcNextRequiredStakeDifficulty()
					if err != nil {
						bmgrLog.Warnf("Failed to get next stake difficulty "+
							"calculation: %v", err)
					} else {
						r.ntfnMgr.NotifyStakeDifficulty(
							&StakeDifficultyNtfnData{
								*newestSha,
								newestHeight,
								nextStakeDiff,
							})
						b.server.txMemPool.PruneStakeTx(nextStakeDiff,
							b.chainState.newestHeight)
						b.server.txMemPool.PruneExpiredTx(
							b.chainState.newestHeight)
					}

					missedTickets := b.blockChain.GetMissedTickets()
					curBlockHeader := b.blockChain.GetCurrentBlockHeader()

					b.updateChainState(newestSha,
						newestHeight,
						lotteryData.finalState,
						lotteryData.poolSize,
						lotteryData.ntfnData.Tickets,
						missedTickets,
						curBlockHeader)
				}

				msg.reply <- processBlockResponse{
					isOrphan: isOrphan,
					err:      nil,
				}

			case processTransactionMsg:
				err := b.server.txMemPool.ProcessTransaction(msg.tx,
					msg.allowOrphans, msg.rateLimit)
				msg.reply <- processTransactionResponse{
					err: err,
				}

			case isCurrentMsg:
				msg.reply <- b.current()

			case missedTicketsMsg:
				tickets, err := b.blockChain.MissedTickets()
				msg.reply <- missedTicketsResponse{
					Tickets: tickets,
					err:     err,
				}

			case pauseMsg:
				// Wait until the sender unpauses the manager.
				<-msg.unpause

			case ticketsForAddressMsg:
				tickets, err := b.blockChain.TicketsWithAddress(msg.Address)
				msg.reply <- ticketsForAddressResponse{
					Tickets: tickets,
					err:     err,
				}

			case existsLiveTicketMsg:
				exists, err := b.blockChain.CheckLiveTicket(msg.hash)
				msg.reply <- existsLiveTicketResponse{
					Exists: exists,
					err:    err,
				}

			case existsLiveTicketsMsg:
				exists, err := b.blockChain.CheckLiveTickets(msg.hashes)
				msg.reply <- existsLiveTicketsResponse{
					Exists: exists,
					err:    err,
				}

			case getCurrentTemplateMsg:
				cur := deepCopyBlockTemplate(b.cachedCurrentTemplate)
				msg.reply <- getCurrentTemplateResponse{
					Template: cur,
				}

			case setCurrentTemplateMsg:
				b.cachedCurrentTemplate = deepCopyBlockTemplate(msg.Template)
				msg.reply <- setCurrentTemplateResponse{}

			case getParentTemplateMsg:
				par := deepCopyBlockTemplate(b.cachedParentTemplate)
				msg.reply <- getParentTemplateResponse{
					Template: par,
				}

			case setParentTemplateMsg:
				b.cachedParentTemplate = deepCopyBlockTemplate(msg.Template)
				msg.reply <- setParentTemplateResponse{}

			default:
				bmgrLog.Warnf("Invalid message type in block "+
					"handler: %T", msg)
			}

		case <-b.quit:
			break out
		}
	}

	b.wg.Done()
	bmgrLog.Trace("Block handler done")
}

// handleNotifyMsg handles notifications from blockhain.  It does things such
// as request orphan block parents and relay accepted blocks to connected peers.
func (b *blockManager) handleNotifyMsg(notification *blockchain.Notification) {
	switch notification.Type {
	// A block has been accepted into the block chain.  Relay it to other
	// peers.

	case blockchain.NTBlockAccepted:
		// Don't relay if we are not current. Other peers that are
		// current should already know about it.
		if !b.current() {
			return
		}

		band, ok := notification.Data.(*blockchain.BlockAcceptedNtfnsData)
		if !ok {
			bmgrLog.Warnf("Chain accepted notification is not " +
				"BlockAcceptedNtfnsData.")
			break
		}
		block := band.Block
		r := b.server.rpcServer

		// Determine the winning tickets for this block, if it hasn't
		// already been sent out.
		if block.Height() >=
			b.server.chainParams.StakeValidationHeight-1 &&
			r != nil {

			hash := block.Sha()
			b.blockLotteryDataCacheMutex.Lock()
			lotteryData := new(BlockLotteryData)

			_, exists := b.blockLotteryDataCache[*hash]
			if !exists {
				// Obtain the winning tickets for this block. handleNotifyMsg
				// should be safe for concurrent access of things contained
				// within blockchain.
				wt, ps, fs, err := b.blockChain.GetWinningTickets(*hash)
				if err != nil {
					b.blockLotteryDataCacheMutex.Unlock()
					bmgrLog.Errorf("Couldn't calculate winning tickets for "+
						"accepted block %v: %v", block.Sha(), err.Error())
				} else {
					lotteryData.finalState = fs
					lotteryData.poolSize = uint32(ps)

					lotteryData.ntfnData = &WinningTicketsNtfnData{
						*hash,
						int64(block.MsgBlock().Header.Height),
						wt}
					b.blockLotteryDataCache[*hash] = lotteryData

					// Notify registered websocket clients of newly
					// eligible tickets to vote on.
					r.ntfnMgr.NotifyWinningTickets(lotteryData.ntfnData)
					b.blockLotteryDataCache[*hash] = lotteryData
					b.blockLotteryDataCacheMutex.Unlock()
				}
			}
		}

		// Generate the inventory vector and relay it.
		iv := wire.NewInvVect(wire.InvTypeBlock, block.Sha())
		b.server.RelayInventory(iv, nil)

	// A block has been connected to the main block chain.
	case blockchain.NTBlockConnected:
		blockSlice, ok := notification.Data.([]*dcrutil.Block)
		if !ok {
			bmgrLog.Warnf("Chain connected notification is not a block slice.")
			break
		}

		if len(blockSlice) != 2 {
			bmgrLog.Warnf("Chain connected notification is wrong size slice.")
			break
		}

		block := blockSlice[0]
		parentBlock := blockSlice[1]

		// Check and see if the regular tx tree of the previous block was
		// invalid or not. If it wasn't, then we need to restore all the tx
		// from this block into the mempool. They may end up being spent in
		// the regular tx tree of the current block, for which there is code
		// below.
		txTreeRegularValid := dcrutil.IsFlagSet16(block.MsgBlock().Header.VoteBits,
			dcrutil.BlockValid)

		if !txTreeRegularValid {
			for _, tx := range parentBlock.Transactions()[1:] {
				_, err := b.server.txMemPool.MaybeAcceptTransaction(tx, false,
					true)
				if err != nil {
					// Remove the transaction and all transactions
					// that depend on it if it wasn't accepted into
					// the transaction pool. Probably this will mostly
					// throw errors, as the majority will already be
					// in the mempool.
					b.server.txMemPool.RemoveTransaction(tx, true)
				}
			}
		}

		// Remove all of the regular and stake transactions in the
		// connected block from the transaction pool.  Also, remove any
		// transactions which are now double spends as a result of these
		// new transactions.  Note that removing a transaction from
		// no longer an orphan.  Note that removing a transaction from
		// transaction are NOT removed recursively because they are still
		// recursively. Do not remove the coinbase [1:] of the regular tx
		// tree.
		for _, tx := range parentBlock.Transactions()[1:] {
			b.server.txMemPool.RemoveTransaction(tx, false)
			b.server.txMemPool.RemoveDoubleSpends(tx)
			b.server.txMemPool.RemoveOrphan(tx.Sha())
			b.server.txMemPool.ProcessOrphans(tx.Sha())
		}

		for _, stx := range block.STransactions()[0:] {
			b.server.txMemPool.RemoveTransaction(stx, false)
			b.server.txMemPool.RemoveDoubleSpends(stx)
			b.server.txMemPool.RemoveOrphan(stx.Sha())
			b.server.txMemPool.ProcessOrphans(stx.Sha())
		}

		if r := b.server.rpcServer; r != nil {
			// Now that this block is in the blockchain we can mark
			// all the transactions (except the coinbase) as no
			// longer needing rebroadcasting.
			if txTreeRegularValid {
				for _, tx := range parentBlock.Transactions()[1:] {
					iv := wire.NewInvVect(wire.InvTypeTx, tx.Sha())
					b.server.RemoveRebroadcastInventory(iv)
				}
			}
			for _, stx := range block.STransactions()[0:] {
				iv := wire.NewInvVect(wire.InvTypeTx, stx.Sha())
				b.server.RemoveRebroadcastInventory(iv)
			}

			// Notify registered websocket clients of incoming block.
			r.ntfnMgr.NotifyBlockConnected(block)
		}

		// If we're maintaing the address index, and it is up to date
		// then update it based off this new block.
		if !cfg.NoAddrIndex && b.server.addrIndexer.IsCaughtUp() {
			err := b.server.addrIndexer.InsertBlock(block, parentBlock)
			if err != nil {
				bmgrLog.Errorf("AddrIndexManager error: %v", err.Error())
			}
		}

	// Stake tickets are spent or missed from the most recently connected block.
	case blockchain.NTSpentAndMissedTickets:
		tnd, ok := notification.Data.(*blockchain.TicketNotificationsData)
		if !ok {
			bmgrLog.Warnf("Tickets connected notification is not " +
				"TicketNotificationsData")
			break
		}

		if r := b.server.rpcServer; r != nil {
			r.ntfnMgr.NotifySpentAndMissedTickets(tnd)
		}

	// Stake tickets are matured from the most recently connected block.
	case blockchain.NTNewTickets:
		tnd, ok := notification.Data.(*blockchain.TicketNotificationsData)
		if !ok {
			bmgrLog.Warnf("Tickets connected notification is not " +
				"TicketNotificationsData")
			break
		}

		if r := b.server.rpcServer; r != nil {
			r.ntfnMgr.NotifyNewTickets(tnd)
		}

	// A block has been disconnected from the main block chain.
	case blockchain.NTBlockDisconnected:
		blockSlice, ok := notification.Data.([]*dcrutil.Block)
		if !ok {
			bmgrLog.Warnf("Chain disconnected notification is not a block slice.")
			break
		}

		if len(blockSlice) != 2 {
			bmgrLog.Warnf("Chain disconnected notification is wrong size slice.")
			break
		}

		block := blockSlice[0]
		parentBlock := blockSlice[1]

		// If the parent tx tree was invalidated, we need to remove these
		// tx from the mempool as the next incoming block may alternatively
		// validate them.
		txTreeRegularValid := dcrutil.IsFlagSet16(block.MsgBlock().Header.VoteBits,
			dcrutil.BlockValid)

		if !txTreeRegularValid {
			for _, tx := range parentBlock.Transactions()[1:] {
				b.server.txMemPool.RemoveTransaction(tx, false)
				b.server.txMemPool.RemoveDoubleSpends(tx)
				b.server.txMemPool.RemoveOrphan(tx.Sha())
				b.server.txMemPool.ProcessOrphans(tx.Sha())
			}
		}

		// Reinsert all of the transactions (except the coinbase) from the parent
		// tx tree regular into the transaction pool.
		for _, tx := range parentBlock.Transactions()[1:] {
			_, err := b.server.txMemPool.MaybeAcceptTransaction(tx, false, true)
			if err != nil {
				// Remove the transaction and all transactions
				// that depend on it if it wasn't accepted into
				// the transaction pool.
				b.server.txMemPool.RemoveTransaction(tx, true)
			}
		}

		for _, tx := range block.STransactions()[0:] {
			_, err := b.server.txMemPool.MaybeAcceptTransaction(tx, false, true)
			if err != nil {
				// Remove the transaction and all transactions
				// that depend on it if it wasn't accepted into
				// the transaction pool.
				b.server.txMemPool.RemoveTransaction(tx, true)
			}
		}

		// Notify registered websocket clients.
		if r := b.server.rpcServer; r != nil {
			r.ntfnMgr.NotifyBlockDisconnected(block)
		}

		// If we're maintaing the address index, and it is up to date
		// then update it based off this removed block.
		if !cfg.NoAddrIndex && b.server.addrIndexer.IsCaughtUp() {
			err := b.server.addrIndexer.RemoveBlock(block, parentBlock)
			if err != nil {
				bmgrLog.Errorf("AddrIndexManager error: %v", err.Error())
			}
		}

	// The blockchain is reorganizing.
	case blockchain.NTReorganization:
		rd, ok := notification.Data.(*blockchain.ReorganizationNtfnsData)
		if !ok {
			bmgrLog.Warnf("Chain reorganization notification is malformed")
			break
		}

		// Notify registered websocket clients.
		if r := b.server.rpcServer; r != nil {
			r.ntfnMgr.NotifyReorganization(rd)
		}
	}
}

// NewPeer informs the block manager of a newly active peer.
func (b *blockManager) NewPeer(p *peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&b.shutdown) != 0 {
		return
	}

	b.msgChan <- &newPeerMsg{peer: p}
}

// QueueTx adds the passed transaction message and peer to the block handling
// queue.
func (b *blockManager) QueueTx(tx *dcrutil.Tx, p *peer) {
	// Don't accept more transactions if we're shutting down.
	if atomic.LoadInt32(&b.shutdown) != 0 {
		p.txProcessed <- struct{}{}
		return
	}

	b.msgChan <- &txMsg{tx: tx, peer: p}
}

// QueueBlock adds the passed block message and peer to the block handling queue.
func (b *blockManager) QueueBlock(block *dcrutil.Block, p *peer) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&b.shutdown) != 0 {
		p.blockProcessed <- struct{}{}
		return
	}

	b.msgChan <- &blockMsg{block: block, peer: p}
}

// QueueInv adds the passed inv message and peer to the block handling queue.
func (b *blockManager) QueueInv(inv *wire.MsgInv, p *peer) {
	// No channel handling here because peers do not need to block on inv
	// messages.
	if atomic.LoadInt32(&b.shutdown) != 0 {
		return
	}

	b.msgChan <- &invMsg{inv: inv, peer: p}
}

// QueueHeaders adds the passed headers message and peer to the block handling
// queue.
func (b *blockManager) QueueHeaders(headers *wire.MsgHeaders, p *peer) {
	// No channel handling here because peers do not need to block on
	// headers messages.
	if atomic.LoadInt32(&b.shutdown) != 0 {
		return
	}

	b.msgChan <- &headersMsg{headers: headers, peer: p}
}

// DonePeer informs the blockmanager that a peer has disconnected.
func (b *blockManager) DonePeer(p *peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&b.shutdown) != 0 {
		return
	}

	b.msgChan <- &donePeerMsg{peer: p}
}

// Start begins the core block handler which processes block and inv messages.
func (b *blockManager) Start() {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return
	}

	bmgrLog.Trace("Starting block manager")
	b.wg.Add(1)
	go b.blockHandler()
}

// Stop gracefully shuts down the block manager by stopping all asynchronous
// handlers and waiting for them to finish.
func (b *blockManager) Stop() error {
	if atomic.AddInt32(&b.shutdown, 1) != 1 {
		bmgrLog.Warnf("Block manager is already in the process of " +
			"shutting down")
		return nil
	}

	bmgrLog.Infof("Block manager shutting down")
	close(b.quit)
	b.wg.Wait()
	return nil
}

// SyncPeer returns the current sync peer.
func (b *blockManager) SyncPeer() *peer {
	reply := make(chan *peer)
	b.msgChan <- getSyncPeerMsg{reply: reply}
	return <-reply
}

// RequestFromPeer allows an outside caller to request blocks or transactions
// from a peer. The requests are logged in the blockmanager's internal map of
// requests so they do not later ban the peer for sending the respective data.
func (b *blockManager) RequestFromPeer(p *peer, blocks, txs []*chainhash.Hash) error {
	reply := make(chan requestFromPeerResponse)
	b.msgChan <- requestFromPeerMsg{peer: p, blocks: blocks, txs: txs,
		reply: reply}
	response := <-reply

	return response.err
}

func (b *blockManager) requestFromPeer(p *peer, blocks, txs []*chainhash.Hash) error {
	msgResp := wire.NewMsgGetData()

	// Add the blocks to the request.
	for _, bh := range blocks {
		// If we've already requested this block, skip it.
		_, alreadyReqP := p.requestedBlocks[*bh]
		_, alreadyReqB := b.requestedBlocks[*bh]

		if alreadyReqP || alreadyReqB {
			continue
		}

		// Check to see if we already have this block, too.
		// If so, skip.
		exists, err := b.blockChain.HaveBlock(bh)
		if err != nil {
			return err
		}
		if exists {
			continue
		}

		err = msgResp.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, bh))
		if err != nil {
			return fmt.Errorf("unexpected error encountered building request "+
				"for mining state block %v: %v",
				bh, err.Error())
		}

		p.requestedBlocks[*bh] = struct{}{}
		b.requestedBlocks[*bh] = struct{}{}
		b.requestedEverBlocks[*bh] = 0
	}

	// Add the vote transactions to the request.
	for _, vh := range txs {
		// If we've already requested this transaction, skip it.
		_, alreadyReqP := p.requestedTxns[*vh]
		_, alreadyReqB := b.requestedTxns[*vh]

		if alreadyReqP || alreadyReqB {
			continue
		}

		// Ask the transaction memory pool if the transaction is known
		// to it in any form (main pool or orphan).
		if b.server.txMemPool.HaveTransaction(vh) {
			continue
		}

		// Check if the transaction exists from the point of view of the
		// end of the main chain.
		exists, err := b.server.db.ExistsTxSha(vh)
		if err != nil {
			return err
		}
		if exists {
			continue
		}

		err = msgResp.AddInvVect(wire.NewInvVect(wire.InvTypeTx, vh))
		if err != nil {
			return fmt.Errorf("unexpected error encountered building request "+
				"for mining state vote %v: %v",
				vh, err.Error())
		}

		p.requestedTxns[*vh] = struct{}{}
		b.requestedTxns[*vh] = struct{}{}
		b.requestedEverTxns[*vh] = 0
	}

	p.QueueMessage(msgResp, nil)

	return nil
}

// CheckConnectBlock performs several checks to confirm connecting the passed
// block to the main chain does not violate any rules.  This function makes use
// of CheckConnectBlock on an internal instance of a block chain.  It is funneled
// through the block manager since blockchain is not safe for concurrent access.
func (b *blockManager) CheckConnectBlock(block *dcrutil.Block) error {
	reply := make(chan error)
	b.msgChan <- checkConnectBlockMsg{block: block, reply: reply}
	return <-reply
}

// CalcNextRequiredDifficulty calculates the required difficulty for the next
// block after the current main chain.  This function makes use of
// CalcNextRequiredDifficulty on an internal instance of a block chain.  It is
// funneled through the block manager since blockchain is not safe for concurrent
// access.
func (b *blockManager) CalcNextRequiredDifficulty(timestamp time.Time) (uint32, error) {
	reply := make(chan calcNextReqDifficultyResponse)
	b.msgChan <- calcNextReqDifficultyMsg{timestamp: timestamp, reply: reply}
	response := <-reply
	return response.difficulty, response.err
}

// CalcNextRequiredDiffNode calculates the required difficulty for the next
// block after the passed block hash.  This function makes use of
// CalcNextRequiredDiffFromNode on an internal instance of a block chain.  It is
// funneled through the block manager since blockchain is not safe for concurrent
// access.
func (b *blockManager) CalcNextRequiredDiffNode(hash *chainhash.Hash,
	timestamp time.Time) (uint32, error) {
	reply := make(chan calcNextReqDifficultyResponse)
	b.msgChan <- calcNextReqDiffNodeMsg{
		hash:      hash,
		timestamp: timestamp,
		reply:     reply,
	}
	response := <-reply
	return response.difficulty, response.err
}

// CalcNextRequiredStakeDifficulty calculates the required Stake difficulty for
// the next block after the current main chain.  This function makes use of
// CalcNextRequiredStakeDifficulty on an internal instance of a block chain.  It is
// funneled through the block manager since blockchain is not safe for concurrent
// access.
func (b *blockManager) CalcNextRequiredStakeDifficulty() (int64, error) {
	reply := make(chan calcNextReqStakeDifficultyResponse)
	b.msgChan <- calcNextReqStakeDifficultyMsg{reply: reply}
	response := <-reply
	return response.stakeDifficulty, response.err
}

// ForceReorganization returns the hashes of all the children of a parent for the
// block hash that is passed to the function. It is funneled through the block
// manager since blockchain is not safe for concurrent access.
func (b *blockManager) ForceReorganization(formerBest, newBest chainhash.Hash) error {
	reply := make(chan forceReorganizationResponse)
	b.msgChan <- forceReorganizationMsg{
		formerBest: formerBest,
		newBest:    newBest,
		reply:      reply}
	response := <-reply
	return response.err
}

// GetGeneration returns the hashes of all the children of a parent for the
// block hash that is passed to the function. It is funneled through the block
// manager since blockchain is not safe for concurrent access.
func (b *blockManager) GetGeneration(h chainhash.Hash) ([]chainhash.Hash, error) {
	reply := make(chan getGenerationResponse)
	b.msgChan <- getGenerationMsg{hash: h, reply: reply}
	response := <-reply
	return response.hashes, response.err
}

// GetBlockFromHash returns a block for some hash from the block manager, so
// long as the block exists. It is funneled through the block manager since
// blockchain is not safe for concurrent access.
func (b *blockManager) GetBlockFromHash(h chainhash.Hash) (*dcrutil.Block, error) {
	reply := make(chan getBlockFromHashResponse)
	b.msgChan <- getBlockFromHashMsg{hash: h, reply: reply}
	response := <-reply
	return response.block, response.err
}

// GetLotteryData returns the hashes of all the winning tickets for a given
// orphan block along with the pool size and the final state. It is funneled
// through the block manager since blockchain is not safe for concurrent access.
func (b *blockManager) GetLotteryData(hash chainhash.Hash) ([]chainhash.Hash,
	uint32, [6]byte, error) {
	reply := make(chan getLotterDataResponse)
	b.msgChan <- getLotteryDataMsg{
		hash:  hash,
		reply: reply}
	response := <-reply
	return response.winningTickets, response.poolSize, response.finalState,
		response.err
}

// GetTopBlockFromChain obtains the current top block from HEAD of the blockchain.
// Returns a pointer to the cached copy of the block in memory.
func (b *blockManager) GetTopBlockFromChain() (*dcrutil.Block, error) {
	reply := make(chan getTopBlockResponse)
	b.msgChan <- getTopBlockMsg{reply: reply}
	response := <-reply
	return &response.block, response.err
}

// ProcessBlock makes use of ProcessBlock on an internal instance of a block
// chain.  It is funneled through the block manager since blockchain is not safe
// for concurrent access.
func (b *blockManager) ProcessBlock(block *dcrutil.Block,
	flags blockchain.BehaviorFlags) (bool, error) {
	reply := make(chan processBlockResponse, 1)
	b.msgChan <- processBlockMsg{block: block, flags: flags, reply: reply}
	response := <-reply
	return response.isOrphan, response.err
}

// ProcessTransaction makes use of ProcessTransaction on an internal instance of
// a block chain.  It is funneled through the block manager since blockchain is
// not safe for concurrent access.
func (b *blockManager) ProcessTransaction(tx *dcrutil.Tx, allowOrphans bool,
	rateLimit bool) error {
	reply := make(chan processTransactionResponse, 1)
	b.msgChan <- processTransactionMsg{tx, allowOrphans, rateLimit, reply}
	response := <-reply
	return response.err
}

// FetchTransactionStore makes use of FetchTransactionStore on an internal
// instance of a block chain. It is safe for concurrent access.
func (b *blockManager) FetchTransactionStore(tx *dcrutil.Tx,
	isTreeValid bool) (blockchain.TxStore, error) {
	reply := make(chan fetchTransactionStoreResponse, 1)
	b.msgChan <- fetchTransactionStoreMsg{tx: tx,
		isTreeValid: isTreeValid,
		reply:       reply}
	response := <-reply
	return response.TxStore, response.err
}

// IsCurrent returns whether or not the block manager believes it is synced with
// the connected peers.
func (b *blockManager) IsCurrent() bool {
	reply := make(chan bool)
	b.msgChan <- isCurrentMsg{reply: reply}
	return <-reply
}

// MissedTickets returns a slice of missed ticket hashes.
func (b *blockManager) MissedTickets() (stake.SStxMemMap, error) {
	reply := make(chan missedTicketsResponse)
	b.msgChan <- missedTicketsMsg{reply: reply}
	response := <-reply
	return response.Tickets, response.err
}

// Pause pauses the block manager until the returned channel is closed.
//
// Note that while paused, all peer and block processing is halted.  The
// message sender should avoid pausing the block manager for long durations.
func (b *blockManager) Pause() chan<- struct{} {
	c := make(chan struct{})
	b.msgChan <- pauseMsg{c}
	return c
}

// TicketsForAddress returns a list of ticket hashes owned by the address.
func (b *blockManager) TicketsForAddress(address dcrutil.Address) (
	[]chainhash.Hash, error) {
	reply := make(chan ticketsForAddressResponse)
	b.msgChan <- ticketsForAddressMsg{Address: address, reply: reply}
	response := <-reply
	return response.Tickets, response.err
}

// ExistsLiveTicket returns whether or not a ticket exists in the live tickets
// database.
func (b *blockManager) ExistsLiveTicket(hash *chainhash.Hash) (bool, error) {
	reply := make(chan existsLiveTicketResponse)
	b.msgChan <- existsLiveTicketMsg{hash: hash, reply: reply}
	response := <-reply
	return response.Exists, response.err
}

// ExistsLiveTickets returns whether or not tickets in a slice of tickets exist
// in the live tickets database.
func (b *blockManager) ExistsLiveTickets(hashes []*chainhash.Hash) ([]bool, error) {
	reply := make(chan existsLiveTicketsResponse)
	b.msgChan <- existsLiveTicketsMsg{hashes: hashes, reply: reply}
	response := <-reply
	return response.Exists, response.err
}

// GetCurrentTemplate gets the current block template for mining.
func (b *blockManager) GetCurrentTemplate() *BlockTemplate {
	reply := make(chan getCurrentTemplateResponse)
	b.msgChan <- getCurrentTemplateMsg{reply: reply}
	response := <-reply
	return response.Template
}

// SetCurrentTemplate sets the current block template for mining.
func (b *blockManager) SetCurrentTemplate(bt *BlockTemplate) {
	reply := make(chan setCurrentTemplateResponse)
	b.msgChan <- setCurrentTemplateMsg{Template: bt, reply: reply}
	<-reply
	return
}

// GetParentTemplate gets the current parent block template for mining.
func (b *blockManager) GetParentTemplate() *BlockTemplate {
	reply := make(chan getParentTemplateResponse)
	b.msgChan <- getParentTemplateMsg{reply: reply}
	response := <-reply
	return response.Template
}

// SetParentTemplate sets the current parent block template for mining.
func (b *blockManager) SetParentTemplate(bt *BlockTemplate) {
	reply := make(chan setParentTemplateResponse)
	b.msgChan <- setParentTemplateMsg{Template: bt, reply: reply}
	<-reply
	return
}

// newBlockManager returns a new decred block manager.
// Use Start to begin processing asynchronous block and inv updates.
func newBlockManager(s *server) (*blockManager, error) {
	newestHash, height, err := s.db.NewestSha()
	if err != nil {
		return nil, err
	}

	bm := blockManager{
		server:              s,
		requestedTxns:       make(map[chainhash.Hash]struct{}),
		requestedEverTxns:   make(map[chainhash.Hash]uint8),
		requestedBlocks:     make(map[chainhash.Hash]struct{}),
		requestedEverBlocks: make(map[chainhash.Hash]uint8),
		progressLogger:      newBlockProgressLogger("Processed", bmgrLog),
		lastBlockLogTime:    time.Now(),
		msgChan:             make(chan interface{}, cfg.MaxPeers*3),
		headerList:          list.New(),
		AggressiveMining:    !cfg.NonAggressive,
		quit:                make(chan struct{}),
	}
	bm.progressLogger = newBlockProgressLogger("Processed", bmgrLog)
	bm.blockChain = blockchain.New(s.db, s.tmdb, s.chainParams, bm.handleNotifyMsg)
	bm.blockChain.DisableCheckpoints(cfg.DisableCheckpoints)
	if !cfg.DisableCheckpoints {
		// Initialize the next checkpoint based on the current height.
		bm.nextCheckpoint = bm.findNextHeaderCheckpoint(height)
		if bm.nextCheckpoint != nil {
			bm.resetHeaderState(newestHash, height)
		}
	} else {
		bmgrLog.Info("Checkpoints are disabled")
	}

	bmgrLog.Infof("Generating initial block node index.  This may " +
		"take a while...")
	err = bm.blockChain.GenerateInitialIndex()
	if err != nil {
		return nil, err
	}
	bmgrLog.Infof("Block index generation complete")

	// Initialize the chain state now that the intial block node index has
	// been generated.

	// Query the DB for the current winning ticket data.
	wt, ps, fs, err := bm.blockChain.GetWinningTickets(*newestHash)
	if err != nil {
		return nil, err
	}

	// Query the DB for the currently missed tickets.
	missedTickets := bm.blockChain.GetMissedTickets()
	if err != nil && height >= bm.server.chainParams.StakeValidationHeight {
		return nil, err
	}

	// Retrieve the current block header.
	curBlockHeader := bm.blockChain.GetCurrentBlockHeader()

	bm.updateChainState(newestHash,
		height,
		fs,
		uint32(ps),
		wt,
		missedTickets,
		curBlockHeader)

	bm.blockLotteryDataCacheMutex = new(sync.Mutex)
	bm.blockLotteryDataCache = make(map[chainhash.Hash]*BlockLotteryData)

	return &bm, nil
}

// dbPath returns the path to the block database given a database type.
func blockDbPath(dbType string) string {
	// The database name is based on the database type.
	dbName := blockDbNamePrefix + "_" + dbType
	if dbType == "sqlite" {
		dbName = dbName + ".db"
	}
	dbPath := filepath.Join(cfg.DataDir, dbName)
	return dbPath
}

// warnMultipleDBs shows a warning if multiple block database types are detected.
// This is not a situation most users want.  It is handy for development however
// to support multiple side-by-side databases.
func warnMultipleDBs() {
	// This is intentionally not using the known db types which depend
	// on the database types compiled into the binary since we want to
	// detect legacy db types as well.
	dbTypes := []string{"leveldb", "sqlite"}
	duplicateDbPaths := make([]string, 0, len(dbTypes)-1)
	for _, dbType := range dbTypes {
		if dbType == cfg.DbType {
			continue
		}

		// Store db path as a duplicate db if it exists.
		dbPath := blockDbPath(dbType)
		if fileExists(dbPath) {
			duplicateDbPaths = append(duplicateDbPaths, dbPath)
		}
	}

	// Warn if there are extra databases.
	if len(duplicateDbPaths) > 0 {
		selectedDbPath := blockDbPath(cfg.DbType)
		dcrdLog.Warnf("WARNING: There are multiple block chain databases "+
			"using different database types.\nYou probably don't "+
			"want to waste disk space by having more than one.\n"+
			"Your current database is located at [%v].\nThe "+
			"additional database is located at %v", selectedDbPath,
			duplicateDbPaths)
	}
}

// setupBlockDB loads (or creates when needed) the block database taking into
// account the selected database backend.  It also contains additional logic
// such warning the user if there are multiple databases which consume space on
// the file system and ensuring the regression test database is clean when in
// regression test mode.
func setupBlockDB() (dcrdb.Db, error) {
	// The memdb backend does not have a file path associated with it, so
	// handle it uniquely.  We also don't want to worry about the multiple
	// database type warnings when running with the memory database.
	if cfg.DbType == "memdb" {
		dcrdLog.Infof("Creating block database in memory.")
		database, err := dcrdb.CreateDB(cfg.DbType)
		if err != nil {
			return nil, err
		}
		return database, nil
	}

	warnMultipleDBs()

	// The database name is based on the database type.
	dbPath := blockDbPath(cfg.DbType)

	dcrdLog.Infof("Loading block database from '%s'", dbPath)
	database, err := dcrdb.OpenDB(cfg.DbType, dbPath)
	if err != nil {
		// Return the error if it's not because the database
		// doesn't exist.
		if err != dcrdb.ErrDbDoesNotExist {
			return nil, err
		}

		// Create the db if it does not exist.
		err = os.MkdirAll(cfg.DataDir, 0700)
		if err != nil {
			return nil, err
		}
		database, err = dcrdb.CreateDB(cfg.DbType, dbPath)
		if err != nil {
			return nil, err
		}
	}

	return database, nil
}

// dumpBlockChain dumps a map of the blockchain blocks as serialized bytes.
func dumpBlockChain(height int64, db dcrdb.Db) error {
	blockchain := make(map[int64][]byte)
	for i := int64(0); i <= height; i++ {
		// Fetch blocks and put them in the map
		sha, err := db.FetchBlockShaByHeight(i)
		if err != nil {
			return err
		}

		block, err := db.FetchBlockBySha(sha)
		if err != nil {
			return err
		}

		blockBytes, err := block.Bytes()
		if err != nil {
			return err
		}
		blockchain[i] = blockBytes
	}

	// Serialize the map into a buffer
	w := new(bytes.Buffer)
	encoder := gob.NewEncoder(w)
	if err := encoder.Encode(blockchain); err != nil {
		return err
	}

	// Write the buffer to disk
	err := ioutil.WriteFile(cfg.DumpBlockchain, w.Bytes(), 0664)
	if err != nil {
		return err
	}

	return nil
}

// loadBlockDB opens the block database and returns a handle to it.
func loadBlockDB() (dcrdb.Db, error) {
	db, err := setupBlockDB()
	if err != nil {
		return nil, err
	}

	// Get the latest block height from the db.
	_, height, err := db.NewestSha()
	if err != nil {
		db.Close()
		return nil, err
	}

	// Insert the appropriate genesis block for the decred network being
	// connected to if needed.
	if height == -1 {
		genesis := dcrutil.NewBlock(activeNetParams.GenesisBlock)
		genesis.SetHeight(int64(0))
		_, err := db.InsertBlock(genesis)
		if err != nil {
			db.Close()
			return nil, err
		}
		dcrdLog.Infof("Inserted genesis block %v",
			activeNetParams.GenesisHash)
		height = 0
	}

	dcrdLog.Infof("Block database loaded with block height %d", height)

	if cfg.DumpBlockchain != "" {
		dumpBlockChain(height, db)
		return nil, errors.New("Block database dump to map completed, closing.")
	}

	return db, nil
}

// loadTicketDB opens the ticket database and returns a handle to it.
func loadTicketDB(db dcrdb.Db,
	chainParams *chaincfg.Params) (*stake.TicketDB, error) {
	path := cfg.DataDir
	filename := filepath.Join(path, "ticketdb.gob")

	// Check to see if the tmdb exists on disk.
	tmdbExists := true
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		tmdbExists = false
	}

	var tmdb stake.TicketDB

	if !tmdbExists {
		// Load a blank copy of the ticket database and sync it.
		tmdb.Initialize(chainParams, db)

		// Get the latest block height from the db.
		_, curHeight, err := db.NewestSha()
		if err != nil {
			return nil, err
		}
		dcrdLog.Infof("Block ticket database initialized empty")

		if curHeight > 0 {
			dcrdLog.Infof("Db non-empty, resyncing ticket DB")
			err := tmdb.RescanTicketDB()

			if err != nil {
				return nil, err
			}
		}
		return &tmdb, nil
	}
	dcrdLog.Infof("Loading ticket database from disk")
	err := tmdb.LoadTicketDBs(path,
		"ticketdb.gob",
		chainParams,
		db)

	if err != nil {
		return nil, err
	}
	dcrdLog.Infof("Ticket DB loaded with top block height %v",
		tmdb.GetTopBlock())

	return &tmdb, nil
}
