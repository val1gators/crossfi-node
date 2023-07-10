package keeper

import (
	"fmt"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	"github.com/mineplexio/mineplex-2-node/x/gravity/types"
)

// this file contains code related to custom governance proposals

func RegisterProposalTypes() {
	// use of prefix stripping to prevent a typo between the proposal we check
	// and the one we register, any issues with the registration string will prevent
	// the proposal from working. We must check for double registration so that cli commands
	// submitting these proposals work.
	// For some reason the cli code is run during app.go startup, but of course app.go is not
	// run during operation of one off tx commands, so we need to run this 'twice'
	prefix := "gravity/"
	metadata := "gravity/IBCMetadata"
	if !govtypes.IsValidProposalType(strings.TrimPrefix(metadata, prefix)) {
		govtypes.RegisterProposalType(types.ProposalTypeIBCMetadata)
		// nolint: exhaustruct
		//govtypes.RegisterProposalTypeCodec(&types.IBCMetadataProposal{}, metadata)
	}
	unhalt := "gravity/UnhaltBridge"
	if !govtypes.IsValidProposalType(strings.TrimPrefix(unhalt, prefix)) {
		govtypes.RegisterProposalType(types.ProposalTypeUnhaltBridge)
		// nolint: exhaustruct
		//govtypes.RegisterProposalTypeCodec(&types.UnhaltBridgeProposal{}, unhalt)
	}
	airdrop := "gravity/Airdrop"
	if !govtypes.IsValidProposalType(strings.TrimPrefix(airdrop, prefix)) {
		govtypes.RegisterProposalType(types.ProposalTypeAirdrop)
		// nolint: exhaustruct
		//govtypes.RegisterProposalTypeCodec(&types.AirdropProposal{}, airdrop)
	}
}

func NewGravityProposalHandler(k Keeper) govtypes.Handler {
	return func(ctx sdk.Context, content govtypes.Content) error {
		switch c := content.(type) {
		case *types.UnhaltBridgeProposal:
			return k.HandleUnhaltBridgeProposal(ctx, c)
		case *types.AirdropProposal:
			return k.HandleAirdropProposal(ctx, c)

		default:
			return sdkerrors.Wrapf(sdkerrors.ErrUnknownRequest, "unrecognized Gravity proposal content type: %T", c)
		}
	}
}

// Unhalt Bridge specific functions

// In the event the bridge is halted and governance has decided to reset oracle
// history, we roll back oracle history and reset the parameters
func (k Keeper) HandleUnhaltBridgeProposal(ctx sdk.Context, p *types.UnhaltBridgeProposal) error {
	ctx.Logger().Info("Gov vote passed: Resetting oracle history", "nonce", p.TargetNonce)
	pruneAttestationsAfterNonce(ctx, types.ChainID(p.ChainId), k, p.TargetNonce)
	return nil
}

// Iterate over all attestations currently being voted on in order of nonce
// and prune those that are older than nonceCutoff
func pruneAttestationsAfterNonce(ctx sdk.Context, chainID types.ChainID, k Keeper, nonceCutoff uint64) {
	// Decide on the most recent nonce we can actually roll back to
	lastObserved := k.GetLastObservedEventNonce(ctx, chainID)
	if nonceCutoff < lastObserved || nonceCutoff == 0 {
		ctx.Logger().Error("Attempted to reset to a nonce before the last \"observed\" event, which is not allowed", "lastObserved", lastObserved, "nonce", nonceCutoff)
		return
	}

	// Get relevant event nonces
	attmap, keys := k.GetAttestationMapping(ctx, chainID)

	// Discover all affected validators whose LastEventNonce must be reset to nonceCutoff

	numValidators := len(k.StakingKeeper.GetBondedValidatorsByPower(ctx))
	// void and setMember are necessary for sets to work
	type void struct{}
	var setMember void
	// Initialize a Set of validators
	affectedValidatorsSet := make(map[string]void, numValidators)

	// Delete all reverted attestations, keeping track of the validators who attested to any of them
	for _, nonce := range keys {
		for _, att := range attmap[nonce] {
			// we delete all attestations earlier than the cutoff event nonce
			if nonce > nonceCutoff {
				ctx.Logger().Info(fmt.Sprintf("Deleting attestation at height %v", att.Height))
				for _, vote := range att.Votes {
					if _, ok := affectedValidatorsSet[vote]; !ok { // if set does not contain vote
						affectedValidatorsSet[vote] = setMember // add key to set
					}
				}

				k.DeleteAttestation(ctx, att)
			}
		}
	}

	// Reset the last event nonce for all validators affected by history deletion
	for vote := range affectedValidatorsSet {
		val, err := sdk.ValAddressFromBech32(vote)
		if err != nil {
			panic(sdkerrors.Wrap(err, "invalid validator address affected by bridge reset"))
		}
		valLastNonce := k.GetLastEventNonceByValidator(ctx, chainID, val)
		if valLastNonce > nonceCutoff {
			ctx.Logger().Info("Resetting validator's last event nonce due to bridge unhalt", "validator", vote, "lastEventNonce", valLastNonce, "resetNonce", nonceCutoff)
			k.SetLastEventNonceByValidator(ctx, chainID, val, nonceCutoff)
		}
	}
}

