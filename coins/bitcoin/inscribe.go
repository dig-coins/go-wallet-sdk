package bitcoin

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type InscriptionData struct {
	ContentType string `json:"contentType"`
	Body        []byte `json:"body"`
	RevealAddr  string `json:"revealAddr"`
}

type PrevOutput struct {
	TxId       string `json:"txId"`
	VOut       uint32 `json:"vOut"`
	Amount     int64  `json:"amount"`
	Address    string `json:"address"`
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
}

type InscriptionRequest struct {
	CommitTxPrevOutputList []*PrevOutput     `json:"commitTxPrevOutputList"`
	CommitFeeRate          int64             `json:"commitFeeRate"`
	RevealFeeRate          int64             `json:"revealFeeRate"`
	InscriptionDataList    []InscriptionData `json:"inscriptionDataList"`
	RevealOutValue         int64             `json:"revealOutValue"`
	ChangeAddress          string            `json:"changeAddress"`
	MinChangeValue         int64             `json:"minChangeValue"`
}

type inscriptionTxCtxData struct {
	PrivateKey              *btcec.PrivateKey
	InscriptionScript       []byte
	CommitTxAddress         string
	CommitTxAddressPkScript []byte
	ControlBlockWitness     []byte
	RevealTxPrevOutput      *wire.TxOut
}

type InscriptionBuilder struct {
	Network                   *chaincfg.Params
	CommitTxPrevOutputFetcher *txscript.MultiPrevOutFetcher
	CommitTxPrivateKeyList    []*btcec.PrivateKey
	InscriptionTxCtxDataList  []*inscriptionTxCtxData
	RevealTxPrevOutputFetcher *txscript.MultiPrevOutFetcher
	CommitTxPrevOutputList    []*PrevOutput
	RevealTx                  []*wire.MsgTx
	CommitTx                  *wire.MsgTx
	MustCommitTxFee           int64
	MustRevealTxFees          []int64
	CommitAddrs               []string
}

type InscribeTxs struct {
	CommitTx     string   `json:"commitTx"`
	RevealTxs    []string `json:"revealTxs"`
	CommitTxFee  int64    `json:"commitTxFee"`
	RevealTxFees []int64  `json:"revealTxFees"`
	CommitAddrs  []string `json:"commitAddrs"`
}

type InscribeForMPCRes struct {
	SigHashList  []string `json:"sigHashList"`
	CommitTx     string   `json:"commitTx"`
	RevealTxs    []string `json:"revealTxs"`
	CommitTxFee  int64    `json:"commitTxFee"`
	RevealTxFees []int64  `json:"revealTxFees"`
	CommitAddrs  []string `json:"commitAddrs"`
}

const (
	DefaultTxVersion      = 2
	DefaultSequenceNum    = 0xfffffffd
	DefaultRevealOutValue = int64(546)
	DefaultMinChangeValue = int64(546)

	MaxStandardTxWeight = 4000000 / 10
	WitnessScaleFactor  = 4

	OrdPrefix = "ord"
)

func NewInscriptionTool(network *chaincfg.Params, request *InscriptionRequest) (*InscriptionBuilder, error) {
	var commitTxPrivateKeyList []*btcec.PrivateKey
	for _, prevOutput := range request.CommitTxPrevOutputList {
		privateKeyWif, err := btcutil.DecodeWIF(prevOutput.PrivateKey)
		if err != nil {
			return nil, err
		}
		commitTxPrivateKeyList = append(commitTxPrivateKeyList, privateKeyWif.PrivKey)
	}
	tool := &InscriptionBuilder{
		Network:                   network,
		CommitTxPrevOutputFetcher: txscript.NewMultiPrevOutFetcher(nil),
		CommitTxPrivateKeyList:    commitTxPrivateKeyList,
		InscriptionTxCtxDataList:  make([]*inscriptionTxCtxData, len(request.InscriptionDataList)),
		RevealTxPrevOutputFetcher: txscript.NewMultiPrevOutFetcher(nil),
		CommitTxPrevOutputList:    request.CommitTxPrevOutputList,
	}
	return tool, tool.initTool(network, request)
}

