package ethash

import (
	"errors"

	"github.com/ledgerwatch/turbo-geth/consensus"
	"github.com/ledgerwatch/turbo-geth/core/types"
)

func (ethash *Ethash) Verify(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header, uncle bool, seal bool) error {
	if len(parents) == 0 {
		return errors.New("need a parent to verify the header")
	}
	return ethash.verifyHeader(chain, header, parents[0], uncle, seal)
}

func (ethash *Ethash) NeededForVerification(_ *types.Header) int {
	return 1
}

func (ethash *Ethash) IsFake() bool {
	return ethash.config.PowMode == ModeFullFake
}
