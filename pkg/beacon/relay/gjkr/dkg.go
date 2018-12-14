package gjkr

import (
	crand "crypto/rand"
	"fmt"
	"math/big"
	"time"

	"github.com/keep-network/keep-core/pkg/beacon/relay/pedersen"
	"github.com/keep-network/paillier"
)

// DKG contains the configuration data needed for the DKG protocol execution.
type DKG struct {
	// P, Q are big primes, such that `p = 2q + 1`
	P, Q *big.Int

	// Pedersen VSS scheme used to calculate commitments.
	vss *pedersen.VSS

	// TODO evidenceLog placed here temporarily. Need to decide how it will be passed to protocol execution.
	evidenceLog evidenceLog
}

// GenerateDKG generates new DKG protocol configuration using randomly chosen
// safe prime.
//
// TODO: It's just a temporary solution. This function should be removed once
// we switch to elliptic curves.
func GenerateDKG() (*DKG, error) {
	bitLength := 256
	concurrencyLevel := 4
	timeout := 120 * time.Second

	p, q, err := paillier.GenerateSafePrime(
		bitLength,
		concurrencyLevel,
		timeout,
		crand.Reader,
	)
	if err != nil {
		return nil, fmt.Errorf("could not generate DKG paramters [%v]", err)
	}

	vss, err := pedersen.NewVSS(crand.Reader, p, q)
	if err != nil {
		return nil, fmt.Errorf("could not generate DKG paramters [%v]", err)
	}

	return &DKG{p, q, vss, newDkgEvidenceLog()}, nil
}

// RandomQ generates a random `big.Int` in range (0, q).
func (d *DKG) RandomQ() (*big.Int, error) {
	for {
		x, err := crand.Int(crand.Reader, d.Q)
		if err != nil {
			return nil, fmt.Errorf("failed to generate random number [%s]", err)
		}
		if x.Sign() > 0 {
			return x, nil
		}
	}
}
