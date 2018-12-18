// Package gjkr contains code that implements Distributed Key Generation protocol
// described in [GJKR 99].
//
// See http://docs.keep.network/cryptography/beacon_dkg.html#_protocol
//
//     [GJKR 99]: Gennaro R., Jarecki S., Krawczyk H., Rabin T. (1999) Secure
//         Distributed Key Generation for Discrete-Log Based Cryptosystems. In:
//         Stern J. (eds) Advances in Cryptology — EUROCRYPT ’99. EUROCRYPT 1999.
//         Lecture Notes in Computer Science, vol 1592. Springer, Berlin, Heidelberg
//         http://groups.csail.mit.edu/cis/pubs/stasio/vss.ps.gz
package gjkr

import (
	"fmt"
	"math/big"

	"github.com/keep-network/keep-core/pkg/net/ephemeral"
)

// GenerateEphemeralKeyPair takes the group member list and generates an
// ephemeral ECDH keypair for every other group member. Generated public
// ephemeral keys are broadcasted within the group.
//
// See Phase 1 of the protocol specification.
func (em *EphemeralKeyPairGeneratingMember) GenerateEphemeralKeyPair() (
	*EphemeralPublicKeyMessage,
	error,
) {
	ephemeralKeys := make(map[MemberID]*ephemeral.PublicKey)

	// Calculate ephemeral key pair for every other group member
	for _, member := range em.group.memberIDs {
		if member == em.ID {
			// don’t actually generate a key with ourselves
			continue
		}

		ephemeralKeyPair, err := ephemeral.GenerateKeyPair()
		if err != nil {
			return nil, err
		}

		// save the generated ephemeral key to our state
		em.ephemeralKeyPairs[member] = ephemeralKeyPair

		// store the public key to the map for the message
		ephemeralKeys[member] = ephemeralKeyPair.PublicKey
	}

	return &EphemeralPublicKeyMessage{
		senderID:            em.ID,
		ephemeralPublicKeys: ephemeralKeys,
	}, nil
}

// GenerateSymmetricKeys attempts to generate symmetric keys for all remote group
// members via ECDH. It generates this symmetric key for each remote group member
// by doing an ECDH between the ephemeral private key generated for a remote
// group member, and the public key for this member, generated and broadcasted by
// the remote group member.
//
// See Phase 2 of the protocol specification.
func (sm *SymmetricKeyGeneratingMember) GenerateSymmetricKeys(
	ephemeralPubKeyMessages []*EphemeralPublicKeyMessage,
) error {
	for _, ephemeralPubKeyMessage := range ephemeralPubKeyMessages {
		sm.evidenceLog.PutEphemeralMessage(ephemeralPubKeyMessage)

		otherMember := ephemeralPubKeyMessage.senderID

		// Find the ephemeral key pair generated by this group member for
		// the other group member.
		ephemeralKeyPair, ok := sm.ephemeralKeyPairs[otherMember]
		if !ok {
			return fmt.Errorf(
				"ephemeral key pair does not exist for member %v",
				otherMember,
			)
		}

		// Get the ephemeral private key generated by this group member for
		// the other group member.
		thisMemberEphemeralPrivateKey := ephemeralKeyPair.PrivateKey

		// Get the ephemeral public key broadcasted by the other group member,
		// which was intended for this group member.
		otherMemberEphemeralPublicKey := ephemeralPubKeyMessage.ephemeralPublicKeys[sm.ID]

		// Create symmetric key for the current group member and the other
		// group member by ECDH'ing the public and private key.
		symmetricKey := thisMemberEphemeralPrivateKey.Ecdh(
			otherMemberEphemeralPublicKey,
		)
		sm.symmetricKeys[otherMember] = symmetricKey
	}

	return nil
}

