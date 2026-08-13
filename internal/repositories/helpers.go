package repositories

import "errors"

var (
	ErrAPIKeyNotFound      = errors.New("API key not found")
	ErrRequestNotFound     = errors.New("request not found")
	ErrMetadataKeyNotFound = errors.New("metadata key not found")
	ErrProviderKeyNotFound = errors.New("provider key not found")
)