func (builder *InscriptionBuilder) initTool(network *chaincfg.Params, request *InscriptionRequest) error {
	destinations := make([]string, len(request.InscriptionDataList))
	revealOutValue := DefaultRevealOutValue
	if request.RevealOutValue > 0 {
		revealOutValue = request.RevealOutValue
	}
	minChangeValue := DefaultMinChangeValue
	if request.MinChangeValue > 0 {
		minChangeValue = request.MinChangeValue
	}
	for i := 0; i < len(request.InscriptionDataList); i++ {
		inscriptionTxCtxData, err := newInscriptionTxCtxData(network, request, i)
		if err != nil {
			return err
		}
		builder.InscriptionTxCtxDataList[i] = inscriptionTxCtxData
		destinations[i] = request.InscriptionDataList[i].RevealAddr
	}
	totalRevealPrevOutputValue, err := builder.buildEmptyRevealTx(destinations, revealOutValue, request.RevealFeeRate)
	if err != nil {
		return err
	}
	err = builder.buildCommitTx(request.CommitTxPrevOutputList, request.ChangeAddress, totalRevealPrevOutputValue, request.CommitFeeRate, minChangeValue)
	if err != nil {
		return err
	}
	err = builder.signCommitTx()
	if err != nil {
		return errors.New("sign commit tx error")
	}
	err = builder.completeRevealTx()
	if err != nil {
		return err
	}
	return nil
}

func newInscriptionTxCtxData(network *chaincfg.Params, inscriptionRequest *InscriptionRequest, indexOfInscriptionDataList int) (*inscriptionTxCtxData, error) {
	privateKeyWif, err := btcutil.DecodeWIF(inscriptionRequest.CommitTxPrevOutputList[0].PrivateKey)
	if err != nil {
		return nil, err
	}
	privateKey := privateKeyWif.PrivKey

	inscriptionBuilder := txscript.NewScriptBuilder().
		AddData(schnorr.SerializePubKey(privateKey.PubKey())).
		AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_FALSE).
		AddOp(txscript.OP_IF).
		AddData([]byte(OrdPrefix)).
		AddOp(txscript.OP_DATA_1).
		AddOp(txscript.OP_DATA_1).
		AddData([]byte(inscriptionRequest.InscriptionDataList[indexOfInscriptionDataList].ContentType)).
		AddOp(txscript.OP_0)
	maxChunkSize := 520
	// use taproot to skip txscript.MaxScriptSize 10000
	bodySize := len(inscriptionRequest.InscriptionDataList[indexOfInscriptionDataList].Body)
	for i := 0; i < bodySize; i += maxChunkSize {
		end := i + maxChunkSize
		if end > bodySize {
			end = bodySize
		}

		inscriptionBuilder.AddFullData(inscriptionRequest.InscriptionDataList[indexOfInscriptionDataList].Body[i:end])
	}
	inscriptionScript, err := inscriptionBuilder.Script()
	if err != nil {
		return nil, err
	}
	inscriptionScript = append(inscriptionScript, txscript.OP_ENDIF)

	proof := &txscript.TapscriptProof{
		TapLeaf:  txscript.NewBaseTapLeaf(schnorr.SerializePubKey(privateKey.PubKey())),
		RootNode: txscript.NewBaseTapLeaf(inscriptionScript),
	}

	controlBlock := proof.ToControlBlock(privateKey.PubKey())
	controlBlockWitness, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	tapHash := proof.RootNode.TapHash()
	commitTxAddress, err := btcutil.NewAddressTaproot(schnorr.SerializePubKey(txscript.ComputeTaprootOutputKey(privateKey.PubKey(), tapHash[:])), network)
	if err != nil {
		return nil, err
	}
	commitTxAddressPkScript, err := txscript.PayToAddrScript(commitTxAddress)
	if err != nil {
		return nil, err
	}

	return &inscriptionTxCtxData{
		PrivateKey:              privateKey,
		InscriptionScript:       inscriptionScript,
		CommitTxAddress:         commitTxAddress.EncodeAddress(),
		CommitTxAddressPkScript: commitTxAddressPkScript,
		ControlBlockWitness:     controlBlockWitness,
	}, nil
}