// CalculateMembersSharesAndCommitments starts with generating coefficients for
// two polynomials. It then calculates shares for all group member and packs
// them into a broadcast message. Individual shares inside the message are
// encrypted with the symmetric key of the indended share receiver.
// Additionally, it calculates commitments to `a` coefficients of first
// polynomial using second's polynomial `b` coefficients.
//
// If there are no symmetric keys established with all other group members,
// function yields an error.
//
// See Phase 3 of the protocol specification.
func (cm *CommittingMember) CalculateMembersSharesAndCommitments() (
	*PeerSharesMessage,
	*MemberCommitmentsMessage,
	error,
) {
	polynomialDegree := cm.group.dishonestThreshold
	coefficientsA, err := generatePolynomial(polynomialDegree, cm.protocolConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot generate polynomial [%v]", err)
	}
	coefficientsB, err := generatePolynomial(polynomialDegree, cm.protocolConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot generate hiding polynomial [%v]", err)
	}

	cm.secretCoefficients = coefficientsA

	// Calculate shares for other group members by evaluating polynomials defined
	// by coefficients `a_i` and `b_i`
	var sharesMessage = newPeerSharesMessage(cm.ID)
	for _, receiverID := range cm.group.MemberIDs() {
		// s_j = f_(j) mod q
		memberShareS := cm.evaluateMemberShare(receiverID, coefficientsA)
		// t_j = g_(j) mod q
		memberShareT := cm.evaluateMemberShare(receiverID, coefficientsB)

		// Check if calculated shares for the current member. If true store them
		// without sharing in a message.
		if cm.ID == receiverID {
			cm.selfSecretShareS = memberShareS
			cm.selfSecretShareT = memberShareT
			continue
		}

		// If there is no symmetric key established with the receiver, error is
		// returned.
		symmetricKey, hasKey := cm.symmetricKeys[receiverID]
		if !hasKey {
			return nil, nil, fmt.Errorf(
				"no symmetric key for receiver %v", receiverID,
			)
		}

		err := sharesMessage.addShares(
			receiverID,
			memberShareS,
			memberShareT,
			symmetricKey,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"could not add shares for receiver %v [%v]",
				receiverID,
				err,
			)
		}
	}

	commitments := make([]*big.Int, len(coefficientsA))
	for k := range commitments {
		// C_k = g^a_k * h^b_k mod p
		commitments[k] = cm.vss.CalculateCommitment(
			coefficientsA[k],
			coefficientsB[k],
			cm.protocolConfig.P,
		)
	}
	commitmentsMessage := &MemberCommitmentsMessage{
		senderID:    cm.ID,
		commitments: commitments,
	}

	return sharesMessage, commitmentsMessage, nil
}

// generatePolynomial generates a random polynomial over Z_q of a given degree.
// This function will generate a slice of `degree + 1` coefficients. Each value
// will be a random `big.Int` in range (0, q).
func generatePolynomial(degree int, dkg *DKG) ([]*big.Int, error) {
	coefficients := make([]*big.Int, degree+1)
	var err error
	for i := range coefficients {
		coefficients[i], err = dkg.RandomQ()
		if err != nil {
			return nil, err
		}
	}
	return coefficients, nil
}

// evaluateMemberShare calculates a share for given memberID.
//
// It calculates `s_j = Σ a_k * j^k mod q`for k in [0..T], where:
// - `a_k` is k coefficient
// - `j` is memberID
// - `T` is threshold
func (cm *CommittingMember) evaluateMemberShare(memberID MemberID, coefficients []*big.Int) *big.Int {
	result := big.NewInt(0)
	for k, a := range coefficients {
		result = new(big.Int).Mod(
			new(big.Int).Add(
				result,
				new(big.Int).Mul(
					a,
					pow(memberID, k),
				),
			),
			cm.protocolConfig.Q,
		)
	}
	return result
}

