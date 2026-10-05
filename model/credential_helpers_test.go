package model

import (
	"context"
	"one-api/common/credentials"
)

func testRotation() credentials.Service { return credentials.Service{Store: ChannelCredentialStore{}} }

type rotationTestSnapshot struct {
	credentials.Snapshot
	Fence     *string
	StartedAt *int64
}

func loadRotationTestSnapshot(ctx context.Context, id int) (rotationTestSnapshot, error) {
	row, err := testRotation().Store.Load(ctx, id)
	result := rotationTestSnapshot{Snapshot: row}
	if err != nil {
		return result, err
	}
	refresh, err := credentials.ReadRefresh(row.InternalState)
	if refresh != nil {
		result.Fence = &refresh.AttemptID
		result.StartedAt = &refresh.StartedAt
	}
	return result, err
}
func rotationTestData(attempt *string, started *int64) []byte {
	if attempt == nil {
		return nil
	}
	refresh := &credentials.Refresh{AttemptID: *attempt}
	if started != nil {
		refresh.StartedAt = *started
	}
	raw, err := credentials.WriteRefresh(nil, refresh)
	if err != nil {
		panic(err)
	}
	return raw
}
