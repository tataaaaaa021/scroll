package watcher

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/params"
	"gorm.io/gorm"

	"scroll-tech/common/forks"
	"scroll-tech/common/types/encoding"
	"scroll-tech/common/types/encoding/codecv0"
	"scroll-tech/common/types/encoding/codecv1"

	"scroll-tech/rollup/internal/config"
	"scroll-tech/rollup/internal/orm"
)

// BatchProposer proposes batches based on available unbatched chunks.
type BatchProposer struct {
	ctx context.Context
	db  *gorm.DB

	batchOrm   *orm.Batch
	chunkOrm   *orm.Chunk
	l2BlockOrm *orm.L2Block

	maxChunkNumPerBatch             uint64
	maxL1CommitGasPerBatch          uint64
	maxL1CommitCalldataSizePerBatch uint64
	batchTimeoutSec                 uint64
	gasCostIncreaseMultiplier       float64
	forkMap                         map[uint64]bool
	banachForkHeight                uint64

	batchProposerCircleTotal           prometheus.Counter
	proposeBatchFailureTotal           prometheus.Counter
	proposeBatchUpdateInfoTotal        prometheus.Counter
	proposeBatchUpdateInfoFailureTotal prometheus.Counter
	totalL1CommitGas                   prometheus.Gauge
	totalL1CommitCalldataSize          prometheus.Gauge
	totalL1CommitBlobSize              prometheus.Gauge
	batchChunksNum                     prometheus.Gauge
	batchFirstBlockTimeoutReached      prometheus.Counter
	batchChunksProposeNotEnoughTotal   prometheus.Counter
}

type batchMetrics struct {
	// common metrics
	numChunks           uint64
	firstBlockTimestamp uint64

	// codecv0 metrics, default 0 for codecv1
	l1CommitCalldataSize uint64
	l1CommitGas          uint64

	// codecv1 metrics, default 0 for codecv0
	l1CommitBlobSize uint64
}

// NewBatchProposer creates a new BatchProposer instance.
func NewBatchProposer(ctx context.Context, cfg *config.BatchProposerConfig, chainCfg *params.ChainConfig, db *gorm.DB, reg prometheus.Registerer) *BatchProposer {
	forkHeights, forkMap := forks.CollectSortedForkHeights(chainCfg)
	log.Debug("new batch proposer",
		"maxChunkNumPerBatch", cfg.MaxChunkNumPerBatch,
		"maxL1CommitGasPerBatch", cfg.MaxL1CommitGasPerBatch,
		"maxL1CommitCalldataSizePerBatch", cfg.MaxL1CommitCalldataSizePerBatch,
		"batchTimeoutSec", cfg.BatchTimeoutSec,
		"gasCostIncreaseMultiplier", cfg.GasCostIncreaseMultiplier,
		"forkHeights", forkHeights)

	p := &BatchProposer{
		ctx:                             ctx,
		db:                              db,
		batchOrm:                        orm.NewBatch(db),
		chunkOrm:                        orm.NewChunk(db),
		l2BlockOrm:                      orm.NewL2Block(db),
		maxChunkNumPerBatch:             cfg.MaxChunkNumPerBatch,
		maxL1CommitGasPerBatch:          cfg.MaxL1CommitGasPerBatch,
		maxL1CommitCalldataSizePerBatch: cfg.MaxL1CommitCalldataSizePerBatch,
		batchTimeoutSec:                 cfg.BatchTimeoutSec,
		gasCostIncreaseMultiplier:       cfg.GasCostIncreaseMultiplier,
		forkMap:                         forkMap,

		batchProposerCircleTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "rollup_propose_batch_circle_total",
			Help: "Total number of propose batch total.",
		}),
		proposeBatchFailureTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "rollup_propose_batch_failure_circle_total",
			Help: "Total number of propose batch total.",
		}),
		proposeBatchUpdateInfoTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "rollup_propose_batch_update_info_total",
			Help: "Total number of propose batch update info total.",
		}),
		proposeBatchUpdateInfoFailureTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "rollup_propose_batch_update_info_failure_total",
			Help: "Total number of propose batch update info failure total.",
		}),
		totalL1CommitGas: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "rollup_propose_batch_total_l1_commit_gas",
			Help: "The total l1 commit gas",
		}),
		totalL1CommitCalldataSize: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "rollup_propose_batch_total_l1_call_data_size",
			Help: "The total l1 call data size",
		}),
		totalL1CommitBlobSize: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "rollup_propose_batch_total_l1_commit_blob_size",
			Help: "The total l1 commit blob size",
		}),
		batchChunksNum: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "rollup_propose_batch_chunks_number",
			Help: "The number of chunks in the batch",
		}),
		batchFirstBlockTimeoutReached: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "rollup_propose_batch_first_block_timeout_reached_total",
			Help: "Total times of batch's first block timeout reached",
		}),
		batchChunksProposeNotEnoughTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "rollup_propose_batch_chunks_propose_not_enough_total",
			Help: "Total number of batch chunk propose not enough",
		}),
	}

	// If BanachBlock is not set in chain's genesis config, banachForkHeight is inf,
	// which means chunk proposer uses the codecv0 version by default.
	// TODO: Must change it to real fork name.
	if chainCfg.BanachBlock != nil {
		p.banachForkHeight = chainCfg.BanachBlock.Uint64()
	} else {
		p.banachForkHeight = math.MaxUint64
	}
	return p
}

