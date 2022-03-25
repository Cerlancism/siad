package chainutil

import (
	"time"

	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
)

// FindBlockNonce finds a block nonce meeting the target.
func FindBlockNonce(h *types.BlockHeader, target types.BlockID) {
	// ensure nonce meets factor requirement
	for h.Nonce%consensus.NonceFactor != 0 {
		h.Nonce++
	}
	for !h.ID().MeetsTarget(target) {
		h.Nonce += consensus.NonceFactor
	}
}

// JustHeaders renters only the headers of each block.
func JustHeaders(blocks []types.Block) []types.BlockHeader {
	headers := make([]types.BlockHeader, len(blocks))
	for i := range headers {
		headers[i] = blocks[i].Header
	}
	return headers
}

// JustTransactions returns only the transactions of each block.
func JustTransactions(blocks []types.Block) [][]types.Transaction {
	txns := make([][]types.Transaction, len(blocks))
	for i := range txns {
		txns[i] = blocks[i].Transactions
	}
	return txns
}

// JustTransactionIDs returns only the transaction ids included in each block.
func JustTransactionIDs(blocks []types.Block) [][]types.TransactionID {
	txns := make([][]types.TransactionID, len(blocks))
	for i := range txns {
		txns[i] = make([]types.TransactionID, len(blocks[i].Transactions))
		for j := range txns[i] {
			txns[i][j] = blocks[i].Transactions[j].ID()
		}
	}
	return txns
}

// JustChainIndexes returns only the chain index of each block.
func JustChainIndexes(blocks []types.Block) []types.ChainIndex {
	cis := make([]types.ChainIndex, len(blocks))
	for i := range cis {
		cis[i] = blocks[i].Index()
	}
	return cis
}

// ChainSim represents a simulation of a blockchain.
type ChainSim struct {
	Genesis consensus.Checkpoint
	Chain   []types.Block
	Context consensus.ValidationContext

	nonce uint64 // for distinguishing forks

	// for simulating transactions
	pubkey    types.PublicKey
	privkey   types.PrivateKey
	scOutputs []types.SiacoinElement
	sfOutputs []types.SiafundElement
	contracts []types.FileContractElement
}

// Fork forks the current chain.
func (cs *ChainSim) Fork() *ChainSim {
	cs2 := *cs
	cs2.Chain = append([]types.Block(nil), cs2.Chain...)
	cs2.scOutputs = append([]types.SiacoinElement(nil), cs2.scOutputs...)
	cs.nonce += 1 << 48
	return &cs2
}

//MineBlockWithTxns mine a block with the given transaction.
func (cs *ChainSim) MineBlockWithTxns(txns ...types.Transaction) types.Block {
	prev := cs.Genesis.Block.Header
	if len(cs.Chain) > 0 {
		prev = cs.Chain[len(cs.Chain)-1].Header
	}
	b := types.Block{
		Header: types.BlockHeader{
			Height:       prev.Height + 1,
			ParentID:     prev.ID(),
			Nonce:        cs.nonce,
			Timestamp:    prev.Timestamp.Add(time.Second),
			MinerAddress: types.VoidAddress,
		},
		Transactions: txns,
	}
	b.Header.Commitment = cs.Context.Commitment(b.Header.MinerAddress, b.Transactions)
	FindBlockNonce(&b.Header, types.HashRequiringWork(cs.Context.Difficulty))

	sau := consensus.ApplyBlock(cs.Context, b)
	cs.Context = sau.Context
	cs.Chain = append(cs.Chain, b)

	// update our outputs
	for i := range cs.scOutputs {
		sau.UpdateElementProof(&cs.scOutputs[i].StateElement)
	}
	for _, out := range sau.NewSiacoinElements {
		if out.Address == types.StandardAddress(cs.pubkey) {
			cs.scOutputs = append(cs.scOutputs, out)
		}
	}
	for i := range cs.sfOutputs {
		sau.UpdateElementProof(&cs.sfOutputs[i].StateElement)
	}
	for _, out := range sau.NewSiafundElements {
		if out.Address == types.StandardAddress(cs.pubkey) {
			cs.sfOutputs = append(cs.sfOutputs, out)
		}
	}
	for i := range cs.contracts {
		sau.UpdateElementProof(&cs.contracts[i].StateElement)
	}
	for _, fc := range sau.NewFileContracts {
		if fc.RenterPublicKey == cs.pubkey {
			cs.contracts = append(cs.contracts, fc)
		}
	}

	return b
}

