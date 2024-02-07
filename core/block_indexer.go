package core

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/blinklabs-io/gouroboros/ledger"
	"github.com/blinklabs-io/gouroboros/protocol/chainsync"
	"github.com/blinklabs-io/gouroboros/protocol/common"
	"github.com/hashicorp/go-hclog"
)

const (
	AddressCheckNone    = 0               // No flags
	AddressCheckInputs  = 1 << (iota - 1) // 1 << 0 = 0x00...0001 = 1
	AddressCheckOutputs                   // 1 << 1 = 0x00...0010 = 2
	AddressCheckAll     = AddressCheckInputs | AddressCheckOutputs
)

type BlockIndexerConfig struct {
	StartingBlockPoint *BlockPoint `json:"startingBlockPoint"`

	// how many children blocks is needed for some block to be considered final
	ConfirmationBlockCount uint `json:"confirmationBlockCount"`

	AddressesOfInterest []string `json:"addressesOfInterest"`

	KeepAllTxOutputsInDb bool `json:"keepAllTxOutputsInDb"`

	AddressCheck int `json:"addressCheck"`
}

type NewConfirmedBlockHandler func(*FullBlock) error

type blockWithLazyTxRetriever struct {
	header *BlockHeader
	getTxs GetTxsFunc
}

type BlockIndexer struct {
	config *BlockIndexerConfig

	// latest confirmed and saved block point
	latestBlockPoint *BlockPoint

	newConfirmedBlockHandler NewConfirmedBlockHandler
	unconfirmedBlocks        []blockWithLazyTxRetriever

	db                  BlockIndexerDb
	addressesOfInterest map[string]bool

	logger hclog.Logger
}

var _ BlockSyncerHandler = (*BlockIndexer)(nil)

func NewBlockIndexer(config *BlockIndexerConfig, newConfirmedBlockHandler NewConfirmedBlockHandler, db BlockIndexerDb, logger hclog.Logger) *BlockIndexer {
	if config.AddressCheck&AddressCheckAll == 0 {
		panic("block indexer must at least check outputs or inputs") //nolint:gocritic
	}

	addressesOfInterest := make(map[string]bool, len(config.AddressesOfInterest))
	for _, x := range config.AddressesOfInterest {
		addressesOfInterest[x] = true
	}

	return &BlockIndexer{
		config: config,

		latestBlockPoint: nil,

		newConfirmedBlockHandler: newConfirmedBlockHandler,
		unconfirmedBlocks:        nil,

		db:                  db,
		addressesOfInterest: addressesOfInterest,
		logger:              logger,
	}
}

func (bi *BlockIndexer) RollBackwardFunc(point common.Point, tip chainsync.Tip) error {
	// linear is ok, there will be smaller number of unconfirmed blocks in memory
	for i := len(bi.unconfirmedBlocks) - 1; i >= 0; i-- {
		unc := bi.unconfirmedBlocks[i]
		if unc.header.BlockSlot == point.Slot && bytes.Equal(unc.header.BlockHash, point.Hash) {
			bi.unconfirmedBlocks = bi.unconfirmedBlocks[:i+1]

			return nil
		}
	}

	if bi.latestBlockPoint.BlockSlot == point.Slot && bytes.Equal(bi.latestBlockPoint.BlockHash, point.Hash) {
		bi.unconfirmedBlocks = nil

		// everything is ok -> we are reverting to the latest confirmed block
		return nil
	}

	// we have confirmed some block that should not be confirmed!!!! TODO: what to do in this case?
	return errors.Join(errBlockSyncerFatal, fmt.Errorf("roll backward, block not found = (%d, %s)", point.Slot, hex.EncodeToString(point.Hash)))
}

func (bi *BlockIndexer) RollForwardFunc(blockHeader *BlockHeader, getTxsFunc GetTxsFunc, tip chainsync.Tip) error {
	if uint(len(bi.unconfirmedBlocks)) < bi.config.ConfirmationBlockCount {
		// If there are not enough children blocks to promote the first one to the confirmed state, a new block header is added, and the function returns
		bi.unconfirmedBlocks = append(bi.unconfirmedBlocks, blockWithLazyTxRetriever{
			header: blockHeader,
			getTxs: getTxsFunc,
		})

		return nil
	}

	confirmedBlock := bi.unconfirmedBlocks[0]

	txs, err := confirmedBlock.getTxs()
	if err != nil {
		return err
	}

	fullBlock, latestBlockPoint, err := bi.processConfirmedBlock(confirmedBlock.header, txs)
	if err != nil {
		return err
	}

	// update latest block point in memory if we have confirmed block
	bi.latestBlockPoint = latestBlockPoint
	// remove first block from unconfirmed list. copy whole list because we do not want memory leak
	bi.unconfirmedBlocks = append(append([]blockWithLazyTxRetriever(nil), bi.unconfirmedBlocks[1:]...), blockWithLazyTxRetriever{
		header: blockHeader,
		getTxs: getTxsFunc,
	})

	// notify listener if needed
	if fullBlock != nil {
		bi.newConfirmedBlockHandler(fullBlock)
	}

	return nil
}

func (bi *BlockIndexer) NextBlockNumber() uint64 {
	if len(bi.unconfirmedBlocks) > 0 {
		return bi.unconfirmedBlocks[len(bi.unconfirmedBlocks)-1].header.BlockNumber + 1
	}

	return bi.latestBlockPoint.BlockNumber + 1
}