// VerifyReceivedSharesAndCommitmentsMessages verifies shares and commitments
// received in messages from peer group members.
// It returns accusation message with ID of members for which verification failed.
//
// If cannot match commitments message with shares message for given sender then
// error is returned. Also, error is returned if the member does not have
// a symmetric encryption key established with sender of a message.
//
// All the received PeerSharesMessage should be validated before they are passed
// to this function. It should never happen that the message can't be decrypted
// by this function.
//
// See Phase 4 of the protocol specification.
func (cvm *CommitmentsVerifyingMember) VerifyReceivedSharesAndCommitmentsMessages(
	sharesMessages []*PeerSharesMessage,
	commitmentsMessages []*MemberCommitmentsMessage,
) (*SecretSharesAccusationsMessage, error) {
	accusedMembersKeys := make(map[MemberID]*ephemeral.PrivateKey)
	for _, commitmentsMessage := range commitmentsMessages {
		// Find share message sent by the same member who sent commitment message
		sharesMessageFound := false
		for _, sharesMessage := range sharesMessages {
			if sharesMessage.senderID == commitmentsMessage.senderID {
				sharesMessageFound = true

				// If there is no symmetric key established with a sender of
				// the message, error is returned.
				symmetricKey, hasKey := cvm.symmetricKeys[sharesMessage.senderID]
				if !hasKey {
					return nil, fmt.Errorf(
						"no symmetric key for sender %v",
						sharesMessage.senderID,
					)
				}

				// Decrypt shares using symmetric key established with sender.
				// Since all the message are validated prior to passing to this
				// function, decryption error should never happen.
				shareS, err := sharesMessage.decryptShareS(cvm.ID, symmetricKey) // s_ji
				if err != nil {
					return nil, fmt.Errorf(
						"could not decrypt share S [%v]",
						err,
					)
				}
				shareT, err := sharesMessage.decryptShareT(cvm.ID, symmetricKey) // t_ji
				if err != nil {
					return nil, fmt.Errorf(
						"could not decrypt share T [%v]",
						err,
					)
				}

				// Check if `commitmentsProduct == expectedProduct`
				// `commitmentsProduct = Π (C_j[k] ^ (i^k)) mod p` for k in [0..T]
				// `expectedProduct = (g ^ s_ji) * (h ^ t_ji) mod p`
				// where: j is sender's ID, i is current member ID, T is threshold.
				if !cvm.areSharesValidAgainstCommitments(
					shareS, // s_ji
					shareT, // t_ji
					commitmentsMessage.commitments, // C_j
					cvm.ID, // i
				) {
					accusedMembersKeys[commitmentsMessage.senderID] = cvm.ephemeralKeyPairs[commitmentsMessage.senderID].PrivateKey
					break
				}
				cvm.receivedValidSharesS[commitmentsMessage.senderID] = shareS
				cvm.receivedValidSharesT[commitmentsMessage.senderID] = shareT
				cvm.receivedValidPeerCommitments[commitmentsMessage.senderID] = commitmentsMessage.commitments
				break
			}
		}
		if !sharesMessageFound {
			return nil, fmt.Errorf("cannot find shares message from member %v",
				commitmentsMessage.senderID,
			)
		}
	}

	return &SecretSharesAccusationsMessage{
		senderID:           cvm.ID,
		accusedMembersKeys: accusedMembersKeys,
	}, nil
}

// areSharesValidAgainstCommitments verifies if commitments are valid for passed
// shares.
//
// The `j` member generated a polynomial with `k` coefficients before. Then they
// calculated a commitments to the polynomial's coefficients `C_j` and individual
// shares `s_ji` and `t_ji` with a polynomial for a member `i`. In this function
// the verifier checks if the shares are valid against the commitments.
//
// The verifier checks that:
// `commitmentsProduct == expectedProduct`
// where:
// `commitmentsProduct = Π (C_j[k] ^ (i^k)) mod p` for k in [0..T],
// and
// `expectedProduct = (g ^ s_ji) * (h ^ t_ji) mod p`.
func (cm *CommittingMember) areSharesValidAgainstCommitments(
	shareS, shareT *big.Int, // s_ji, t_ji
	commitments []*big.Int, // C_j
	memberID MemberID, // i
) bool {
	// `commitmentsProduct = Π (C_j[k] ^ (i^k)) mod p`
	commitmentsProduct := big.NewInt(1)
	for k, c := range commitments {
		commitmentsProduct = new(big.Int).Mod(
			new(big.Int).Mul(
				commitmentsProduct,
				new(big.Int).Exp(
					c,
					pow(memberID, k),
					cm.protocolConfig.P,
				),
			),
			cm.protocolConfig.P,
		)
	}

	// `expectedProduct = (g ^ s_ji) * (h ^ t_ji) mod p`, where:
	expectedProduct := cm.vss.CalculateCommitment(
		shareS,
		shareT,
		cm.protocolConfig.P,
	)

	return expectedProduct.Cmp(commitmentsProduct) == 0
}