func (builder *InscriptionBuilder) buildEmptyRevealTx(destination []string, revealOutValue, revealFeeRate int64) (int64, error) {
	addTxInTxOutIntoRevealTx := func(tx *wire.MsgTx, index int) error {
		in := wire.NewTxIn(&wire.OutPoint{Index: uint32(index)}, nil, nil)
		in.Sequence = DefaultSequenceNum
		tx.AddTxIn(in)
		scriptPubKey, err := AddrToPkScript(destination[index], builder.Network)
		if err != nil {
			return err
		}
		out := wire.NewTxOut(revealOutValue, scriptPubKey)
		tx.AddTxOut(out)
		return nil
	}

	totalPrevOutputValue := int64(0)
	total := len(builder.InscriptionTxCtxDataList)
	revealTx := make([]*wire.MsgTx, total)
	mustRevealTxFees := make([]int64, total)
	commitAddrs := make([]string, total)
	for i := 0; i < total; i++ {
		tx := wire.NewMsgTx(DefaultTxVersion)
		err := addTxInTxOutIntoRevealTx(tx, i)
		if err != nil {
			return 0, err
		}
		prevOutputValue := revealOutValue + int64(tx.SerializeSize())*revealFeeRate
		emptySignature := make([]byte, 64)
		emptyControlBlockWitness := make([]byte, 33)
		fee := (int64(wire.TxWitness{emptySignature, builder.InscriptionTxCtxDataList[i].InscriptionScript, emptyControlBlockWitness}.SerializeSize()+2+3) / 4) * revealFeeRate
		prevOutputValue += fee
		builder.InscriptionTxCtxDataList[i].RevealTxPrevOutput = &wire.TxOut{
			PkScript: builder.InscriptionTxCtxDataList[i].CommitTxAddressPkScript,
			Value:    prevOutputValue,
		}
		totalPrevOutputValue += prevOutputValue
		revealTx[i] = tx
		mustRevealTxFees[i] = int64(tx.SerializeSize())*revealFeeRate + fee
		commitAddrs[i] = builder.InscriptionTxCtxDataList[i].CommitTxAddress
	}
	builder.RevealTx = revealTx
	builder.MustRevealTxFees = mustRevealTxFees
	builder.CommitAddrs = commitAddrs

	return totalPrevOutputValue, nil
}

func (builder *InscriptionBuilder) buildCommitTx(commitTxPrevOutputList []*PrevOutput, changeAddress string, totalRevealPrevOutputValue, commitFeeRate int64, minChangeValue int64) error {
	totalSenderAmount := btcutil.Amount(0)
	tx := wire.NewMsgTx(DefaultTxVersion)
	changePkScript, err := AddrToPkScript(changeAddress, builder.Network)
	if err != nil {
		return err
	}
	for _, prevOutput := range commitTxPrevOutputList {
		txHash, err := chainhash.NewHashFromStr(prevOutput.TxId)
		if err != nil {
			return err
		}
		outPoint := wire.NewOutPoint(txHash, prevOutput.VOut)
		pkScript, err := AddrToPkScript(prevOutput.Address, builder.Network)
		if err != nil {
			return err
		}
		txOut := wire.NewTxOut(prevOutput.Amount, pkScript)
		builder.CommitTxPrevOutputFetcher.AddPrevOut(*outPoint, txOut)

		in := wire.NewTxIn(outPoint, nil, nil)
		in.Sequence = DefaultSequenceNum
		tx.AddTxIn(in)

		totalSenderAmount += btcutil.Amount(prevOutput.Amount)
	}
	for i := range builder.InscriptionTxCtxDataList {
		tx.AddTxOut(builder.InscriptionTxCtxDataList[i].RevealTxPrevOutput)
	}

	tx.AddTxOut(wire.NewTxOut(0, changePkScript))

	txForEstimate := wire.NewMsgTx(DefaultTxVersion)
	txForEstimate.TxIn = tx.TxIn
	txForEstimate.TxOut = tx.TxOut
	if err = Sign(txForEstimate, builder.CommitTxPrivateKeyList, builder.CommitTxPrevOutputFetcher); err != nil {
		return err
	}

	fee := btcutil.Amount(GetTxVirtualSize(btcutil.NewTx(txForEstimate))) * btcutil.Amount(commitFeeRate)
	changeAmount := totalSenderAmount - btcutil.Amount(totalRevealPrevOutputValue) - fee
	if int64(changeAmount) >= minChangeValue {
		tx.TxOut[len(tx.TxOut)-1].Value = int64(changeAmount)
	} else {
		tx.TxOut = tx.TxOut[:len(tx.TxOut)-1]
		if changeAmount < 0 {
			txForEstimate.TxOut = txForEstimate.TxOut[:len(txForEstimate.TxOut)-1]
			feeWithoutChange := btcutil.Amount(GetTxVirtualSize(btcutil.NewTx(txForEstimate))) * btcutil.Amount(commitFeeRate)
			if totalSenderAmount-btcutil.Amount(totalRevealPrevOutputValue)-feeWithoutChange < 0 {
				builder.MustCommitTxFee = int64(fee)
				return errors.New("insufficient balance")
			}
		}
	}
	builder.CommitTx = tx
	return nil
}