// TxnWithSiacoinOutputs returns a transaction containing the specified outputs.
// The ChainSim must have funds equal to or exceeding the sum of the outputs.
func (cs *ChainSim) TxnWithSiacoinOutputs(scos ...types.SiacoinOutput) types.Transaction {
	txn := types.Transaction{
		SiacoinOutputs: scos,
		MinerFee:       types.NewCurrency64(cs.Context.Index.Height),
	}

	totalOut := txn.MinerFee
	for _, sco := range scos {
		totalOut = totalOut.Add(sco.Value)
	}

	// select inputs and compute change output
	var totalIn types.Currency
	for i, out := range cs.scOutputs {
		txn.SiacoinInputs = append(txn.SiacoinInputs, types.SiacoinInput{
			Parent:      out,
			SpendPolicy: types.PolicyPublicKey(cs.pubkey),
		})
		totalIn = totalIn.Add(out.Value)
		if totalIn.Cmp(totalOut) >= 0 {
			cs.scOutputs = cs.scOutputs[i+1:]
			break
		}
	}

	if totalIn.Cmp(totalOut) < 0 {
		panic("insufficient funds")
	} else if totalIn.Cmp(totalOut) > 0 {
		// add change output
		txn.SiacoinOutputs = append(txn.SiacoinOutputs, types.SiacoinOutput{
			Address: types.StandardAddress(cs.pubkey),
			Value:   totalIn.Sub(totalOut),
		})
	}

	// sign
	sigHash := cs.Context.InputSigHash(txn)
	for i := range txn.SiacoinInputs {
		txn.SiacoinInputs[i].Signatures = []types.Signature{cs.privkey.SignHash(sigHash)}
	}
	return txn
}

// MineBlockWithSiacoinOutputs mines a block with a transaction containing the
// specified siacoin outputs. The ChainSim must have funds equal to or exceeding
// the sum of the outputs.
func (cs *ChainSim) MineBlockWithSiacoinOutputs(scos ...types.SiacoinOutput) types.Block {
	txn := types.Transaction{
		SiacoinOutputs: scos,
		MinerFee:       types.NewCurrency64(cs.Context.Index.Height),
	}

	totalOut := txn.MinerFee
	for _, b := range scos {
		totalOut = totalOut.Add(b.Value)
	}

	// select inputs and compute change output
	var totalIn types.Currency
	for i, out := range cs.scOutputs {
		txn.SiacoinInputs = append(txn.SiacoinInputs, types.SiacoinInput{
			Parent:      out,
			SpendPolicy: types.PolicyPublicKey(cs.pubkey),
		})
		totalIn = totalIn.Add(out.Value)
		if totalIn.Cmp(totalOut) >= 0 {
			cs.scOutputs = cs.scOutputs[i+1:]
			break
		}
	}

	if totalIn.Cmp(totalOut) < 0 {
		panic("insufficient funds")
	} else if totalIn.Cmp(totalOut) > 0 {
		// add change output
		txn.SiacoinOutputs = append(txn.SiacoinOutputs, types.SiacoinOutput{
			Address: types.StandardAddress(cs.pubkey),
			Value:   totalIn.Sub(totalOut),
		})
	}

	// sign and mine
	sigHash := cs.Context.InputSigHash(txn)
	for i := range txn.SiacoinInputs {
		txn.SiacoinInputs[i].Signatures = []types.Signature{cs.privkey.SignHash(sigHash)}
	}
	return cs.MineBlockWithTxns(txn)
}

