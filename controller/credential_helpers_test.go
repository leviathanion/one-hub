package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"one-api/common/credentials"
	"one-api/model"
	"testing"

	"github.com/gin-gonic/gin"
)

func testRotation() credentials.Service {
	return credentials.Service{Store: model.ChannelCredentialStore{}}
}

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
	refresh, err := credentials.ReadRefresh(row.BizData)
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

// Existing edit fixtures represent a freshly loaded form. Stale/missing-version
// regression tests construct their own requests without this fixture helper.
func attachChannelEditVersion(t *testing.T, c *gin.Context, tag bool) {
	t.Helper()
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}
	if tag {
		rows, err := model.GetChannelsByTag(c.Param("tag"))
		if err != nil {
			t.Fatal(err)
		}
		versions := map[int]uint64{}
		for _, row := range rows {
			versions[row.Id] = row.Version
		}
		payload["expected_versions"], _ = json.Marshal(versions)
	} else {
		var id int
		_ = json.Unmarshal(payload["id"], &id)
		row, err := model.GetChannelById(id)
		if err == nil {
			payload["expected_version"], _ = json.Marshal(row.Version)
		}
	}
	raw, _ = json.Marshal(payload)
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
}