// ResolveSecretSharesAccusationsMessages resolves complaints received in
// secret shares accusations messages. The member calls this function to judge
// which party of the dispute is lying.
//
// The current member cannot be a part of a dispute. If the current member is
// either an accuser or is accused the function will return an error. The accused
// party cannot be a judge in its own case. From the other hand, the accuser has
// already performed the calculation in the previous phase which resulted in the
// accusation and waits now for a judgment from other players.
//
// This function needs to decrypt shares sent previously by the accused member
// to the accuser in an encrypted form. To do that it needs to recover a symmetric
// key used for data encryption. It takes private key revealed by the accuser
// and public key broadcasted by the accused and performs Elliptic Curve Diffie-
// Hellman operation between them.
//
// It returns IDs of members who should be disqualified. It will be an accuser
// if the validation shows that shares and commitments are valid, so the accusation
// was unfounded. Else it confirms that accused member misbehaved and their ID is
// added to the list.
//
// See Phase 5 of the protocol specification.
func (sjm *SharesJustifyingMember) ResolveSecretSharesAccusationsMessages(
	messages []*SecretSharesAccusationsMessage,
) ([]MemberID, error) {
	var disqualifiedMembers []MemberID
	for _, message := range messages {
		accuserID := message.senderID
		for accusedID, revealedAccuserPrivateKey := range message.accusedMembersKeys {
			if sjm.ID == accuserID || sjm.ID == accusedID {
				return nil, fmt.Errorf("current member cannot be a part of a dispute")
			}

			symmetricKey, err := recoverSymmetricKey(
				sjm.evidenceLog,
				accusedID,
				accuserID,
				revealedAccuserPrivateKey,
			)
			if err != nil {
				// TODO Should we disqualify accuser/accused member here?
				return nil, fmt.Errorf("could not recover symmetric key [%v]", err)
			}

			shareS, shareT, err := recoverShares(
				sjm.evidenceLog,
				accusedID,
				accuserID,
				symmetricKey,
			)
			if err != nil {
				// TODO Should we disqualify accuser/accused member here?
				return nil, fmt.Errorf("could not decrypt shares [%v]", err)
			}

			// Check if `commitmentsProduct == expectedProduct`
			// `commitmentsProduct = Π (C_m[k] ^ (j^k)) mod p` for k in [0..T]
			// `expectedProduct = (g ^ s_mj) * (h ^ t_mj) mod p`
			// where: m is accused member's ID, j is sender's ID, T is threshold.
			if sjm.areSharesValidAgainstCommitments(
				shareS, shareT, // s_mj, t_mj
				sjm.receivedValidPeerCommitments[accusedID], // C_m
				accuserID, // j
			) {
				disqualifiedMembers = append(disqualifiedMembers, accuserID)
			} else {
				disqualifiedMembers = append(disqualifiedMembers, accusedID)
			}
		}
	}
	return disqualifiedMembers, nil
}

