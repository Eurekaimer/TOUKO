package domain

import (
	"errors"
	"fmt"
)

var (
	ErrAssetNotFound           = errors.New("asset not found")
	ErrAmbiguousSelector       = errors.New("ambiguous selector")
	ErrConflict                = errors.New("conflict")
	ErrRepositoryCorrupt       = errors.New("repository corrupt")
	ErrHashMismatch            = errors.New("hash mismatch")
	ErrUnauthorized            = errors.New("unauthorized")
	ErrRepositoryUninitialized = errors.New("repository uninitialized; run repo init")
	ErrBusy                    = errors.New("state directory busy")
	ErrDeliveryUnavailable     = errors.New("delivery unavailable")
)

type ConflictError struct {
	Base    RepositoryHead
	Remote  RepositoryHead
	AssetID *AssetID
	Reason  string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s: %s (base=%s remote=%s)", ErrConflict, e.Reason, e.Base, e.Remote)
}
func (e *ConflictError) Unwrap() error { return ErrConflict }