func (builder *InscriptionBuilder) completeRevealTx() error {
	for i := range builder.InscriptionTxCtxDataList {
		builder.RevealTxPrevOutputFetcher.AddPrevOut(wire.OutPoint{
			Hash:  builder.CommitTx.TxHash(),
			Index: uint32(i),
		}, builder.InscriptionTxCtxDataList[i].RevealTxPrevOutput)
		builder.RevealTx[i].TxIn[0].PreviousOutPoint.Hash = builder.CommitTx.TxHash()
	}
	for i := range builder.InscriptionTxCtxDataList {
		revealTx := builder.RevealTx[i]
		witnessArray, err := txscript.CalcTapscriptSignaturehash(txscript.NewTxSigHashes(revealTx, builder.RevealTxPrevOutputFetcher),
			txscript.SigHashDefault, revealTx, 0, builder.RevealTxPrevOutputFetcher, txscript.NewBaseTapLeaf(builder.InscriptionTxCtxDataList[i].InscriptionScript))
		if err != nil {
			return err
		}
		signature, err := schnorr.Sign(builder.InscriptionTxCtxDataList[i].PrivateKey, witnessArray)
		if err != nil {
			return err
		}
		witness := wire.TxWitness{signature.Serialize(), builder.InscriptionTxCtxDataList[i].InscriptionScript, builder.InscriptionTxCtxDataList[i].ControlBlockWitness}
		builder.RevealTx[i].TxIn[0].Witness = witness
	}
	// check tx max tx wight
	for i, tx := range builder.RevealTx {
		revealWeight := GetTransactionWeight(btcutil.NewTx(tx))
		if revealWeight > MaxStandardTxWeight {
			return errors.New(fmt.Sprintf("reveal(index %d) transaction weight greater than %d (MAX_STANDARD_TX_WEIGHT): %d", i, MaxStandardTxWeight, revealWeight))
		}
	}
	return nil
}

func (builder *InscriptionBuilder) signCommitTx() error {
	return Sign(builder.CommitTx, builder.CommitTxPrivateKeyList, builder.CommitTxPrevOutputFetcher)
}

func SignTxInput1(privateKey *btcec.PrivateKey, tx *wire.MsgTx, index int, txSigHashes *txscript.TxSigHashes,
	pkScript []byte, amount int64) error {
	if txscript.IsPayToTaproot(pkScript) {
		witness, err := txscript.TaprootWitnessSignature(tx, txSigHashes, index, amount, pkScript, txscript.SigHashDefault, privateKey)
		if err != nil {
			return err
		}

		tx.TxIn[index].Witness = witness

		return nil
	}

	if txscript.IsPayToPubKeyHash(pkScript) {
		sigScript, err := txscript.SignatureScript(tx, index, pkScript, txscript.SigHashAll, privateKey, true)
		if err != nil {
			return err
		}

		tx.TxIn[index].SignatureScript = sigScript

		return nil
	}

	pubKeyBytes := privateKey.PubKey().SerializeCompressed()
	script, err := PayToPubKeyHashScript(btcutil.Hash160(pubKeyBytes))
	if err != nil {
		return err
	}

	witness, err := txscript.WitnessSignature(tx, txSigHashes, index, amount, script, txscript.SigHashAll, privateKey, true)
	if err != nil {
		return err
	}
	tx.TxIn[index].Witness = witness

	if !txscript.IsPayToScriptHash(pkScript) {
		return nil
	}

	redeemScript, err := PayToWitnessPubKeyHashScript(btcutil.Hash160(pubKeyBytes))
	if err != nil {
		return err
	}

	tx.TxIn[index].SignatureScript = append([]byte{byte(len(redeemScript))}, redeemScript...)

	return nil
}