// TryProposeBatch tries to propose a new batches.
func (p *BatchProposer) TryProposeBatch() {
	p.batchProposerCircleTotal.Inc()
	batch, err := p.proposeBatch()
	if err != nil {
		p.proposeBatchFailureTotal.Inc()
		log.Error("proposeBatchChunks failed", "err", err)
		return
	}
	if batch == nil {
		return
	}
	err = p.db.Transaction(func(dbTX *gorm.DB) error {
		batch, dbErr := p.batchOrm.InsertBatch(p.ctx, batch, dbTX)
		if dbErr != nil {
			log.Warn("BatchProposer.updateBatchInfoInDB insert batch failure",
				"start chunk index", batch.StartChunkIndex, "end chunk index", batch.EndChunkIndex, "error", dbErr)
			return dbErr
		}
		dbErr = p.chunkOrm.UpdateBatchHashInRange(p.ctx, batch.StartChunkIndex, batch.EndChunkIndex, batch.Hash, dbTX)
		if dbErr != nil {
			log.Warn("BatchProposer.UpdateBatchHashInRange update the chunk's batch hash failure", "hash", batch.Hash, "error", dbErr)
			return dbErr
		}
		return nil
	})
	if err != nil {
		p.proposeBatchUpdateInfoFailureTotal.Inc()
		log.Error("update batch info in db failed", "err", err)
	}
}

func (p *BatchProposer) proposeBatch() (*encoding.Batch, error) {
	unbatchedChunkIndex, err := p.batchOrm.GetFirstUnbatchedChunkIndex(p.ctx)
	if err != nil {
		return nil, err
	}

	// select at most p.maxChunkNumPerBatch chunks
	dbChunks, err := p.chunkOrm.GetChunksGEIndex(p.ctx, unbatchedChunkIndex, int(p.maxChunkNumPerBatch))
	if err != nil {
		return nil, err
	}

	if len(dbChunks) == 0 {
		return nil, nil
	}

	maxChunksThisBatch := p.maxChunkNumPerBatch
	for i, chunk := range dbChunks {
		// if a chunk is starting at a fork boundary, only consider earlier chunks
		if i != 0 && p.forkMap[chunk.StartBlockNumber] {
			dbChunks = dbChunks[:i]
			if uint64(len(dbChunks)) < maxChunksThisBatch {
				maxChunksThisBatch = uint64(len(dbChunks))
			}
			break
		}
	}

	daChunks, err := p.getDAChunks(dbChunks)
	if err != nil {
		return nil, err
	}

	parentDBBatch, err := p.batchOrm.GetLatestBatch(p.ctx)
	if err != nil {
		return nil, err
	}

	var batch encoding.Batch
	if parentDBBatch != nil { // TODO: remove this check, return error when nil.
		batch.Index = parentDBBatch.Index + 1
		var parentDABatch *codecv0.DABatch
		parentDABatch, err = codecv0.NewDABatchFromBytes(parentDBBatch.BatchHeader)
		if err != nil {
			return nil, err
		}
		batch.TotalL1MessagePoppedBefore = parentDABatch.TotalL1MessagePopped
		batch.ParentBatchHash = common.HexToHash(parentDBBatch.Hash)
	}

	for i, chunk := range daChunks {
		batch.Chunks = append(batch.Chunks, chunk)
		metrics, calcErr := p.calculateBatchMetrics(&batch)
		if calcErr != nil {
			return nil, fmt.Errorf("failed to calculate batch metrics: %w", calcErr)
		}
		totalOverEstimateL1CommitGas := uint64(p.gasCostIncreaseMultiplier * float64(metrics.l1CommitGas))
		if metrics.l1CommitCalldataSize > p.maxL1CommitCalldataSizePerBatch ||
			totalOverEstimateL1CommitGas > p.maxL1CommitGasPerBatch ||
			metrics.l1CommitBlobSize > maxBlobSize {
			if i == 0 {
				// The first chunk exceeds hard limits, which indicates a bug in the chunk-proposer, manual fix is needed.
				return nil, fmt.Errorf(
					"the first chunk exceeds limits; start block number: %v, end block number: %v, limits: %+v, maxChunkNum: %v, maxL1CommitCalldataSize: %v, maxL1CommitGas: %v, maxBlobSize: %v",
					dbChunks[0].StartBlockNumber, dbChunks[0].EndBlockNumber, metrics, p.maxChunkNumPerBatch, p.maxL1CommitCalldataSizePerBatch, p.maxL1CommitGasPerBatch, maxBlobSize)
			}

			log.Debug("breaking limit condition in batching",
				"currentL1CommitCalldataSize", metrics.l1CommitCalldataSize,
				"maxL1CommitCalldataSizePerBatch", p.maxL1CommitCalldataSizePerBatch,
				"currentOverEstimateL1CommitGas", totalOverEstimateL1CommitGas,
				"maxL1CommitGasPerBatch", p.maxL1CommitGasPerBatch)

			batch.Chunks = batch.Chunks[:len(batch.Chunks)-1]

			metrics, err := p.calculateBatchMetrics(&batch)
			if err != nil {
				return nil, fmt.Errorf("failed to calculate batch metrics: %w", err)
			}

			p.recordBatchMetrics(metrics)
			return &batch, nil
		}
	}

	metrics, calcErr := p.calculateBatchMetrics(&batch)
	if calcErr != nil {
		return nil, fmt.Errorf("failed to calculate batch metrics: %w", calcErr)
	}
	currentTimeSec := uint64(time.Now().Unix())
	if metrics.firstBlockTimestamp+p.batchTimeoutSec < currentTimeSec || metrics.numChunks == maxChunksThisBatch {
		log.Info("reached maximum number of chunks in batch or first block timeout",
			"chunk count", metrics.numChunks,
			"start block number", dbChunks[0].StartBlockNumber,
			"start block timestamp", dbChunks[0].StartBlockTime,
			"current time", currentTimeSec)

		p.batchFirstBlockTimeoutReached.Inc()
		p.recordBatchMetrics(metrics)
		return &batch, nil
	}

	log.Debug("pending chunks do not reach one of the constraints or contain a timeout block")
	p.batchChunksProposeNotEnoughTotal.Inc()
	return nil, nil
}

