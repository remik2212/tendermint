package v1

import (
	"context"
	"sync/atomic"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/libs/clist"
	"github.com/tendermint/tendermint/libs/log"
	tmsync "github.com/tendermint/tendermint/libs/sync"
	"github.com/tendermint/tendermint/mempool"
	"github.com/tendermint/tendermint/proxy"
	"github.com/tendermint/tendermint/types"
)

var _ mempool.Mempool = (*TxMempool)(nil)

// TxMempoolOption sets an optional parameter on the TxMempool.
type TxMempoolOption func(*TxMempool)

// TxMempool defines a prioritized mempool data structure used by the v1 mempool
// reactor. It keeps a thread-safe priority queue of transactions that is used
// when a block proposer constructs a block and a thread-safe linked-list that
// is used to gossip transactions to peers in a FIFO manner.
type TxMempool struct {
	logger       log.Logger
	metrics      *mempool.Metrics
	config       *config.MempoolConfig
	proxyAppConn proxy.AppConnMempool

	// txsAvailable fires once for each height when the mempool is not empty
	txsAvailable         chan struct{}
	notifiedTxsAvailable bool

	// height defines the last block height process during Update()
	height int64

	// sizeBytes defines the total size of the mempool (sum of all tx bytes)
	sizeBytes int64

	// txsMap defines the main storage of valid transactions. Indexes are built
	// on top of this map.
	txsMap *TxMap

	// cache defines a fixed-size cache of already seen transactions as this
	// reduces pressure on the proxyApp.
	cache mempool.TxCache

	// gossipIndex defines the gossiping index of valid transactions
	gossipIndex *clist.CList

	// priorityIndex defines the priority index of valid transactions
	priorityIndex *TxPriorityQueue

	// A read/write lock is used to safe guard updates, insertions and deletions
	// from the mempool. A read-lock is implicitly acquired when executing CheckTx,
	// however, a caller must explicitly grab a write-lock via Lock when updating
	// the mempool via Update().
	mtx       tmsync.RWMutex
	preCheck  mempool.PreCheckFunc
	postCheck mempool.PostCheckFunc
}

func NewTxMempool(
	logger log.Logger,
	cfg *config.MempoolConfig,
	proxyAppConn proxy.AppConnMempool,
	height int64,
	options ...TxMempoolOption,
) *TxMempool {

	mp := &TxMempool{
		logger:        logger,
		config:        cfg,
		proxyAppConn:  proxyAppConn,
		height:        height,
		metrics:       mempool.NopMetrics(),
		gossipIndex:   clist.New(),
		priorityIndex: NewTxPriorityQueue(),
	}

	if cfg.CacheSize > 0 {
		mp.cache = mempool.NewLRUTxCache(cfg.CacheSize)
	} else {
		mp.cache = mempool.NopTxCache{}
	}

	// TODO:
	// proxyAppConn.SetResponseCallback(mp.globalCb)

	for _, opt := range options {
		opt(mp)
	}

	return mp
}

// EnableTxsAvailable enables the mempool to trigger events when transactions
// are available on a block by block basis.
//
// NOTE: It is NOT thread-safe and should only be called once on startup.
func (txmp *TxMempool) EnableTxsAvailable() {
	txmp.txsAvailable = make(chan struct{}, 1)
}

// WithPreCheck sets a filter for the mempool to reject a transaction if f(tx)
// returns an error. This is executed before CheckTx. It only applies to the
// first created block. After that, Update() overwrites the existing value.
func WithPreCheck(f mempool.PreCheckFunc) TxMempoolOption {
	return func(txmp *TxMempool) { txmp.preCheck = f }
}

// WithPostCheck sets a filter for the mempool to reject a transaction if
// f(tx, resp) returns an error. This is executed after CheckTx. It only applies
// to the first created block. After that, Update overwrites the existing value.
func WithPostCheck(f mempool.PostCheckFunc) TxMempoolOption {
	return func(txmp *TxMempool) { txmp.postCheck = f }
}

// WithMetrics sets the mempool's metrics collector.
func WithMetrics(metrics *mempool.Metrics) TxMempoolOption {
	return func(txmp *TxMempool) { txmp.metrics = metrics }
}

// Lock obtains a write-lock on the mempool. A caller must be sure to explicitly
// release the lock when finished.
func (txmp *TxMempool) Lock() {
	txmp.mtx.Lock()
}

// Unlock releases a write-lock on the mempool.
func (txmp *TxMempool) Unlock() {
	txmp.mtx.Unlock()
}

// Size returns the number of valid transactions in the mempool. It is
// thread-safe and uses the underlying gossip index to infer the total number of
// transactions.
func (txmp *TxMempool) Size() int {
	return txmp.gossipIndex.Len()
}

// SizeBytes return the total sum in bytes of all the valid transactions in the
// mempool. It is thread-safe.
func (txmp *TxMempool) SizeBytes() int64 {
	return atomic.LoadInt64(&txmp.sizeBytes)
}

// FlushAppConn executes FlushSync on the mempool's proxyAppConn.
//
// NOTE: The caller must obtain a write-lock via Lock() prior to execution.
func (txmp *TxMempool) FlushAppConn() error {
	return txmp.proxyAppConn.FlushSync(context.Background())
}

// WaitForNextTx returns a blocking channel that will be closed when the next
// valid transaction is available to gossip. It is thread-safe.
func (txmp *TxMempool) WaitForNextTx() <-chan struct{} {
	return txmp.gossipIndex.WaitChan()
}

// NextGossipTx returns the next valid transaction to gossip. A caller must wait
// for WaitForNextTx to signal a transaction is available to gossip first. It is
// thread-safe.
func (txmp *TxMempool) NextGossipTx() *WrappedTx {
	return txmp.gossipIndex.Front().Value.(*WrappedTx)
}

// TxsAvailable returns a channel which fires once for every height, and only
// when transactions are available in the mempool. It is thread-safe.
func (txmp *TxMempool) TxsAvailable() <-chan struct{} {
	return txmp.txsAvailable
}

func (txmp *TxMempool) CheckTx(tx types.Tx, cb func(*abci.Response), txInfo mempool.TxInfo) error {
	panic("not implemented")
}

func (txmp *TxMempool) Flush() {
	panic("not implemented")
}

func (txmp *TxMempool) ReapMaxBytesMaxGas(maxBytes, maxGas int64) types.Txs {
	panic("not implemented")
}

func (txmp *TxMempool) ReapMaxTxs(max int) types.Txs {
	panic("not implemented")
}

func (txmp *TxMempool) Update(
	blockHeight int64,
	blockTxs types.Txs,
	deliverTxResponses []*abci.ResponseDeliverTx,
	newPreFn mempool.PreCheckFunc,
	newPostFn mempool.PostCheckFunc,
) error {
	panic("not implemented")
}
