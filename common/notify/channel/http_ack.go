package channel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const maxNotificationACKBytes int64 = 64 << 10

func decodeNotificationACK(resp *http.Response, destination any) error {
	if resp == nil || resp.Body == nil {
		return errors.New("notification response body is missing")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNotificationACKBytes+1))
	if err != nil {
		return fmt.Errorf("read notification response: %w", err)
	}
	if int64(len(body)) > maxNotificationACKBytes {
		return fmt.Errorf("notification response exceeds %d bytes", maxNotificationACKBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode notification acknowledgement: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("notification acknowledgement contains trailing JSON")
		}
		return fmt.Errorf("decode notification acknowledgement trailer: %w", err)
	}
	return nil
}