// Allows governance to deploy an airdrop to a provided list of addresses
func (k Keeper) HandleAirdropProposal(ctx sdk.Context, p *types.AirdropProposal) error {
	ctx.Logger().Info("Gov vote passed: Performing airdrop")
	startingSupply := k.bankKeeper.GetSupply(ctx, p.Denom)

	validateDenom := sdk.ValidateDenom(p.Denom)
	if validateDenom != nil {
		ctx.Logger().Info("Airdrop failed to execute invalid denom!")
		return sdkerrors.Wrap(types.ErrInvalid, "Invalid airdrop denom")
	}

	feePool := k.DistKeeper.GetFeePool(ctx)
	feePoolAmount := feePool.CommunityPool.AmountOf(p.Denom)

	airdropTotal := sdk.NewInt(0)
	for _, v := range p.Amounts {
		airdropTotal = airdropTotal.Add(sdk.NewIntFromUint64(v))
	}

	totalRequiredDecCoin := sdk.NewDecCoinFromCoin(sdk.NewCoin(p.Denom, airdropTotal))

	// check that we have enough tokens in the community pool to actually execute
	// this airdrop with the provided recipients list
	totalRequiredDec := totalRequiredDecCoin.Amount
	if totalRequiredDec.GT(feePoolAmount) {
		ctx.Logger().Info("Airdrop failed to execute insufficient tokens in the community pool!")
		return sdkerrors.Wrap(types.ErrInvalid, "Insufficient tokens in community pool")
	}

	// we're packing addresses as 20 bytes rather than valid bech32 in order to maximize participants
	// so if the recipients list is not a multiple of 20 it must be invalid
	numRecipients := len(p.Recipients) / 20
	if len(p.Recipients)%20 != 0 || numRecipients != len(p.Amounts) {
		ctx.Logger().Info("Airdrop failed to execute invalid recipients")
		return sdkerrors.Wrap(types.ErrInvalid, "Invalid recipients")
	}

	parsedRecipients := make([]sdk.AccAddress, len(p.Recipients)/20)
	for i := 0; i < numRecipients; i++ {
		indexStart := i * 20
		indexEnd := indexStart + 20
		addr := p.Recipients[indexStart:indexEnd]
		parsedRecipients[i] = addr
	}

	// check again, just in case the above modulo math is somehow wrong or spoofed
	if len(parsedRecipients) != len(p.Amounts) {
		ctx.Logger().Info("Airdrop failed to execute invalid recipients")
		return sdkerrors.Wrap(types.ErrInvalid, "Invalid recipients")
	}

	// the total amount actually sent in dec coins
	totalSent := sdk.NewDec(0)
	for i, addr := range parsedRecipients {
		usersAmount := p.Amounts[i]
		usersIntAmount := sdk.NewIntFromUint64(usersAmount)
		usersDecAmount := sdk.NewDecFromInt(usersIntAmount)
		err := k.bankKeeper.SendCoinsFromModuleToAccount(ctx, disttypes.ModuleName, addr, sdk.NewCoins(sdk.NewCoin(p.Denom, usersIntAmount)))
		// if there is no error we add to the total actually sent
		if err == nil {
			totalSent = totalSent.Add(usersDecAmount)
		} else {
			// return an err to prevent execution from finishing, this will prevent the changes we
			// have made so far from taking effect the governance proposal will instead time out
			ctx.Logger().Info("invalid address in airdrop! not executing", "address", addr)
			return err
		}
	}

	if !totalRequiredDecCoin.Amount.Equal(totalSent) {
		ctx.Logger().Info("Airdrop failed to execute Invalid amount sent", "sent", totalRequiredDecCoin.Amount, "expected", totalSent)
		return sdkerrors.Wrap(types.ErrInvalid, "Invalid amount sent")
	}

	newCoins, InvalidModuleBalance := feePool.CommunityPool.SafeSub(sdk.NewDecCoins(totalRequiredDecCoin))
	// this shouldn't ever happen because we check that we have enough before starting
	// but lets be conservative.
	if InvalidModuleBalance {
		return sdkerrors.Wrap(types.ErrInvalid, "internal error!")
	}
	feePool.CommunityPool = newCoins
	k.DistKeeper.SetFeePool(ctx, feePool)

	endingSupply := k.bankKeeper.GetSupply(ctx, p.Denom)
	if !startingSupply.Equal(endingSupply) {
		return sdkerrors.Wrap(types.ErrInvalid, "total chain supply has changed!")
	}

	return nil
}