func Sign(tx *wire.MsgTx, privateKeys []*btcec.PrivateKey, prevOutFetcher *txscript.MultiPrevOutFetcher) error {
	txSigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	for i, in := range tx.TxIn {
		prevOut := prevOutFetcher.FetchPrevOutput(in.PreviousOutPoint)
		err := SignTxInput1(privateKeys[i], tx, i, txSigHashes, prevOut.PkScript, prevOut.Value)
		if err != nil {
			return err
		}
	}

	return nil
}

func GetTxHex(tx *wire.MsgTx) (string, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf.Bytes()), nil
}

func (builder *InscriptionBuilder) GetCommitTxHex() (string, error) {
	return GetTxHex(builder.CommitTx)
}

func (builder *InscriptionBuilder) GetRevealTxHexList() ([]string, error) {
	txHexList := make([]string, len(builder.RevealTx))
	for i := range builder.RevealTx {
		txHex, err := GetTxHex(builder.RevealTx[i])
		if err != nil {
			return nil, err
		}
		txHexList[i] = txHex
	}
	return txHexList, nil
}

func (builder *InscriptionBuilder) CalculateFee() (int64, []int64) {
	commitTxFee := int64(0)
	for _, in := range builder.CommitTx.TxIn {
		commitTxFee += builder.CommitTxPrevOutputFetcher.FetchPrevOutput(in.PreviousOutPoint).Value
	}
	for _, out := range builder.CommitTx.TxOut {
		commitTxFee -= out.Value
	}
	revealTxFees := make([]int64, 0)
	for _, tx := range builder.RevealTx {
		revealTxFee := int64(0)
		for i, in := range tx.TxIn {
			revealTxFee += builder.RevealTxPrevOutputFetcher.FetchPrevOutput(in.PreviousOutPoint).Value
			revealTxFee -= tx.TxOut[i].Value
			revealTxFees = append(revealTxFees, revealTxFee)
		}
	}
	return commitTxFee, revealTxFees
}

func Inscribe(network *chaincfg.Params, request *InscriptionRequest) (*InscribeTxs, error) {
	tool, err := NewInscriptionTool(network, request)
	if err != nil && err.Error() == "insufficient balance" {
		return &InscribeTxs{
			CommitTx:     "",
			RevealTxs:    []string{},
			CommitTxFee:  tool.MustCommitTxFee,
			RevealTxFees: tool.MustRevealTxFees,
			CommitAddrs:  tool.CommitAddrs,
		}, nil
	}

	if err != nil {
		return nil, err
	}

	commitTx, err := tool.GetCommitTxHex()
	if err != nil {
		return nil, err
	}
	revealTxs, err := tool.GetRevealTxHexList()
	if err != nil {
		return nil, err
	}

	commitTxFee, revealTxFees := tool.CalculateFee()

	return &InscribeTxs{
		CommitTx:     commitTx,
		RevealTxs:    revealTxs,
		CommitTxFee:  commitTxFee,
		RevealTxFees: revealTxFees,
		CommitAddrs:  tool.CommitAddrs,
	}, nil
}

// GetTransactionWeight computes the value of the weight metric for a given
// transaction. Currently the weight metric is simply the sum of the
// transactions's serialized size without any witness data scaled
// proportionally by the WitnessScaleFactor, and the transaction's serialized
// size including any witness data.
func GetTransactionWeight(tx *btcutil.Tx) int64 {
	msgTx := tx.MsgTx()

	baseSize := msgTx.SerializeSizeStripped()
	totalSize := msgTx.SerializeSize()

	// (baseSize * 3) + totalSize
	return int64((baseSize * (WitnessScaleFactor - 1)) + totalSize)
}

func GetTransactionWeight2(msgTx *wire.MsgTx) int64 {
	baseSize := msgTx.SerializeSizeStripped()
	totalSize := msgTx.SerializeSize()

	// (baseSize * 3) + totalSize
	return int64((baseSize * (WitnessScaleFactor - 1)) + totalSize)
}