// Recover ephemeral symmetric key used to encrypt communication between sender
// and receiver assuming that receiver revealed its private ephemeral key.
//
// Finds ephemeral public key sent by sender to the receiver. Performs ECDH
// operation between sender's public key and receiver's private key to recover
// the ephemeral symmetric key.
func recoverSymmetricKey(
	evidenceLog evidenceLog,
	senderID, receiverID MemberID,
	receiverPrivateKey *ephemeral.PrivateKey,
) (ephemeral.SymmetricKey, error) {
	ephemeralPublicKeyMessage := evidenceLog.ephemeralPublicKeyMessage(senderID)
	if ephemeralPublicKeyMessage == nil {
		return nil, fmt.Errorf(
			"no ephemeral public key message for sender %v",
			senderID,
		)
	}

	senderPublicKey, ok := ephemeralPublicKeyMessage.ephemeralPublicKeys[receiverID]
	if !ok {
		return nil, fmt.Errorf(
			"no ephemeral public key generated for receiver %v",
			receiverID,
		)
	}

	return receiverPrivateKey.Ecdh(senderPublicKey), nil
}

// Recovers from the evidence log share S and share T sent by sender to the
// receiver.
//
// First it finds in the evidence log the Peer Shares Message sent by the sender
// to the receiver. Then it decrypts the decrypted shares with provided symmetric
// key.
func recoverShares(
	evidenceLog evidenceLog,
	senderID, receiverID MemberID,
	symmetricKey ephemeral.SymmetricKey,
) (*big.Int, *big.Int, error) {
	peerSharesMessage := evidenceLog.peerSharesMessage(senderID)
	if peerSharesMessage == nil {
		return nil, nil, fmt.Errorf(
			"no peer shares message for sender %v",
			senderID,
		)
	}

	shareS, err := peerSharesMessage.decryptShareS(receiverID, symmetricKey) // s_mj
	if err != nil {
		// TODO Should we disqualify accuser/accused member here?
		return nil, nil, fmt.Errorf("cannot decrypt share S [%v]", err)
	}
	shareT, err := peerSharesMessage.decryptShareT(receiverID, symmetricKey) // t_mj
	if err != nil {
		// TODO Should we disqualify accuser/accused member here?
		return nil, nil, fmt.Errorf("cannot decrypt share T [%v]", err)
	}

	return shareS, shareT, nil
}

// CombineMemberShares sums up all `s` and `t` shares intended for this member.
// Combines secret shares calculated by current member `i` for itself `s_ii` with
// shares calculated by peer members `j` for this member `s_ji`.
//
// `x_i = Σ s_ji mod q` and `x'_i = Σ t_ji mod q` for `j` in a group of players
// who passed secret shares accusations stage.
//
// See Phase 6 of the protocol specification.
func (qm *QualifiedMember) CombineMemberShares() {
	combinedSharesS := qm.selfSecretShareS // s_ii
	for _, s := range qm.receivedValidSharesS {
		combinedSharesS = new(big.Int).Mod(
			new(big.Int).Add(combinedSharesS, s),
			qm.protocolConfig.Q,
		)
	}

	combinedSharesT := qm.selfSecretShareT // t_ii
	for _, t := range qm.receivedValidSharesT {
		combinedSharesT = new(big.Int).Mod(
			new(big.Int).Add(combinedSharesT, t),
			qm.protocolConfig.Q,
		)
	}

	qm.masterPrivateKeyShare = combinedSharesS
	qm.shareT = combinedSharesT
}

// CalculatePublicKeySharePoints calculates public values for member's coefficients.
// It calculates `A_k = g^a_k mod p` for k in [0..T].
//
// See Phase 7 of the protocol specification.
func (sm *SharingMember) CalculatePublicKeySharePoints() *MemberPublicKeySharePointsMessage {
	sm.publicKeySharePoints = make([]*big.Int, len(sm.secretCoefficients))
	for i, a := range sm.secretCoefficients {
		sm.publicKeySharePoints[i] = new(big.Int).Exp(
			sm.vss.G,
			a,
			sm.protocolConfig.P,
		)
	}

	return &MemberPublicKeySharePointsMessage{
		senderID:             sm.ID,
		publicKeySharePoints: sm.publicKeySharePoints,
	}
}

