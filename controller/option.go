package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"one-api/common/config"
	"one-api/model"
	"one-api/providers/codex"
	runtimeaffinity "one-api/runtime/channelaffinity"
	"one-api/safty"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type optionMutationRequest struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Inherit bool            `json:"inherit"`
}

type optionUpdateRequest struct {
	ExpectedVersion int64 `json:"expected_version"`
	optionMutationRequest
}

func normalizeOptionValue(raw any) (string, error) {
	switch value := raw.(type) {
	case string:
		return value, nil
	case json.Number:
		return value.String(), nil
	case bool:
		return strconv.FormatBool(value), nil
	default:
		return "", errors.New("unsupported option value type")
	}
}

func GetOptions(c *gin.Context) {
	snapshot := config.GlobalOption.RuntimeSnapshot()
	if snapshot == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "运行时配置尚未发布"})
		return
	}
	type publicOption struct {
		Key       string                     `json:"key"`
		Effective string                     `json:"effective"`
		Override  *string                    `json:"override"`
		Source    config.RuntimeOptionSource `json:"source"`
		Version   int64                      `json:"version"`
	}
	options := make([]publicOption, 0)
	sensitive := make(map[string]gin.H)
	for key := range snapshot.EffectiveValues() {
		value, ok := snapshot.Get(key)
		if !ok {
			continue
		}
		metadata, _ := config.GlobalOption.GetMetadata(key)
		switch metadata.Visibility {
		case config.OptionVisibilityPublic:
			options = append(options, publicOption{Key: key, Effective: value.Effective, Override: value.Override, Source: value.Source, Version: snapshot.Version()})
		case config.OptionVisibilitySensitive:
			sensitive[key] = gin.H{
				"configured": strings.TrimSpace(value.Effective) != "",
				"source":     value.Source,
				"version":    snapshot.Version(),
			}
		}
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Key < options[j].Key })
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    options,
		"version": snapshot.Version(),
		"meta": gin.H{
			"sensitive_options": sensitive,
		},
	})
	return
}

func GetSafeTools(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    safty.GetAllSafeToolsName(),
	})
	return
}

func GetChannelAffinityCache(c *gin.Context) {
	snapshot := config.GlobalOption.RuntimeSnapshot()
	settings := config.RuntimeChannelAffinitySettings(snapshot)
	waitMilliseconds := 0
	pollMilliseconds := 50
	if snapshot != nil {
		waitMilliseconds = snapshot.Int("PreferredChannelWaitMilliseconds", waitMilliseconds)
		pollMilliseconds = snapshot.Int("PreferredChannelWaitPollMilliseconds", pollMilliseconds)
	} else {
		waitMilliseconds = config.PreferredChannelWaitMilliseconds
		pollMilliseconds = config.PreferredChannelWaitPollMilliseconds
	}
	manager := runtimeaffinity.DefaultManager()
	stats := manager.Stats()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"enabled":                             settings.Enabled,
			"default_ttl_seconds":                 settings.DefaultTTLSeconds,
			"max_entries":                         settings.MaxEntries,
			"rules_count":                         len(settings.Rules),
			"rules":                               settings.Rules,
			"backend":                             stats.Backend,
			"local_entries":                       stats.LocalEntries,
			"backend_entries":                     stats.BackendEntries,
			"preferred_channel_wait_milliseconds": waitMilliseconds,
			"preferred_channel_wait_poll_milliseconds": pollMilliseconds,
		},
	})
}

func ClearChannelAffinityCache(c *gin.Context) {
	manager := runtimeaffinity.DefaultManager()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"cleared": manager.Clear(),
		},
	})
}

func GetExecutionSessionCache(c *gin.Context) {
	stats := codex.GetExecutionSessionStats()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"backend":                 stats.Backend,
			"local_sessions":          stats.LocalSessions,
			"local_bindings":          stats.LocalBindings,
			"backend_bindings":        stats.BackendBindings,
			"max_sessions":            stats.MaxSessions,
			"max_sessions_per_caller": stats.MaxSessionsPerCaller,
			"default_ttl_seconds":     stats.DefaultTTLSeconds,
			"managed_provider":        "codex",
		},
	})
}

func UpdateOption(c *gin.Context) {
	var request optionUpdateRequest
	if err := decodeOptionRequest(c, &request); err != nil {
		writeInvalidOptionRequest(c)
		return
	}

	mutation, err := optionRequestMutation(request.Key, request.Value, request.Inherit)
	if err != nil || request.ExpectedVersion < 1 {
		writeInvalidOptionRequest(c)
		return
	}
	version, err := model.ApplyOptionMutations(c.Request.Context(), request.ExpectedVersion, []model.OptionMutation{mutation})
	if err != nil {
		writeOptionUpdateFailure(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"version": version,
	})
}

func optionRequestMutation(key string, raw json.RawMessage, inherit bool) (model.OptionMutation, error) {
	if inherit {
		if len(bytes.TrimSpace(raw)) != 0 {
			return model.OptionMutation{}, errors.New("inherit cannot include value")
		}
		return model.OptionMutation{Key: key, Inherit: true}, nil
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return model.OptionMutation{}, errors.New("value is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return model.OptionMutation{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return model.OptionMutation{}, errors.New("value must contain one JSON value")
	}
	value, err := normalizeOptionValue(decoded)
	if err != nil {
		return model.OptionMutation{}, err
	}
	return model.OptionMutation{Key: key, Value: &value}, nil
}