func (bi *BlockIndexer) SyncBlockPoint() (BlockPoint, error) {
	var err error

	if bi.latestBlockPoint == nil {
		// read from database
		bi.latestBlockPoint, err = bi.db.GetLatestBlockPoint()
		if err != nil {
			return BlockPoint{}, err
		}

		// if there is nothing in database read from default config
		if bi.latestBlockPoint == nil {
			bi.latestBlockPoint = bi.config.StartingBlockPoint
		}

		if bi.latestBlockPoint == nil {
			bi.latestBlockPoint = &BlockPoint{
				BlockSlot:   0,
				BlockNumber: math.MaxUint64,
				BlockHash:   nil,
			}
		}
	}

	return *bi.latestBlockPoint, nil
}

func (bi *BlockIndexer) processConfirmedBlock(confirmedBlockHeader *BlockHeader, allBlockTransactions []ledger.Transaction) (*FullBlock, *BlockPoint, error) {
	if confirmedBlockHeader == nil {
		return nil, bi.latestBlockPoint, nil
	}

	var (
		fullBlock         *FullBlock = nil
		txOutputsToSave   []*TxInputOutput
		txOutputsToRemove []*TxInput

		dbTx = bi.db.OpenTx() // open database tx
	)

	// get all transactions of interest from block
	txsOfInterest, err := bi.getTxsOfInterest(allBlockTransactions)
	if err != nil {
		return nil, nil, err
	}

	if bi.config.KeepAllTxOutputsInDb {
		txOutputsToSave = bi.getAllTxOutputs(allBlockTransactions)
		txOutputsToRemove = bi.getAllTxInputs(allBlockTransactions)
	} else {
		txOutputsToSave = bi.getTxOutputsOfInterest(txsOfInterest)
		txOutputsToRemove = bi.getTxInputs(txsOfInterest)
	}

	// add confirmed block to db and create full block only if there are some transactions of interest
	if len(txsOfInterest) > 0 {
		fullBlock = NewFullBlock(confirmedBlockHeader, txsOfInterest)
		dbTx.AddConfirmedBlock(fullBlock) // add confirmed block in db tx (dbTx implementation should handle nil case)
	}

	latestBlockPoint := &BlockPoint{
		BlockSlot:   confirmedBlockHeader.BlockSlot,
		BlockHash:   confirmedBlockHeader.BlockHash,
		BlockNumber: confirmedBlockHeader.BlockNumber,
	}
	dbTx.SetLatestBlockPoint(latestBlockPoint)                            // update latest block point in db tx
	dbTx.AddTxOutputs(txOutputsToSave).RemoveTxOutputs(txOutputsToRemove) // add all needed outputs, remove used ones in db tx

	// update database -> execute db transaction
	if err := dbTx.Execute(); err != nil {
		return nil, nil, err
	}

	return fullBlock, latestBlockPoint, nil
}

func (bi *BlockIndexer) getTxsOfInterest(txs []ledger.Transaction) (result []*Tx, err error) {
	if len(bi.addressesOfInterest) == 0 {
		return NewTransactions(txs), nil
	}

	for _, tx := range txs {
		if bi.isTxOutputOfInterest(tx) {
			result = append(result, NewTransaction(tx))
		} else {
			txIsGood, err := bi.isTxInputOfInterest(tx)
			if err != nil {
				return nil, err
			} else if txIsGood {
				result = append(result, NewTransaction(tx))
			}
		}
	}

	return result, nil
}

func (bi *BlockIndexer) isTxOutputOfInterest(tx ledger.Transaction) bool {
	if bi.config.AddressCheck&AddressCheckOutputs == 0 {
		return false
	}

	for _, out := range tx.Outputs() {
		address := out.Address().String()
		if bi.addressesOfInterest[address] {
			return true
		}
	}

	return false
}

func (bi *BlockIndexer) isTxInputOfInterest(tx ledger.Transaction) (bool, error) {
	if bi.config.AddressCheck&AddressCheckInputs == 0 {
		return false, nil
	}

	for _, inp := range tx.Inputs() {
		txOutput, err := bi.db.GetTxOutput(TxInput{
			Hash:  inp.Id().String(),
			Index: inp.Index(),
		})
		if err != nil {
			return false, err
		} else if txOutput != nil && bi.addressesOfInterest[txOutput.Address] {
			return true, nil
		}
	}

	return false, nil
}

func (bi *BlockIndexer) getTxOutputsOfInterest(txs []*Tx) (res []*TxInputOutput) {
	// return empty slice if we do not check inputs
	if bi.config.AddressCheck&AddressCheckInputs == 0 {
		return nil
	}

	for _, tx := range txs {
		for ind, txOut := range tx.Outputs {
			if bi.addressesOfInterest[txOut.Address] {
				res = append(res, &TxInputOutput{
					Input: &TxInput{
						Hash:  tx.Hash,
						Index: uint32(ind),
					},
					Output: txOut,
				})
			}
		}
	}

	return res
}

func (bi *BlockIndexer) getAllTxOutputs(txs []ledger.Transaction) (res []*TxInputOutput) {
	for _, tx := range txs {
		for ind, txOut := range tx.Outputs() {
			res = append(res, &TxInputOutput{
				Input: &TxInput{
					Hash:  tx.Hash(),
					Index: uint32(ind),
				},
				Output: &TxOutput{
					Address: txOut.Address().String(),
					Amount:  txOut.Amount(),
				},
			})
		}
	}

	return res
}

func (bi *BlockIndexer) getTxInputs(txs []*Tx) (res []*TxInput) {
	// return empty slice if we do not check inputs
	if bi.config.AddressCheck&AddressCheckInputs == 0 {
		return nil
	}

	for _, tx := range txs {
		res = append(res, tx.Inputs...)
	}

	return res
}

func (bi *BlockIndexer) getAllTxInputs(txs []ledger.Transaction) (res []*TxInput) {
	for _, tx := range txs {
		for _, inp := range tx.Inputs() {
			res = append(res, &TxInput{
				Hash:  inp.Id().String(),
				Index: inp.Index(),
			})
		}
	}

	return res
}
