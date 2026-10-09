// Package vip provides related functionality.
package vip

import stderrors "errors"

var (
	ErrFreeVIPAlreadyClaimed  = stderrors.New("free vip already claimed")
	ErrTrialVIPAlreadyClaimed = stderrors.New("trial vip already claimed")
	ErrTrialVIPDisabled       = stderrors.New("trial vip disabled")
	ErrInvalidVIP             = stderrors.New("invalid vip")
)
