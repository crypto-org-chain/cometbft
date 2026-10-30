package state

import (
	"fmt"
	"time"

	"github.com/cometbft/cometbft/internal/mediantime"
	"github.com/cometbft/cometbft/types"
	cmttime "github.com/cometbft/cometbft/types/time"
)

// MedianTimeActivationHeight returns the height from which block time skips
// Nil precommits on chainID. ok is false for chains that always count them.
// Exported so downstream binaries can report it, e.g. in version output.
func MedianTimeActivationHeight(chainID string) (height int64, ok bool) {
	return mediantime.ActivationHeight(chainID)
}

// ValidateMedianTimeActivation fails for a known chain whose activation height
// is unset in this build, so a node refuses to start before it validates blocks.
func ValidateMedianTimeActivation(chainID string) error {
	if height, ok := mediantime.ActivationHeight(chainID); ok && height <= 0 {
		return errMedianTimeActivationUnset(chainID)
	}
	return nil
}

func errMedianTimeActivationUnset(chainID string) error {
	return fmt.Errorf("median time activation height for chain %q is not set in this build", chainID)
}

// blockMedianTime picks the median time rule for the block at height, so the
// proposer and every validator agree on it. A known chain with an unset height
// fails here too, for paths that never run the node startup check.
func blockMedianTime(state State, height int64, commit *types.Commit) (time.Time, error) {
	activation, ok := mediantime.ActivationHeight(state.ChainID)
	if ok && activation <= 0 {
		return time.Time{}, errMedianTimeActivationUnset(state.ChainID)
	}
	if ok && height >= activation {
		return MedianTime(commit, state.LastValidators)
	}
	return medianTimeWithNil(commit, state.LastValidators)
}

// medianTimeWithNil is the v0.39 rule: Nil precommits count, Absent ones don't.
func medianTimeWithNil(commit *types.Commit, validators *types.ValidatorSet) (time.Time, error) {
	return weightedMedianTime(commit, validators, func(flag types.BlockIDFlag) bool {
		return flag != types.BlockIDFlagAbsent
	})
}

func weightedMedianTime(
	commit *types.Commit,
	validators *types.ValidatorSet,
	counts func(types.BlockIDFlag) bool,
) (time.Time, error) {
	weightedTimes := make([]*cmttime.WeightedTime, len(commit.Signatures))
	totalVotingPower := int64(0)

	for i, commitSig := range commit.Signatures {
		if !counts(commitSig.BlockIDFlag) {
			continue
		}
		_, validator := validators.GetByAddress(commitSig.ValidatorAddress)
		// If there's no condition, TestValidateBlockCommit panics; not needed normally.
		if validator == nil {
			return time.Time{}, fmt.Errorf("commit validator not found in validator set: %X",
				commitSig.ValidatorAddress)
		}
		totalVotingPower += validator.VotingPower
		weightedTimes[i] = cmttime.NewWeightedTime(commitSig.Timestamp, validator.VotingPower)
	}

	return cmttime.WeightedMedian(weightedTimes, totalVotingPower), nil
}
