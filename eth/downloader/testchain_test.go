// Copyright 2018 The go-ethereum Authors
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

package downloader

import (
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/consensus/ethash"
	"github.com/ledgerwatch/turbo-geth/core"
	"github.com/ledgerwatch/turbo-geth/core/types"
	"github.com/ledgerwatch/turbo-geth/crypto"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/params"
)

// Test getChain() parameters.
var (
	testKey, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	testAddress = crypto.PubkeyToAddress(testKey.PublicKey)
	testDb      = ethdb.NewMemDatabase()
	testGenesis = core.GenesisBlockForTesting(testDb, testAddress, big.NewInt(1000000000))
)

// The common prefix of all test chains:
var testChainBaseMu sync.Mutex
var testChainBase *testChain

// Different forks on top of the base getChain():
var testChainForkLightA, testChainForkLightB, testChainForkHeavy *testChain
var forkLen = int(fullMaxForkAncestry + 50)

func getTestChainForkLightA() *testChain {
	testChainBaseMu.Lock()
	defer testChainBaseMu.Unlock()
	if testChainForkLightA == nil {
		_ = getTestChainBase()
		testChainForkLightA = getTestChainBase().makeFork(forkLen, false, 1)
	}
	return testChainForkLightA
}
func getTestChainForkLightB() *testChain {
	testChainBaseMu.Lock()
	defer testChainBaseMu.Unlock()
	if testChainForkLightB == nil {
		_ = getTestChainBase()
		testChainForkLightB = getTestChainBase().makeFork(forkLen, false, 2)
	}
	return testChainForkLightB
}
func getTestChainForkHeavy() *testChain {
	testChainBaseMu.Lock()
	defer testChainBaseMu.Unlock()
	if testChainForkHeavy == nil {
		_ = getTestChainBase()
		testChainForkHeavy = getTestChainBase().makeFork(forkLen+1, true, 3)
	}
	return testChainForkHeavy
}
func getTestChainBase() *testChain {
	testChainBaseMu.Lock()
	defer testChainBaseMu.Unlock()
	if testChainBase == nil {
		testChainBase = newTestChain(OverwriteBlockCacheItems+200, testDb, testGenesis)
	}
	return testChainBase
}

func TestMain(m *testing.M) {
	result := m.Run()

	// teardown
	testDb.Close()

	os.Exit(result)
}

type testChain struct {
	db       *ethdb.ObjectDatabase
	genesis  *types.Block
	chain    []common.Hash
	headerm  map[common.Hash]*types.Header
	blockm   map[common.Hash]*types.Block
	receiptm map[common.Hash][]*types.Receipt
	tdm      map[common.Hash]*big.Int
	cpyLock  sync.Mutex
}

// newTestChain creates a blockchain of the given length.
func newTestChain(length int, db *ethdb.ObjectDatabase, genesis *types.Block) *testChain {
	tc := new(testChain).copy(length)
	tc.db = db
	tc.genesis = genesis
	tc.chain = append(tc.chain, genesis.Hash())
	tc.headerm[tc.genesis.Hash()] = tc.genesis.Header()
	tc.tdm[tc.genesis.Hash()] = tc.genesis.Difficulty()
	tc.blockm[tc.genesis.Hash()] = tc.genesis
	tc.generate(length-1, 0, genesis, false)
	return tc
}

// makeFork creates a fork on top of the test getChain().
func (tc *testChain) makeFork(length int, heavy bool, seed byte) *testChain {
	fork := tc.copy(tc.len() + length)
	fork.generate(length, seed, tc.headBlock(), heavy)
	return fork
}

// shorten creates a copy of the getChain() with the given length. It panics if the
// length is longer than the number of available blocks.
func (tc *testChain) shorten(length int) *testChain {
	if length > tc.len() {
		panic(fmt.Errorf("can't shorten test chain to %d blocks, it's only %d blocks long", length, tc.len()))
	}
	return tc.copy(length)
}

func (tc *testChain) copy(newlen int) *testChain {
	tc.cpyLock.Lock()
	defer tc.cpyLock.Unlock()
	cpy := &testChain{
		genesis:  tc.genesis,
		db:       tc.db,
		headerm:  make(map[common.Hash]*types.Header, newlen),
		blockm:   make(map[common.Hash]*types.Block, newlen),
		receiptm: make(map[common.Hash][]*types.Receipt, newlen),
		tdm:      make(map[common.Hash]*big.Int, newlen),
	}
	for i := 0; i < len(tc.chain) && i < newlen; i++ {
		hash := tc.chain[i]
		cpy.chain = append(cpy.chain, tc.chain[i])
		cpy.tdm[hash] = tc.tdm[hash]
		cpy.blockm[hash] = tc.blockm[hash]
		cpy.headerm[hash] = tc.headerm[hash]
		cpy.receiptm[hash] = tc.receiptm[hash]
	}
	return cpy
}