// MineBlock mine an empty block.
func (cs *ChainSim) MineBlock() types.Block {
	// simulate chain activity by sending our existing outputs to new addresses
	var txns []types.Transaction
	for _, out := range cs.scOutputs {
		txn := types.Transaction{
			SiacoinInputs: []types.SiacoinInput{{
				Parent:      out,
				SpendPolicy: types.PolicyPublicKey(cs.pubkey),
			}},
			SiacoinOutputs: []types.SiacoinOutput{
				{Address: types.StandardAddress(cs.pubkey), Value: out.Value.Sub(types.NewCurrency64(cs.Context.Index.Height + 1))},
				{Address: types.Address{byte(cs.nonce >> 48), byte(cs.nonce >> 56), 1, 2, 3}, Value: types.NewCurrency64(1)},
			},
			MinerFee: types.NewCurrency64(cs.Context.Index.Height),
		}
		sigHash := cs.Context.InputSigHash(txn)
		for i := range txn.SiacoinInputs {
			txn.SiacoinInputs[i].Signatures = []types.Signature{cs.privkey.SignHash(sigHash)}
		}
		txns = append(txns, txn)
	}
	cs.scOutputs = cs.scOutputs[:0]
	for _, out := range cs.sfOutputs {
		txn := types.Transaction{
			SiafundInputs: []types.SiafundInput{{
				Parent:      out,
				SpendPolicy: types.PolicyPublicKey(cs.pubkey),
			}},
			SiafundOutputs: []types.SiafundOutput{
				{Address: types.StandardAddress(cs.pubkey), Value: out.Value - 1},
				{Address: types.Address{byte(cs.nonce >> 48), byte(cs.nonce >> 56), 1, 2, 3}, Value: 1},
			},
		}
		sigHash := cs.Context.InputSigHash(txn)
		for i := range txn.SiafundInputs {
			txn.SiafundInputs[i].Signatures = []types.Signature{cs.privkey.SignHash(sigHash)}
		}
		txns = append(txns, txn)
	}
	cs.sfOutputs = cs.sfOutputs[:0]
	for i, fc := range cs.contracts {
		if (i % 2) == 0 {
			// resolve
			rev := fc.FileContract
			rev.RevisionNumber = types.MaxRevisionNumber
			txn := types.Transaction{
				FileContractResolutions: []types.FileContractResolution{{
					Parent:       fc,
					Finalization: rev,
				}},
			}
			txns = append(txns, txn)
		} else {
			// revise
			rev := fc.FileContract
			rev.Filesize += 1
			txn := types.Transaction{
				FileContractRevisions: []types.FileContractRevision{{
					Parent:   fc,
					Revision: rev,
				}},
			}
			txns = append(txns, txn)
		}
	}
	cs.contracts = cs.contracts[:0]

	return cs.MineBlockWithTxns(txns...)
}

// MineBlocks mine a number of blocks.
func (cs *ChainSim) MineBlocks(n int) []types.Block {
	blocks := make([]types.Block, n)
	for i := range blocks {
		blocks[i] = cs.MineBlock()
	}
	return blocks
}

// NewChainSim returns a new ChainSim useful for simulating forks.
func NewChainSim() *ChainSim {
	// gift ourselves some coins in the genesis block
	privkey := types.NewPrivateKeyFromSeed([32]byte{})
	pubkey := privkey.PublicKey()

	hostPrivkey := types.NewPrivateKeyFromSeed([32]byte{})
	hostPubkey := hostPrivkey.PublicKey()

	ourAddr := types.StandardAddress(pubkey)
	scGift := make([]types.SiacoinOutput, 10)
	for i := range scGift {
		scGift[i] = types.SiacoinOutput{
			Address: ourAddr,
			Value:   types.Siacoins(10 * uint32(i+1)),
		}
	}
	sfGift := make([]types.SiafundOutput, 10)
	for i := range sfGift {
		sfGift[i] = types.SiafundOutput{
			Address: ourAddr,
			Value:   uint64(10 * uint32(i+1)),
		}
	}
	contractGift := make([]types.FileContract, 10)
	for i := range contractGift {
		contractGift[i] = types.FileContract{
			WindowStart: uint64(i + 1),
			WindowEnd:   uint64(i + 10),
			RenterOutput: types.SiacoinOutput{
				Address: types.StandardAddress(pubkey),
				Value:   types.Siacoins(10 * uint32(i+1)),
			},
			HostOutput: types.SiacoinOutput{
				Address: types.StandardAddress(pubkey),
				Value:   types.Siacoins(5 * uint32(i+1)),
			},
			TotalCollateral: types.ZeroCurrency,
			RenterPublicKey: pubkey,
			HostPublicKey:   hostPubkey,
		}
	}

	genesisTxns := []types.Transaction{{SiacoinOutputs: scGift, SiafundOutputs: sfGift, FileContracts: contractGift}}
	genesis := types.Block{
		Header: types.BlockHeader{
			Timestamp: time.Unix(734600000, 0),
		},
		Transactions: genesisTxns,
	}
	sau := consensus.GenesisUpdate(genesis, types.Work{NumHashes: [32]byte{31: 4}})
	var scOutputs []types.SiacoinElement
	for _, out := range sau.NewSiacoinElements {
		if out.Address == types.StandardAddress(pubkey) {
			scOutputs = append(scOutputs, out)
		}
	}
	var sfOutputs []types.SiafundElement
	for _, out := range sau.NewSiafundElements {
		if out.Address == types.StandardAddress(pubkey) {
			sfOutputs = append(sfOutputs, out)
		}
	}
	var contracts []types.FileContractElement
	for _, fc := range sau.NewFileContracts {
		if fc.RenterPublicKey == pubkey {
			contracts = append(contracts, fc)
		}
	}
	return &ChainSim{
		Genesis: consensus.Checkpoint{
			Block:   genesis,
			Context: sau.Context,
		},
		Context:   sau.Context,
		privkey:   privkey,
		pubkey:    pubkey,
		scOutputs: scOutputs,
		sfOutputs: sfOutputs,
	}
}
