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

package core

import (
	"bytes"
	"fmt"
	"github.com/usechain/go-usechain/log"
	"math/big"

	"github.com/usechain/go-usechain/common"
	"github.com/usechain/go-usechain/consensus"
	"github.com/usechain/go-usechain/contracts/minerlist"
	"github.com/usechain/go-usechain/core/state"
	"github.com/usechain/go-usechain/core/types"
	"github.com/usechain/go-usechain/crypto"
	"github.com/usechain/go-usechain/params"
)

// BlockValidator is responsible for validating block headers, uncles and
// processed state.
//
// BlockValidator implements Validator.
type BlockValidator struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for validating
}

// NewBlockValidator returns a new block validator which is safe for re-use
func NewBlockValidator(config *params.ChainConfig, blockchain *BlockChain, engine consensus.Engine) *BlockValidator {
	validator := &BlockValidator{
		config: config,
		engine: engine,
		bc:     blockchain,
	}
	return validator
}

// ValidateBody validates the given block's uncles and verifies the the block
// header's transaction and uncle roots. The headers are assumed to be already
// validated at this point.
func (v *BlockValidator) ValidateBody(block *types.Block) error {
	// Check whether the block's known, and if not, that it's linkable
	if v.bc.HasBlockAndState(block.Hash(), block.NumberU64()) {
		return ErrKnownBlock
	}
	if !v.bc.HasBlockAndState(block.ParentHash(), block.NumberU64()-1) {
		if !v.bc.HasBlock(block.ParentHash(), block.NumberU64()-1) {
			return consensus.ErrUnknownAncestor
		}
		return consensus.ErrPrunedAncestor
	}
	// Header validity is known at this point, check the uncles and transactions
	header := block.Header()
	state, _ := v.bc.State()
	if err := v.engine.VerifyUncles(v.bc, block, state); err != nil {
		return err
	}
	if hash := types.CalcUncleHash(block.Uncles()); hash != header.UncleHash {
		return fmt.Errorf("uncle root hash mismatch: have %x, want %x", hash, header.UncleHash)
	}
	if hash := types.DeriveSha(block.Transactions()); hash != header.TxHash {
		return fmt.Errorf("transaction root hash mismatch: have %x, want %x", hash, header.TxHash)
	}
	return nil
}

// ValidateState validates the various changes that happen after a state
// transition, such as amount of used gas, the receipt roots and the state root
// itself. ValidateState returns a database batch if the validation was a success
// otherwise nil and an error is returned.
func (v *BlockValidator) ValidateState(block, parent *types.Block, statedb *state.StateDB, receipts types.Receipts, usedGas uint64) error {
	header := block.Header()
	if block.GasUsed() != usedGas {
		return fmt.Errorf("invalid gas used (remote: %d local: %d)", block.GasUsed(), usedGas)
	}
	// Validate the received block's bloom with the one derived from the generated receipts.
	// For valid blocks this should always validate to true.
	rbloom := types.CreateBloom(receipts)
	if rbloom != header.Bloom {
		return fmt.Errorf("invalid bloom (remote: %x  local: %x)", header.Bloom, rbloom)
	}
	// Tre receipt Trie's root (R = (Tr [[H1, R1], ... [Hn, R1]]))
	receiptSha := types.DeriveSha(receipts)
	if receiptSha != header.ReceiptHash {
		return fmt.Errorf("invalid receipt root hash (remote: %x local: %x)", header.ReceiptHash, receiptSha)
	}
	// Validate the state root against the received state root and throw
	// an error if they don't match.
	if root := statedb.IntermediateRoot(v.config.IsEIP158(header.Number)); header.Root != root {
		return fmt.Errorf("invalid merkle root (remote: %x local: %x)", header.Root, root)
	}
	return nil
}

