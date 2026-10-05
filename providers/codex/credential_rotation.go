package codex

import (
	"context"
	"one-api/common/config"
	"one-api/common/credentials"
	"one-api/model"
)

var CredentialRotation = credentials.Service{Store: model.ChannelCredentialStore{}}

// RecoverCredential only accepts an independently authorized result for the
// same account and the exact channel snapshot captured by the OAuth session.
func RecoverCredential(ctx context.Context, id int, version uint64, attemptID, accountID, key string) error {
	if accountID == "" || CodexCredentialAccountID(key) != accountID {
		return credentials.ErrConflict
	}
	row, err := CredentialRotation.Store.Load(ctx, id)
	if err != nil {
		return err
	}
	if row.Type != config.ChannelTypeCodex || row.Version != version || CodexCredentialAccountID(row.Key) != accountID {
		return credentials.ErrConflict
	}
	return CredentialRotation.Recover(ctx, row, attemptID, key)
}