// generate creates a getChain() of n blocks starting at and including parent.
// the returned hash getChain() is ordered head->parent. In addition, every 22th block
// contains a transaction and every 5th an uncle to allow testing correct block
// reassembly.
func (tc *testChain) generate(n int, seed byte, parent *types.Block, heavy bool) {
	// start := time.Now()
	// defer func() { fmt.Printf("test chain generated in %v\n", time.Since(start)) }()
	tc.cpyLock.Lock()
	defer tc.cpyLock.Unlock()

	existingLen := len(tc.chain) - 1
	blocks, receipts, err := core.GenerateChain(params.TestChainConfig, tc.genesis, ethash.NewFaker(), tc.db, existingLen+n, func(i int, block *core.BlockGen) {
		if i < existingLen || existingLen == 0 {
			block.SetCoinbase(common.Address{0})
			// Include transactions to the miner to make blocks more interesting.
			if i%22 == 0 {
				signer := types.MakeSigner(params.TestChainConfig, block.Number())
				tx, err := types.SignTx(types.NewTransaction(block.TxNonce(testAddress), common.Address{0}, uint256.NewInt().SetUint64(1000), params.TxGas, nil, nil), signer, testKey)
				if err != nil {
					panic(err)
				}
				block.AddTx(tx)
			}
			// if the block number is a multiple of 5, add a bonus uncle to the block
			if i > 0 && i%5 == 0 {
				block.AddUncle(&types.Header{
					ParentHash: block.PrevBlock(i - 1).Hash(),
					Number:     big.NewInt(block.Number().Int64() - 1),
				})
			}
		} else {
			block.SetCoinbase(common.Address{seed})
			// If a heavy chain is requested, delay blocks to raise difficulty
			if heavy {
				block.OffsetTime(-1)
			}
			// if the block number is a multiple of 5, add a bonus uncle to the block
			if i > existingLen && (i-existingLen)%5 == 0 {
				block.AddUncle(&types.Header{
					ParentHash: block.PrevBlock(i - existingLen - 1).Hash(),
					Number:     big.NewInt(block.Number().Int64() - 1),
				})
			}
		}
	}, false /* intermediateHashes */)
	if err != nil {
		panic(err)
	}

	// Convert the block-chain into a hash-chain and header/block maps
	td := new(big.Int).Set(tc.td(parent.Hash()))
	for i, b := range blocks[existingLen:] {
		td := td.Add(td, b.Difficulty())
		hash := b.Hash()
		tc.chain = append(tc.chain, hash)
		tc.blockm[hash] = b
		tc.headerm[hash] = b.Header()
		tc.receiptm[hash] = receipts[existingLen+i]
		tc.tdm[hash] = new(big.Int).Set(td)
	}
}

// len returns the total number of blocks in the getChain().
func (tc *testChain) len() int {
	return len(tc.chain)
}

// headBlock returns the head of the getChain().
func (tc *testChain) headBlock() *types.Block {
	return tc.blockm[tc.chain[len(tc.chain)-1]]
}

// td returns the total difficulty of the given block.
func (tc *testChain) td(hash common.Hash) *big.Int {
	return tc.tdm[hash]
}

// headersByHash returns headers in ascending order from the given hash.
func (tc *testChain) headersByHash(origin common.Hash, amount int, skip int) []*types.Header {
	num, _ := tc.hashToNumber(origin)
	return tc.headersByNumber(num, amount, skip)
}

// headersByNumber returns headers in ascending order from the given number.
func (tc *testChain) headersByNumber(origin uint64, amount int, skip int) []*types.Header {
	result := make([]*types.Header, 0, amount)
	for num := origin; num < uint64(len(tc.chain)) && len(result) < amount; num += uint64(skip) + 1 {
		if header, ok := tc.headerm[tc.chain[int(num)]]; ok {
			result = append(result, header)
		}
	}
	return result
}

// receipts returns the receipts of the given block hashes.
func (tc *testChain) receipts(hashes []common.Hash) [][]*types.Receipt {
	results := make([][]*types.Receipt, 0, len(hashes))
	for _, hash := range hashes {
		if receipt, ok := tc.receiptm[hash]; ok {
			results = append(results, receipt)
		}
	}
	return results
}

// bodies returns the block bodies of the given block hashes.
func (tc *testChain) bodies(hashes []common.Hash) ([][]*types.Transaction, [][]*types.Header) {
	transactions := make([][]*types.Transaction, 0, len(hashes))
	uncles := make([][]*types.Header, 0, len(hashes))
	for _, hash := range hashes {
		if block, ok := tc.blockm[hash]; ok {
			transactions = append(transactions, block.Transactions())
			uncles = append(uncles, block.Uncles())
		}
	}
	return transactions, uncles
}

func (tc *testChain) hashToNumber(target common.Hash) (uint64, bool) {
	for num, hash := range tc.chain {
		if hash == target {
			return uint64(num), true
		}
	}
	return 0, false
}