// VerifyPublicKeySharePoints validates public key share points received in
// messages from peer group members.
// It returns accusation message with ID of members for which the verification
// failed.
//
// See Phase 8 of the protocol specification.
func (sm *SharingMember) VerifyPublicKeySharePoints(
	messages []*MemberPublicKeySharePointsMessage,
) (*PointsAccusationsMessage, error) {
	accusedMembersKeys := make(map[MemberID]*ephemeral.PrivateKey)
	// `product = Π (A_jk ^ (i^k)) mod p` for k in [0..T],
	// where: j is sender's ID, i is current member ID, T is threshold.
	for _, message := range messages {
		if !sm.isShareValidAgainstPublicKeySharePoints(
			sm.ID,
			sm.receivedValidSharesS[message.senderID],
			message.publicKeySharePoints,
		) {
			accusedMembersKeys[message.senderID] = sm.ephemeralKeyPairs[message.senderID].PrivateKey
			continue
		}
		sm.receivedValidPeerPublicKeySharePoints[message.senderID] = message.publicKeySharePoints
	}

	return &PointsAccusationsMessage{
		senderID:           sm.ID,
		accusedMembersKeys: accusedMembersKeys,
	}, nil
}

// isShareValidAgainstPublicKeySharePoints verifies if public key share points
// are valid for passed share S.
//
// The `j` member calculated public key share points for their polynomial coefficients
// and share `s_ji` with a polynomial for a member `i`. In this function the
// verifier checks if the public key share points are valid against the share S.
//
// The verifier checks that:
// `product == expectedProduct`
// where:
// `product = Π (A_jk ^ (i^k)) mod p` for k in [0..T],
// and
// `expectedProduct = g^s_ji mod p`.
func (sm *SharingMember) isShareValidAgainstPublicKeySharePoints(
	senderID MemberID,
	shareS *big.Int,
	publicKeySharePoints []*big.Int,
) bool {
	// `product = Π (A_jk ^ (i^k)) mod p` for k in [0..T],
	// where: j is sender's ID, i is current member ID, T is threshold.
	product := big.NewInt(1)
	for k, a := range publicKeySharePoints {
		product = new(big.Int).Mod(
			new(big.Int).Mul(
				product,
				new(big.Int).Exp(
					a,
					pow(senderID, k),
					sm.protocolConfig.P,
				),
			),
			sm.protocolConfig.P,
		)
	}

	// `expectedProduct = g^s_ji mod p`, where:
	// where: j is sender's ID, i is current member ID.
	expectedProduct := new(big.Int).Exp(
		sm.vss.G,
		shareS,
		sm.protocolConfig.P,
	)

	return expectedProduct.Cmp(product) == 0
}

