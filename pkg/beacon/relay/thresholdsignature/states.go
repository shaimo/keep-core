package thresholdsignature

import (
	"fmt"
	"math/big"
	"os"
	"time"

	bn256 "github.com/ethereum/go-ethereum/crypto/bn256/cloudflare"
	"github.com/keep-network/keep-core/pkg/altbn128"
	relayChain "github.com/keep-network/keep-core/pkg/beacon/relay/chain"
	"github.com/keep-network/keep-core/pkg/beacon/relay/dkg"
	"github.com/keep-network/keep-core/pkg/beacon/relay/event"
	"github.com/keep-network/keep-core/pkg/beacon/relay/group"
	"github.com/keep-network/keep-core/pkg/beacon/relay/state"
	"github.com/keep-network/keep-core/pkg/bls"
	"github.com/keep-network/keep-core/pkg/chain"
	"github.com/keep-network/keep-core/pkg/net"
)

type signingState = state.State

type signingStateBase struct {
	channel      net.BroadcastChannel
	relayChain   relayChain.RelayEntryInterface
	blockCounter chain.BlockCounter

	signer *dkg.ThresholdSigner

	requestID     *big.Int
	previousEntry *big.Int
	seed          *big.Int

	threshold int
}

type signatureShareState struct {
	signingStateBase

	selfSignatureShare     *bn256.G1
	signatureShareMessages []*SignatureShareMessage
}

func (sss *signatureShareState) DelayBlocks() uint64 {
	return state.MessagingStateDelayBlocks
}

func (sss *signatureShareState) ActiveBlocks() uint64 {
	return state.MessagingStateActiveBlocks
}

func (sss *signatureShareState) Initiate() error {
	entryToSign := CombineEntryToSign(sss.previousEntry, sss.seed)
	share := sss.signer.CalculateSignatureShare(entryToSign)
	sss.selfSignatureShare = share
	message := &SignatureShareMessage{
		sss.MemberIndex(),
		share.Marshal(),
		sss.requestID,
	}
	if err := sss.channel.Send(message); err != nil {
		return err
	}
	return nil
}

func (sss *signatureShareState) Receive(msg net.Message) error {
	switch signatureShareMessage := msg.Payload().(type) {
	case *SignatureShareMessage:
		if !group.IsMessageFromSelf(
			sss.MemberIndex(),
			signatureShareMessage,
		) {
			sss.signatureShareMessages = append(
				sss.signatureShareMessages,
				signatureShareMessage,
			)
		}
	}

	return nil
}

func (sss *signatureShareState) Next() signingState {
	return &signatureCompleteState{
		signingStateBase:      sss.signingStateBase,
		selfSignatureShare:    sss.selfSignatureShare,
		previousPhaseMessages: sss.signatureShareMessages,
	}
}

func (sss *signatureShareState) MemberIndex() group.MemberIndex {
	return sss.signer.MemberID()
}

type signatureCompleteState struct {
	signingStateBase

	selfSignatureShare    *bn256.G1
	previousPhaseMessages []*SignatureShareMessage
	fullSignature         []byte
}

func (scs *signatureCompleteState) DelayBlocks() uint64 {
	return state.SilentStateDelayBlocks
}

func (scs *signatureCompleteState) ActiveBlocks() uint64 {
	return state.SilentStateActiveBlocks
}

func (scs *signatureCompleteState) Initiate() error {
	seenShares := make(map[group.MemberIndex]*bn256.G1)
	seenShares[scs.MemberIndex()] = scs.selfSignatureShare

	for _, message := range scs.previousPhaseMessages {
		share := new(bn256.G1)
		_, err := share.Unmarshal(message.ShareBytes)
		if err != nil {
			fmt.Fprintf(
				os.Stderr,
				"[member:%v] failed to unmarshal signature share from [%v]: [%v]",
				scs.MemberIndex(),
				message.senderID,
				err,
			)
		} else {
			seenShares[message.senderID] = share

		}
	}

	seenSharesSlice := make([]*bls.SignatureShare, 0)
	for memberID, share := range seenShares {
		signatureShare := &bls.SignatureShare{I: int(memberID), V: share}
		seenSharesSlice = append(seenSharesSlice, signatureShare)
	}

	signature, err := scs.signer.CompleteSignature(seenSharesSlice, scs.threshold)
	if err != nil {
		return err
	}

	scs.fullSignature = altbn128.G1Point{G1: signature}.Compress()

	return nil
}

func (scs *signatureCompleteState) Receive(msg net.Message) error {
	return nil
}

func (scs *signatureCompleteState) Next() signingState {
	return &entrySubmissionState{
		signingStateBase: scs.signingStateBase,
		signature:        scs.fullSignature,
	}
}

func (scs *signatureCompleteState) MemberIndex() group.MemberIndex {
	return scs.signer.MemberID()
}

type entrySubmissionState struct {
	signingStateBase

	signature []byte
}

func (ess *entrySubmissionState) DelayBlocks() uint64 {
	return state.SilentStateDelayBlocks
}

func (ess *entrySubmissionState) ActiveBlocks() uint64 {
	// We do not exchange any messages in this phase. We publish entry to the
	// chain but it is an action blocking all group members for the same time
	// - members exit when the first valid entry is accepted by the chain.
	// How long it takes depends on the block step and group size.
	return state.SilentStateActiveBlocks
}

func (ess *entrySubmissionState) Initiate() error {
	rightSizeSignature := big.NewInt(0).SetBytes(ess.signature[:32])

	newEntry := &event.Entry{
		RequestID:     ess.requestID,
		Value:         rightSizeSignature,
		PreviousEntry: ess.previousEntry,
		Timestamp:     time.Now().UTC(),
		GroupPubKey:   ess.signer.GroupPublicKeyBytes(),
		Seed:          ess.seed,
	}

	// TODO: Extract submission code to a separate class
	ess.relayChain.SubmitRelayEntry(
		newEntry,
	).OnFailure(func(err error) {
		if err != nil {
			fmt.Fprintf(
				os.Stderr,
				"Failed submission of relay entry: [%v].\n",
				err,
			)
		}
	})

	return nil
}

func (ess *entrySubmissionState) Receive(msg net.Message) error {
	return nil
}

func (ess *entrySubmissionState) Next() signingState {
	return nil
}

func (ess *entrySubmissionState) MemberIndex() group.MemberIndex {
	return ess.signer.MemberID()
}