// GetTxVirtualSize computes the virtual size of a given transaction. A
// transaction's virtual size is based off its weight, creating a discount for
// any witness data it contains, proportional to the current
// blockchain.WitnessScaleFactor value.
func GetTxVirtualSize(tx *btcutil.Tx) int64 {
	// vSize := (weight(tx) + 3) / 4
	//       := (((baseSize * 3) + totalSize) + 3) / 4
	// We add 3 here as a way to compute the ceiling of the prior arithmetic
	// to 4. The division by 4 creates a discount for wit witness data.
	return (GetTransactionWeight(tx) + (WitnessScaleFactor - 1)) / WitnessScaleFactor
}
func InscribeForMPCUnsigned(request *InscriptionRequest, network *chaincfg.Params, unsignedCommitHash, signedCommitTxHash *chainhash.Hash) (*InscribeForMPCRes, error) {

	wif, err := btcutil.DecodeWIF(request.CommitTxPrevOutputList[0].PrivateKey)
	if err != nil {
		return nil, err
	}
	randPrvKey := wif.PrivKey
	scriptCtxList, err := buildInscriptionScriptCtxList(request, network)
	if err != nil {
		return nil, err
	}

	// build reveal tx list
	revealTxList := make([]*wire.MsgTx, len(scriptCtxList))
	commitTxOutList := make([]*wire.TxOut, 0)
	totalRevealInValue := int64(0)
	for i, ctx := range scriptCtxList {
		revealTx := wire.NewMsgTx(DefaultTxVersion)

		in := wire.NewTxIn(&wire.OutPoint{Index: uint32(i)}, nil, nil)
		in.Sequence = DefaultSequenceNum
		revealTx.AddTxIn(in)

		scriptPubKey, err := AddrToPkScript(request.InscriptionDataList[i].RevealAddr, network)
		if err != nil {
			return nil, err
		}
		revealOutValue := DefaultRevealOutValue
		if request.RevealOutValue > 0 {
			revealOutValue = request.RevealOutValue
		}
		out := wire.NewTxOut(revealOutValue, scriptPubKey)
		revealTx.AddTxOut(out)

		revealTxList[i] = revealTx

		emptySignature := make([]byte, 64)
		emptyControlBlockWitness := make([]byte, 33)
		fakeWitness := wire.TxWitness{emptySignature, ctx.InscriptionScript, emptyControlBlockWitness}
		revealFee := int64(revealTx.SerializeSize()+((fakeWitness.SerializeSize()+2+3)/4)) * request.RevealFeeRate
		revealInValue := revealOutValue + revealFee

		ctx.RevealTxPrevOutput = &wire.TxOut{
			PkScript: ctx.CommitTxAddressPkScript,
			Value:    revealInValue,
		}
		totalRevealInValue += revealInValue

		commitTxOutList = append(commitTxOutList, wire.NewTxOut(revealInValue, ctx.CommitTxAddressPkScript))
	}

	// build commit tx
	commitTx := wire.NewMsgTx(DefaultTxVersion)
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	totalCommitInValue := int64(0)
	for _, utxo := range request.CommitTxPrevOutputList {
		txHash, err := chainhash.NewHashFromStr(utxo.TxId)
		if err != nil {
			return nil, err
		}
		outPoint := wire.NewOutPoint(txHash, utxo.VOut)

		in := wire.NewTxIn(outPoint, nil, nil)
		in.Sequence = DefaultSequenceNum
		commitTx.AddTxIn(in)

		pkScript, err := AddrToPkScript(utxo.Address, network)
		if err != nil {
			return nil, err
		}
		txOut := wire.NewTxOut(utxo.Amount, pkScript)
		prevOutFetcher.AddPrevOut(*outPoint, txOut)

		totalCommitInValue += utxo.Amount
	}

	for _, commitTxOut := range commitTxOutList {
		commitTx.AddTxOut(commitTxOut)
	}

	changePkScript, err := AddrToPkScript(request.ChangeAddress, network)
	if err != nil {
		return nil, err
	}
	commitTx.AddTxOut(wire.NewTxOut(0, changePkScript))

	estimateTx := commitTx.Copy()
	fakePrvKeyList := make([]*btcec.PrivateKey, len(estimateTx.TxIn))
	fakePrvKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	for i := range fakePrvKeyList {
		fakePrvKeyList[i] = fakePrvKey
	}
	if err := Sign(estimateTx, fakePrvKeyList, prevOutFetcher); err != nil {
		return nil, err
	}

	commitFee := GetTxVirtualSize(btcutil.NewTx(estimateTx)) * request.CommitFeeRate
	changeValue := totalCommitInValue - totalRevealInValue - commitFee
	minChangeValue := DefaultMinChangeValue
	if request.MinChangeValue > 0 {
		minChangeValue = request.MinChangeValue
	}
	if changeValue >= minChangeValue {
		commitTx.TxOut[len(commitTx.TxOut)-1].Value = changeValue
	} else {
		commitTx.TxOut = commitTx.TxOut[:len(commitTx.TxOut)-1]
		estimateTx.TxOut = estimateTx.TxOut[:len(estimateTx.TxOut)-1]
		feeWithoutChange := GetTxVirtualSize(btcutil.NewTx(estimateTx)) * request.CommitFeeRate
		if totalCommitInValue-totalRevealInValue-feeWithoutChange < 0 {
			return nil, errors.New("insufficient balance")
		}
	}

	sigHashList, err := calcSigHash(commitTx, prevOutFetcher, request)
	if err != nil {
		return nil, err
	}
	// sign reveal tx
	commitTxHash := commitTx.TxHash()
	if signedCommitTxHash != nil {
		commitTxHash = *signedCommitTxHash
	}
	revealTxFees := make([]int64, 0)
	commitAddrs := make([]string, len(scriptCtxList))
	for i, ctx := range scriptCtxList {
		revealTxList[i].TxIn[0].PreviousOutPoint.Hash = commitTxHash
		outPoint := wire.NewOutPoint(&commitTxHash, uint32(i))
		revealTxPrevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
		revealTxPrevOutFetcher.AddPrevOut(*outPoint, ctx.RevealTxPrevOutput)
		txSigHashes := txscript.NewTxSigHashes(revealTxList[i], revealTxPrevOutFetcher)
		tapLeaf := txscript.NewBaseTapLeaf(ctx.InscriptionScript)

		signature, err := txscript.RawTxInTapscriptSignature(revealTxList[i], txSigHashes, 0,
			ctx.RevealTxPrevOutput.Value, ctx.RevealTxPrevOutput.PkScript, tapLeaf, txscript.SigHashDefault, randPrvKey)
		if err != nil {
			return nil, err
		}
		revealTxList[i].TxIn[0].Witness = wire.TxWitness{signature, ctx.InscriptionScript, ctx.ControlBlockWitness}

		revealTxFee := int64(0)
		tx := revealTxList[i]
		for k, in := range tx.TxIn {
			revealTxFee += revealTxPrevOutFetcher.FetchPrevOutput(in.PreviousOutPoint).Value
			revealTxFee -= tx.TxOut[k].Value
			revealTxFees = append(revealTxFees, revealTxFee)
		}
		commitAddrs[i] = ctx.CommitTxAddress
	}

	commitTxFee := int64(0)
	for _, in := range commitTx.TxIn {
		commitTxFee += prevOutFetcher.FetchPrevOutput(in.PreviousOutPoint).Value
	}
	for _, out := range commitTx.TxOut {
		commitTxFee -= out.Value
	}
	unsignedCommitTxHex, err := GetTxHex(commitTx)
	if err != nil {
		return nil, err
	}
	revealTxHexList := make([]string, 0)
	for _, tx := range revealTxList {
		s, err := GetTxHex(tx)
		if err != nil {
			return nil, err
		}
		revealTxHexList = append(revealTxHexList, s)
	}
	res := &InscribeForMPCRes{
		SigHashList:  sigHashList,
		CommitTx:     unsignedCommitTxHex,
		RevealTxs:    revealTxHexList,
		CommitTxFee:  commitTxFee,
		RevealTxFees: revealTxFees,
		CommitAddrs:  commitAddrs,
	}
	return res, nil
}

