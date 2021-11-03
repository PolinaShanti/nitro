//
// Copyright 2021, Offchain Labs, Inc. All rights reserved.
//

package arbtest

import (
	"context"
	"encoding/hex"
	"math/big"
	"math/rand"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/offchainlabs/arbstate/arbstate"
	"github.com/offchainlabs/arbstate/solgen/go/precompilesgen"
	"github.com/offchainlabs/arbstate/util"
	"github.com/offchainlabs/arbstate/util/merkletree"
)

func TestOutboxProofs(t *testing.T) {
	arbstate.RequireHookedGeth()

	arbSysAbi, err := precompilesgen.ArbSysMetaData.GetAbi()
	failOnError(t, err, "failed to get abi")
	withdrawTopic := arbSysAbi.Events["L2ToL1Transaction"].ID
	merkleTopic := arbSysAbi.Events["SendMerkleUpdate"].ID
	arbSysAddress := common.HexToAddress("0x64")

	backend, l2info := CreateTestL2(t)
	client := ClientForArbBackend(t, backend)
	arbSys, err := precompilesgen.NewArbSys(arbSysAddress, client)
	if err != nil {
		t.Fatal(err)
	}
	ownerOps := l2info.GetDefaultTransactOpts("Owner")

	ctx := context.Background()
	txnCount := int64(1 + rand.Intn(128))

	// represents a send we should be able to prove exists
	type proofPair struct {
		hash common.Hash
		leaf uint64
	}

	provables := make([]proofPair, 0)

	txns := []common.Hash{}

	for i := int64(0); i < txnCount; i++ {
		ownerOps.Value = big.NewInt(i * 1000000000)
		ownerOps.Nonce = big.NewInt(i)
		tx, err := arbSys.WithdrawEth(&ownerOps, common.Address{})
		failOnError(t, err, "ArbSys failed")
		txns = append(txns, tx.Hash())
	}

	for _, tx := range txns {
		var receipt *types.Receipt
		for {
			receipt, err = client.TransactionReceipt(ctx, tx)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
			} else {
				break
			}
		}

		failOnError(t, err, "No receipt for txn")

		if receipt.Status != types.ReceiptStatusSuccessful {
			t.Fatal("Tx failed with status code:", receipt)
		}
		if len(receipt.Logs) == 0 {
			t.Fatal("Tx didn't emit any logs")
		}

		for _, log := range receipt.Logs {

			if log.Topics[0] == withdrawTopic {
				parsedLog, err := arbSys.ParseL2ToL1Transaction(*log)
				failOnError(t, err, "Failed to parse log")

				provables = append(provables, proofPair{
					hash: common.BigToHash(parsedLog.Hash),
					leaf: parsedLog.Position.Uint64(),
				})
			}
		}
	}

	merkleState, err := arbSys.SendMerkleTreeState(&bind.CallOpts{})
	failOnError(t, err, "could not get merkle root")
	rootHash := merkleState.Root          // we assume the user knows the root and size
	treeSize := merkleState.Size.Uint64() //

	treeLevels := util.Log2ceil(treeSize)

	t.Log("Tree has", treeSize, "leaves and", treeLevels, "levels")
	t.Log("Root hash", hex.EncodeToString(rootHash[:]))
	t.Log("Will query against topics\n\tmerkle:   ", merkleTopic, "\n\twithdraw: ", withdrawTopic)

	// using only the root and position, we'll prove the send hash exists for each node
	for _, provable := range provables {
		t.Log("Proving leaf", provable.leaf)

		// find which nodes we'll want in our proof
		needs := make([]common.Hash, 0)
		place := uint64(1)
		for level := 0; level < int(treeLevels); level++ {
			sibling := provable.leaf ^ place

			position := new(big.Int).Add(
				new(big.Int).Lsh(big.NewInt(int64(level)), 192),
				big.NewInt(int64(sibling)),
			)

			needs = append(needs, common.BigToHash(position))
			place <<= 1
		}

		// query geth for
		logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{
			Addresses: []common.Address{
				arbSysAddress,
			},
			Topics: [][]common.Hash{
				{merkleTopic, withdrawTopic},
				nil,
				nil,
				needs,
			},
		})
		failOnError(t, err, "couldn't get logs")

		t.Log("Querried for", len(needs), "positions", needs)
		t.Log("Found", len(logs), "logs for proof", provable.leaf, "of", txnCount)

		for _, log := range logs {
			t.Log(log)
		}

		proof := merkletree.MerkleProof{
			RootHash:  rootHash,
			LeafHash:  provable.hash,
			LeafIndex: provable.leaf,
			Proof:     []common.Hash{},
		}

		if !proof.IsCorrect() {
			t.Fatal("Proof is wrong")
		}
	}

	_ = merkleTopic
}

func failOnError(t *testing.T, err error, msg string) {
	if err != nil {
		t.Fatal(msg+":", err)
	}
}
