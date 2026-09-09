package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"gorm.io/datatypes"
)

type remotePriceDTO struct {
	Model       *string             `json:"model"`
	Type        *string             `json:"type"`
	ChannelType *int                `json:"channel_type"`
	Input       *float64            `json:"input"`
	Output      *float64            `json:"output"`
	Locked      *bool               `json:"locked"`
	ExtraRatios *map[string]float64 `json:"extra_ratios"`
	RateRules   json.RawMessage     `json:"rate_rules"`
	// model_info 是公开描述元数据，保持原始 JSON，避免其内部演进改变定价合同。
	ModelInfo json.RawMessage `json:"model_info"`
}

func decodeRemotePriceCatalog(data []byte) ([]*Price, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("remote price catalog is empty")
	}
	var entries []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := decodeStrictPriceJSON(trimmed, &entries); err != nil {
			return nil, err
		}
	case '{':
		var wrapper struct {
			Success *bool              `json:"success"`
			Message *string            `json:"message"`
			Version *int64             `json:"version"`
			Data    *[]json.RawMessage `json:"data"`
		}
		if err := decodeStrictPriceJSON(trimmed, &wrapper); err != nil {
			return nil, err
		}
		if wrapper.Data == nil {
			return nil, errors.New("remote price catalog data is required")
		}
		entries = *wrapper.Data
	default:
		return nil, errors.New("remote price catalog must be an array or data object")
	}
	if len(entries) == 0 {
		return nil, errors.New("remote price catalog must contain at least one price")
	}
	prices := make([]*Price, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index, raw := range entries {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("remote price catalog item %d must not be null", index)
		}
		var rawFields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rawFields); err != nil {
			return nil, fmt.Errorf("remote price catalog item %d: %w", index, err)
		}
		for name, value := range rawFields {
			if name == "model_info" {
				continue
			}
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fmt.Errorf("remote price catalog item %d field %q must not be null", index, name)
			}
		}
		var dto remotePriceDTO
		if err := decodeStrictPriceJSON(raw, &dto); err != nil {
			return nil, fmt.Errorf("remote price catalog item %d: %w", index, err)
		}
		price, err := dto.price()
		if err != nil {
			return nil, fmt.Errorf("remote price catalog item %d: %w", index, err)
		}
		if _, exists := seen[price.Model]; exists {
			return nil, fmt.Errorf("duplicate remote price model %q", price.Model)
		}
		seen[price.Model] = struct{}{}
		prices = append(prices, price)
	}
	return prices, nil
}

// DecodeRemotePriceCatalog parses the supplier catalog used by preview/apply.
// It is deliberately separate from the manual Price management request DTO.
func DecodeRemotePriceCatalog(data []byte) ([]*Price, error) {
	return decodeRemotePriceCatalog(data)
}

func (d remotePriceDTO) price() (*Price, error) {
	if d.Model == nil || strings.TrimSpace(*d.Model) == "" {
		return nil, errors.New("model is required")
	}
	if d.Type == nil || strings.TrimSpace(*d.Type) == "" {
		return nil, errors.New("type is required")
	}
	if d.Input == nil || d.Output == nil {
		return nil, errors.New("input and output are required; explicit zero is allowed")
	}
	price := &Price{Model: *d.Model, Type: *d.Type, Input: *d.Input, Output: *d.Output}
	if d.ChannelType != nil {
		price.ChannelType = *d.ChannelType
	}
	if d.Locked != nil {
		price.Locked = *d.Locked
	}
	if d.ExtraRatios != nil {
		extra := datatypes.NewJSONType(*d.ExtraRatios)
		price.ExtraRatios = &extra
	}
	if len(d.RateRules) > 0 {
		if bytes.Equal(bytes.TrimSpace(d.RateRules), []byte("null")) {
			return nil, errors.New("rate_rules must be an object; omit to preserve or use {} to clear")
		}
		var rules PriceRateRules
		if err := json.Unmarshal(d.RateRules, &rules); err != nil {
			return nil, fmt.Errorf("invalid rate_rules: %w", err)
		}
		encoded := datatypes.NewJSONType(rules)
		price.RateRules = &encoded
	}
	if err := price.prepareForPersistence(); err != nil {
		return nil, err
	}
	return price, nil
}

func decodeStrictPriceJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("JSON must contain one value")
		}
		return err
	}
	return nil
}