func InscribeForMPCSigned(request *InscriptionRequest, network *chaincfg.Params, commitTx string, signatures []string) (*InscribeForMPCRes, error) {
	var tx wire.MsgTx
	buf, err := hex.DecodeString(commitTx)
	if err != nil {
		return nil, err
	}
	err = tx.Deserialize(bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	unsignedCommitTxHash := tx.TxHash()

	for i, in := range tx.TxIn {
		rBytes, err := hex.DecodeString(signatures[i][:64])
		if err != nil {
			return nil, err
		}
		sBytes, err := hex.DecodeString(signatures[i][64:128])
		if err != nil {
			return nil, err
		}

		r := new(btcec.ModNScalar)
		r.SetByteSlice(rBytes)
		s := new(btcec.ModNScalar)
		s.SetByteSlice(sBytes)
		signature := append(ecdsa.NewSignature(r, s).Serialize(), byte(txscript.SigHashAll))

		if len(in.Witness) == 0 {
			pubKey := in.SignatureScript
			script, err := txscript.NewScriptBuilder().AddData(signature).AddData(pubKey).Script()
			if err != nil {
				return nil, err
			}
			in.SignatureScript = script
		} else {
			pubKey := in.Witness[0]
			in.Witness = wire.TxWitness{signature, pubKey}
		}
	}
	signedCommitTxHash := tx.TxHash()
	var buffer bytes.Buffer
	if err := tx.Serialize(&buffer); err != nil {
		return nil, err
	}
	signedCommitTxHex := hex.EncodeToString(buffer.Bytes())
	res, err := InscribeForMPCUnsigned(request, network, &unsignedCommitTxHash, &signedCommitTxHash)
	if err != nil {
		return nil, err
	}
	res.SigHashList = nil
	res.CommitTx = signedCommitTxHex
	return res, nil
}

func buildInscriptionScriptCtxList(request *InscriptionRequest, network *chaincfg.Params) ([]*inscriptionTxCtxData, error) {
	var scriptCtxList []*inscriptionTxCtxData
	for i := range request.InscriptionDataList {
		scriptCtx, err := newInscriptionTxCtxData(network, request, i)
		if err != nil {
			return nil, err
		}

		scriptCtxList = append(scriptCtxList, scriptCtx)
	}

	return scriptCtxList, nil
}

func calcSigHash(tx *wire.MsgTx, prevOutFetcher txscript.PrevOutputFetcher, request *InscriptionRequest) ([]string, error) {
	sigHashList := make([]string, len(tx.TxIn))

	txSigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	for i, in := range tx.TxIn {
		pubKeyBytes, err := hex.DecodeString(request.CommitTxPrevOutputList[i].PublicKey)
		if err != nil {
			return nil, err
		}
		prevOut := prevOutFetcher.FetchPrevOutput(in.PreviousOutPoint)
		var sigHash []byte
		if txscript.IsPayToTaproot(prevOut.PkScript) {
			sigHash, err = txscript.CalcTaprootSignatureHash(txSigHashes, txscript.SigHashDefault, tx, i, prevOutFetcher)
			if err != nil {
				return nil, err
			}
		} else if txscript.IsPayToPubKeyHash(prevOut.PkScript) {
			sigHash, err = txscript.CalcSignatureHash(prevOut.PkScript, txscript.SigHashAll, tx, i)
			if err != nil {
				return nil, err
			}
			// store publicKey
			in.SignatureScript = pubKeyBytes
		} else {
			script, err := PayToPubKeyHashScript(btcutil.Hash160(pubKeyBytes))
			if err != nil {
				return nil, err
			}
			sigHash, err = txscript.CalcWitnessSigHash(script, txSigHashes, txscript.SigHashAll, tx, i, prevOut.Value)
			if err != nil {
				return nil, err
			}

			// store publicKey
			in.Witness = wire.TxWitness{pubKeyBytes}
			if txscript.IsPayToScriptHash(prevOut.PkScript) {
				redeemScript, err := PayToWitnessPubKeyHashScript(btcutil.Hash160(pubKeyBytes))
				if err != nil {
					return nil, err
				}
				in.SignatureScript = append([]byte{byte(len(redeemScript))}, redeemScript...)
			}
		}

		sigHashList[i] = hex.EncodeToString(sigHash)
	}

	return sigHashList, nil
}

func GetTxVirtualSize2(msgTx *wire.MsgTx) int64 {
	// vSize := (weight(tx) + 3) / 4
	//       := (((baseSize * 3) + totalSize) + 3) / 4
	// We add 3 here as a way to compute the ceiling of the prior arithmetic
	// to 4. The division by 4 creates a discount for wit witness data.
	return (GetTransactionWeight2(msgTx) + (WitnessScaleFactor - 1)) / WitnessScaleFactor
}