// ResolvePublicKeySharePointsAccusationsMessages resolves a complaint received
// in points accusations messages. The member calls this function to judge
// which party of the dispute is lying.
//
// The current member cannot be a part of a dispute. If the current member is
// either an accuser or is accused the function will return an error. The accused
// party cannot be a judge in its own case. From the other hand, the accuser has
// already performed the calculation in the previous phase which resulted in the
// accusation and waits now for a judgment from other players.
//
// This function needs to decrypt shares sent previously by the accused member
// to the accuser in an encrypted form. To do that it needs to recover a symmetric
// key used for data encryption. It takes private key revealed by the accuser
// and public key broadcasted by the accused and performs Elliptic Curve Diffie-
// Hellman operation between them.
//
// It returns IDs of members who should be disqualified. It will be an accuser
// if the validation shows that coefficients are valid, so the accusation was
// unfounded. Else it confirms that accused member misbehaved and their ID is
// added to the list.
//
// See Phase 9 of the protocol specification.
func (pjm *PointsJustifyingMember) ResolvePublicKeySharePointsAccusationsMessages(
	messages []*PointsAccusationsMessage,
) ([]MemberID, error) {
	var disqualifiedMembers []MemberID
	for _, message := range messages {
		accuserID := message.senderID
		for accusedID, revealedAccuserPrivateKey := range message.accusedMembersKeys {
			if pjm.ID == message.senderID || pjm.ID == accusedID {
				return nil, fmt.Errorf("current member cannot be a part of a dispute")
			}

			evidenceLog := pjm.evidenceLog

			recoveredSymmetricKey, err := recoverSymmetricKey(
				evidenceLog,
				accusedID,
				accuserID,
				revealedAccuserPrivateKey,
			)
			if err != nil {
				// TODO Should we disqualify accuser/accused member here?
				return nil, fmt.Errorf("could not recover symmetric key [%v]", err)
			}

			shareS, _, err := recoverShares(
				evidenceLog,
				accusedID,
				accuserID,
				recoveredSymmetricKey,
			)
			if err != nil {
				// TODO Should we disqualify accuser/accused member here?
				return nil, fmt.Errorf("could not decrypt share S [%v]", err)
			}

			if pjm.isShareValidAgainstPublicKeySharePoints(
				message.senderID,
				shareS,
				pjm.receivedValidPeerPublicKeySharePoints[accusedID],
			) {
				// TODO The accusation turned out to be unfounded. Should we add accused
				// member's individual public key to receivedValidPeerPublicKeySharePoints?
				disqualifiedMembers = append(disqualifiedMembers, message.senderID)
				continue
			}
			disqualifiedMembers = append(disqualifiedMembers, accusedID)
		}
	}
	return disqualifiedMembers, nil
}

// DisqualifiedShares contains shares `s_mk` calculated by the disqualified
// member `m` for peer members `k`. The shares were revealed due to disqualification
// of the member `m` from the protocol execution.
type DisqualifiedShares struct {
	disqualifiedMemberID MemberID              // m
	peerSharesS          map[MemberID]*big.Int // <k, s_mk>
}

// ReconstructIndividualPrivateKeys reconstructs disqualified members' individual
// private keys `z_m` from provided revealed shares calculated by disqualified
// members for peer members.
//
// Function need to be executed for qualified members that presented valid shares
// and commitments and were approved for Phase 6 but were disqualified on public
// key shares validation stage (Phase 9).
//
// It stores a map of reconstructed individual private keys for each disqualified
// member in a current member's reconstructedIndividualPrivateKeys field:
// <disqualifiedMemberID, privateKeyShare>
//
// See Phase 11 of the protocol specification.
func (rm *ReconstructingMember) ReconstructIndividualPrivateKeys(
	revealedDisqualifiedShares []*DisqualifiedShares,
) {
	rm.reconstructedIndividualPrivateKeys = make(map[MemberID]*big.Int, len(revealedDisqualifiedShares))

	for _, ds := range revealedDisqualifiedShares { // for each disqualified member
		// Reconstruct individual private key `z_m = Σ (s_mk * a_mk) mod q` where:
		// - `z_m` is disqualified member's individual private key
		// - `s_mk` is a share calculated by disqualified member `m` for peer member `k`
		// - `a_mk` is lagrange coefficient for peer member k (see below)
		individualPrivateKey := big.NewInt(0)
		// Get IDs of all peer members from disqualified shares.
		var peerIDs []MemberID
		for k := range ds.peerSharesS {
			peerIDs = append(peerIDs, k)
		}
		// For each peerID `k` and peerShareS `s_mk` calculate `s_mk * a_mk`
		for peerID, peerShareS := range ds.peerSharesS {
			// a_mk
			lagrangeCoefficient := rm.calculateLagrangeCoefficient(peerID, peerIDs)

			// Σ (s_mk * a_mk) mod q
			individualPrivateKey = new(big.Int).Mod(
				new(big.Int).Add(
					individualPrivateKey,
					// s_mk * a_mk
					new(big.Int).Mul(peerShareS, lagrangeCoefficient),
				),
				rm.protocolConfig.Q,
			)
		}
		// <m, z_m>
		rm.reconstructedIndividualPrivateKeys[ds.disqualifiedMemberID] =
			individualPrivateKey
	}
}

