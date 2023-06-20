package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ledgerwatch/erigon-lib/chain"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv/memdb"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/consensus/ethash"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/params"
)

func postLondonConfig() *chain.Config {
	config := *params.TestChainConfig
	config.LondonBlock = big.NewInt(0)
	return &config
}

func preLondonConfig() *chain.Config {
	config := *params.TestChainConfig
	config.LondonBlock = nil
	return &config
}

func TestSelfDestructGasPreEIP3529(t *testing.T) {
	bytecode := []byte{
		byte(vm.PC),
		byte(vm.SELFDESTRUCT),
	}

	// Expected gas is (intrinsic +  pc + cold load (due to legacy tx)  + selfdestruct cost ) / 2
	// The refund of 24000 gas (i.e. params.SelfdestructRefundGas) is not applied since refunds pre-EIP3529 are
	// capped to half of the transaction's gas.
	expectedGasUsed := (params.TxGas + vm.GasQuickStep + params.ColdAccountAccessCostEIP2929 + params.SelfdestructGasEIP150) / 2
	testGasUsage(t, preLondonConfig(), ethash.NewFaker(), bytecode, nil, 60_000, expectedGasUsed)
}

func TestSstoreGasPreEIP3529(t *testing.T) {
	bytecode := []byte{
		byte(vm.PUSH1), 0x3, // value
		byte(vm.PUSH1), 0x3, // location
		byte(vm.SSTORE), // Set slot[3] = 3
	}
	// 	// Expected gas is intrinsic +  2*pushGas + cold load (due to legacy tx) +  SstoreGas
	expectedGasUsed := params.TxGas + 2*vm.GasFastestStep + params.ColdSloadCostEIP2929 + params.SstoreSetGasEIP2200
	testGasUsage(t, preLondonConfig(), ethash.NewFaker(), bytecode, nil, 60_000, expectedGasUsed)
}

func TestSelfDestructGasPostEIP3529(t *testing.T) {
	bytecode := []byte{
		byte(vm.PC),
		byte(vm.SELFDESTRUCT),
	}
	// Expected gas is intrinsic +  pc + cold load (due to legacy tx) + SelfDestructGas
	expectedGasUsed := params.TxGas + vm.GasQuickStep + params.ColdAccountAccessCostEIP2929 + params.SelfdestructGasEIP150
	testGasUsage(t, postLondonConfig(), ethash.NewFaker(), bytecode, nil, 60_000, expectedGasUsed)
}

func TestSstoreGasPostEIP3529(t *testing.T) {
	bytecode := []byte{
		byte(vm.PUSH1), 0x3, // value
		byte(vm.PUSH1), 0x3, // location
		byte(vm.SSTORE), // Set slot[3] = 3
	}
	// Expected gas is intrinsic +  2*pushGas + cold load (due to legacy tx) +  SstoreGas
	expectedGasUsed := params.TxGas + 2*vm.GasFastestStep + params.ColdSloadCostEIP2929 + params.SstoreSetGasEIP2200
	testGasUsage(t, postLondonConfig(), ethash.NewFaker(), bytecode, nil, 60_000, expectedGasUsed)
}

func TestSstoreModifyGasPostEIP3529(t *testing.T) {
	bytecode := []byte{
		byte(vm.PUSH1), 0x3, // value
		byte(vm.PUSH1), 0x1, // location
		byte(vm.SSTORE), // Set slot[1] = 3
	}
	// initialize contract storage
	initialStorage := make(map[libcommon.Hash]libcommon.Hash)
	// Populate two slots
	initialStorage[libcommon.HexToHash("01")] = libcommon.HexToHash("01")
	initialStorage[libcommon.HexToHash("02")] = libcommon.HexToHash("02")
	// Expected gas is intrinsic +  2*pushGas + cold load (due to legacy tx) + SstoreReset (a->b such that a!=0)
	expectedGasUsed := params.TxGas + 2*vm.GasFastestStep + params.ColdSloadCostEIP2929 + (params.SstoreResetGasEIP2200 - params.ColdSloadCostEIP2929)
	testGasUsage(t, postLondonConfig(), ethash.NewFaker(), bytecode, initialStorage, 60_000, expectedGasUsed)
}

func TestSstoreClearGasPostEIP3529(t *testing.T) {
	bytecode := []byte{
		byte(vm.PUSH1), 0x0, // value
		byte(vm.PUSH1), 0x1, // location
		byte(vm.SSTORE), // Set slot[1] = 0
	}
	// initialize contract storage
	initialStorage := make(map[libcommon.Hash]libcommon.Hash)
	// Populate two slots
	initialStorage[libcommon.HexToHash("01")] = libcommon.HexToHash("01")
	initialStorage[libcommon.HexToHash("02")] = libcommon.HexToHash("02")

	// Expected gas is intrinsic +  2*pushGas + cold load (due to legacy tx) + SstoreReset (a->b such that a!=0) - sstoreClearGasRefund
	expectedGasUsage := params.TxGas + 2*vm.GasFastestStep + params.ColdSloadCostEIP2929 + (params.SstoreResetGasEIP2200 - params.ColdSloadCostEIP2929) - params.SstoreClearsScheduleRefundEIP3529
	testGasUsage(t, postLondonConfig(), ethash.NewFaker(), bytecode, initialStorage, 60_000, expectedGasUsage)
}

// Test the gas used by running a transaction sent to a smart contract with given bytecode and storage.
func testGasUsage(t *testing.T, config *chain.Config, engine consensus.Engine, bytecode []byte, initialStorage map[libcommon.Hash]libcommon.Hash, initialGas, expectedGasUsed uint64) {
	var (
		aa = libcommon.HexToAddress("0x000000000000000000000000000000000000aaaa")

		// Generate a canonical chain to act as the main dataset
		db = memdb.NewTestDB(t)

		// A sender who makes transactions, has some funds
		key, _        = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address       = crypto.PubkeyToAddress(key.PublicKey)
		balanceBefore = big.NewInt(1000000000000000)
		gspec         = &types.Genesis{Config: params.TestChainConfig, Alloc: core.GenesisAlloc{
			address: {Balance: balanceBefore},
			aa: {
				Code:    bytecode,
				Storage: initialStorage,
				Nonce:   0,
				Balance: big.NewInt(0),
			},
		}}
		genesis = gspec.MustCommit(db)
	)

	blocks, _ := GenerateChain(gspec.Config, genesis, engine, db, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})

		// One transaction to 0xAAAA
		signer := types.LatestSigner(gspec.Config)
		tx, _ := types.SignNewTx(key, signer, &types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Gas:      initialGas,
			GasPrice: newGwei(5),
		})
		b.AddTx(tx)
	})

	// Import the canonical chain
	diskdb := rawdb.NewMemoryDatabase()
	gspec.MustCommit(diskdb)

	chain, err := NewBlockChain(diskdb, nil, gspec.Config, engine, vm.Config{}, nil, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block := chain.GetBlockByNumber(1)

	if block.GasUsed() != expectedGasUsed {
		t.Fatalf("incorrect amount of gas spent: expected %d, got %d", expectedGasUsed, block.GasUsed())
	}
}