func (v *BlockValidator) ValidateMiner(block, parent *types.Block, statedb *state.StateDB) error {
	header := block.Header()
	tstampParent := parent.Time()
	tstampHead := header.Time
	tstampSub := new(big.Int).Sub(tstampHead, tstampParent)

	if tstampSub.Int64() < int64(common.BlockInterval) {
		return fmt.Errorf("Block time slot should be more than five seconds")
	}

	totalMinerNum := minerlist.ReadMinerNum(statedb)

	// Verify block miner && verify the minerQrSignature legality
	isMiner, flag := minerlist.IsMiner(statedb, header.Coinbase, totalMinerNum, header.Number)
	if !isMiner {
		if flag == 1 {
			return fmt.Errorf("miner address is being punished, invalid miner")
		} else {
			return fmt.Errorf("miner address needs to register as a miner, invalid miner")
		}
	}

	preCoinbase := parent.Coinbase()
	blockNumber := header.Number
	preQrSignature := parent.MinerQrSignature()
	minerQrSignature := header.MinerQrSignature
	if header.Number.Cmp(common.Big1) == 0 {
		preQrSignature = common.GenesisMinerQrSignature
	}
	n := new(big.Int).Div(tstampSub, common.BlockSlot)
	qr, err := minerlist.CalQrOrIdNext(preCoinbase.Bytes(), blockNumber, preQrSignature)
	if err != nil {
		return err
	}

	if header.Number.Int64() > 1 {
		if len(minerQrSignature) != minerlist.PreQrLength {
			return fmt.Errorf("invalid minerQrSignature length")
		}
		qrtemp := common.BytesToHash(minerQrSignature[65:])
		if qr.String() != qrtemp.String() {
			return fmt.Errorf("invalid minerQrSignature, qr is not correct")
		}

		if !VerifySig(minerQrSignature[:65], qr, header.Coinbase) {
			return fmt.Errorf("invalid minerQrSignature")
		}
	}
	IsValidMiner, level, preMinerid := minerlist.IsValidMiner(statedb, header.Coinbase, preCoinbase, preQrSignature, blockNumber, totalMinerNum, n)
	if !IsValidMiner {
		return fmt.Errorf("invalid miner")
	}

	// Verify PrimaryMiner and DifficultyLevel
	var preMiner common.Address
	if totalMinerNum.Int64() != 0 {
		preMiner = common.BytesToAddress(minerlist.ReadMinerAddress(statedb, preMinerid))
	}
	if bytes.Compare(header.PrimaryMiner.Bytes(), preMiner.Bytes()) != 0 && totalMinerNum.Int64() != 0 {
		return fmt.Errorf("invalid primaryMiner: have %s, want %s", header.PrimaryMiner.String(), preMiner.String())
	}

	if header.Number.Cmp(common.Big1) == 0 {
		if header.DifficultyLevel.Int64() != 0 {
			return fmt.Errorf("invalid difficultyLevel: have %v, want 0", header.DifficultyLevel)
		}
	} else {
		if level > header.DifficultyLevel.Int64() {
			return fmt.Errorf("invalid difficultyLevel: have %v, want %v", header.DifficultyLevel, level)
		}
	}

	return nil
}

// CalcGasLimit computes the gas limit of the next block after parent.
// This is miner strategy, not consensus protocol.
func CalcGasLimit(parent *types.Block) uint64 {
	// contrib = (parentGasUsed * 3 / 2) / 1024
	contrib := (parent.GasUsed() + parent.GasUsed()/2) / params.GasLimitBoundDivisor

	// decay = parentGasLimit / 1024 -1
	decay := parent.GasLimit()/params.GasLimitBoundDivisor - 1

	/*
		strategy: gasLimit of block-to-mine is set based on parent's
		gasUsed value.  if parentGasUsed > parentGasLimit * (2/3) then we
		increase it, otherwise lower it (or leave it unchanged if it's right
		at that usage) the amount increased/decreased depends on how far away
		from parentGasLimit * (2/3) parentGasUsed is.
	*/
	limit := parent.GasLimit() - decay + contrib
	if limit < params.MinGasLimit {
		limit = params.MinGasLimit
	}
	// however, if we're now below the target (TargetGasLimit) we increase the
	// limit as much as we can (parentGasLimit / 1024 -1)
	if limit < params.TargetGasLimit {
		limit = parent.GasLimit() + decay
		if limit > params.TargetGasLimit {
			limit = params.TargetGasLimit
		}
	}
	return limit
}

// verify the qrSignature legality
// need to verify the sig legality and singer must equal to miner
func VerifySig(sig []byte, hash common.Hash, miner common.Address) bool {
	pub, err := crypto.Ecrecover(hash.Bytes(), sig)
	if err != nil {
		log.Error("retrieve public key failed")
		return false
	}
	pubKey := crypto.ToECDSAPub(pub)
	return crypto.VerifySignature(pub, hash.Bytes(), sig[:64]) && (crypto.PubkeyToAddress(*pubKey) == miner)
}
