// Package family provides related functionality.
package family

import "errors"

var (
	ErrAlreadyInFamily        = errors.New("already in a normal family")
	ErrFamilyNotFound         = errors.New("family not found")
	ErrTargetIsPersonalFamily = errors.New("cannot join personal family")
	ErrAlreadyInTargetFamily  = errors.New("already in target family")
	ErrNotInNormalFamily      = errors.New("not in a normal family")
	ErrOwnerCannotLeave       = errors.New("owner cannot leave family")
	ErrFamilyFull             = errors.New("family is full")
	ErrCannotRemoveSelf       = errors.New("cannot remove self")
	ErrCannotRemoveOwner      = errors.New("cannot remove family owner")
	ErrNotOwner               = errors.New("not family owner")
	ErrTargetNotInFamily      = errors.New("target user not in family")
	ErrCannotDissolvePersonal = errors.New("cannot dissolve personal family")
	ErrOperationInProgress    = errors.New("operation in progress")
	ErrRemovedRejoinCooldown  = errors.New("removed from this family recently, try again later")
)