func (p *BatchProposer) getDAChunks(dbChunks []*orm.Chunk) ([]*encoding.Chunk, error) {
	chunks := make([]*encoding.Chunk, len(dbChunks))
	for i, c := range dbChunks {
		blocks, err := p.l2BlockOrm.GetL2BlocksInRange(p.ctx, c.StartBlockNumber, c.EndBlockNumber)
		if err != nil {
			log.Error("Failed to fetch blocks", "start number", c.StartBlockNumber, "end number", c.EndBlockNumber, "error", err)
			return nil, err
		}
		chunks[i] = &encoding.Chunk{
			Blocks: blocks,
		}
	}
	return chunks, nil
}

func (p *BatchProposer) recordBatchMetrics(metrics *batchMetrics) {
	p.totalL1CommitGas.Set(float64(metrics.l1CommitGas))
	p.totalL1CommitCalldataSize.Set(float64(metrics.l1CommitCalldataSize))
	p.batchChunksNum.Set(float64(metrics.numChunks))
	p.totalL1CommitBlobSize.Set(float64(metrics.l1CommitBlobSize))
}

func (p *BatchProposer) calculateBatchMetrics(batch *encoding.Batch) (*batchMetrics, error) {
	var err error
	metrics := &batchMetrics{}
	metrics.numChunks = uint64(len(batch.Chunks))
	metrics.firstBlockTimestamp = batch.Chunks[0].Blocks[0].Header.Time
	firstBlockNum := batch.Chunks[0].Blocks[0].Header.Number.Uint64()
	if firstBlockNum >= p.banachForkHeight { // codecv1
		metrics.l1CommitBlobSize, err = codecv1.EstimateBatchL1CommitBlobSize(batch)
		if err != nil {
			return metrics, fmt.Errorf("failed to estimate chunk L1 commit blob size: %w", err)
		}
	} else { // codecv0
		metrics.l1CommitGas, err = codecv0.EstimateBatchL1CommitGas(batch)
		if err != nil {
			return nil, fmt.Errorf("failed to estimate batch L1 commit gas: %w", err)
		}
		metrics.l1CommitCalldataSize, err = codecv0.EstimateBatchL1CommitCalldataSize(batch)
		if err != nil {
			return nil, fmt.Errorf("failed to estimate batch L1 commit calldata size: %w", err)
		}
	}
	return metrics, nil
}