// Calculates Lagrange coefficient `a_mk` for member `k` in a group of members.
//
// `a_mk = Π (l / (l - k)) mod q` where:
// - `a_mk` is a lagrange coefficient for the member `k`,
// - `l` are IDs of members who provided shares,
// and `l != k`.
func (rm *ReconstructingMember) calculateLagrangeCoefficient(memberID MemberID, groupMembersIDs []MemberID) *big.Int {
	lagrangeCoefficient := big.NewInt(1)
	// For each otherID `l` in groupMembersIDs:
	for _, otherID := range groupMembersIDs {
		if otherID != memberID { // l != k
			// l / (l - k)
			quotient := new(big.Int).Mod(
				new(big.Int).Mul(
					big.NewInt(int64(otherID)),
					new(big.Int).ModInverse(
						new(big.Int).Sub(
							otherID.Int(),
							memberID.Int(),
						),
						rm.protocolConfig.Q,
					),
				),
				rm.protocolConfig.Q,
			)

			// Π (l / (l - k)) mod q
			lagrangeCoefficient = new(big.Int).Mod(
				new(big.Int).Mul(
					lagrangeCoefficient, quotient,
				),
				rm.protocolConfig.Q,
			)
		}
	}
	return lagrangeCoefficient // a_mk
}

// ReconstructIndividualPublicKeys calculates and stores individual public keys
// `y_m` from reconstructed individual private keys `z_m`.
//
// Public key is calculated as `g^privateKey mod p`.
//
// See Phase 11 of the protocol specification.
func (rm *ReconstructingMember) ReconstructIndividualPublicKeys() {
	rm.reconstructedIndividualPublicKeys = make(map[MemberID]*big.Int, len(rm.reconstructedIndividualPrivateKeys))
	for memberID, individualPrivateKey := range rm.reconstructedIndividualPrivateKeys {
		// `y_m = g^{z_m}`
		individualPublicKey := new(big.Int).Exp(
			rm.vss.G,
			individualPrivateKey,
			rm.protocolConfig.P,
		)
		rm.reconstructedIndividualPublicKeys[memberID] = individualPublicKey
	}
}

func pow(id MemberID, y int) *big.Int {
	return new(big.Int).Exp(id.Int(), big.NewInt(int64(y)), nil)
}

// CombineGroupPublicKey calculates a group public key by combining individual
// public keys. Group public key is calculated as a product of individual public
// keys of all group members including member themself.
//
// `Y = Π y_j mod p` for `j`, where `y_j` is individual public key of each qualified
// group member.
//
// This function combines individual public keys of all Qualified Members who were
// approved for Phase 6. Three categories of individual public keys are considered:
// 1. Current member's individual public key.
// 2. Peer members' individual public keys - for members who passed a public key
//    share points validation in Phase 8 and accusations resolution in Phase 9 and
//    are still active group members.
// 3. Disqualified members' individual public keys - for members who were disqualified
//    in Phase 9 and theirs individual private and public keys were reconstructed
//    in Phase 11.
//
// See Phase 12 of the protocol specification.
func (rm *CombiningMember) CombineGroupPublicKey() {
	// Current member's individual public key `A_i0`.
	groupPublicKey := rm.individualPublicKey()

	// Multiply received peer group members' individual public keys `A_j0`.
	for _, peerPublicKey := range rm.receivedValidPeerIndividualPublicKeys() {
		groupPublicKey = new(big.Int).Mod(
			new(big.Int).Mul(groupPublicKey, peerPublicKey),
			rm.protocolConfig.P,
		)
	}

	// Multiply reconstructed disqualified members' individual public keys `g^{z_m}`.
	for _, peerPublicKey := range rm.reconstructedIndividualPublicKeys {
		groupPublicKey = new(big.Int).Mod(
			new(big.Int).Mul(groupPublicKey, peerPublicKey),
			rm.protocolConfig.P,
		)
	}

	rm.groupPublicKey = groupPublicKey
}
