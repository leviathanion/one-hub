package controller

import (
	"context"
	"errors"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	"one-api/providers/codex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// Keep this hard limit server-side. Large admin views should batch on the client
	// instead of turning one request into an unbounded server-side fan-out.
	codexUsagePreviewMaxIDs      = 100
	codexUsagePreviewConcurrency = 4
	codexUsagePreviewTimeout     = 5 * time.Second
	codexUsageDetailFetchTimeout = 15 * time.Second
)

type codexUsagePreviewRequest struct {
	IDs []int `json:"ids" binding:"required"`
}

type codexUsagePreviewItem struct {
	ChannelID int                      `json:"channel_id"`
	OK        bool                     `json:"ok"`
	Preview   *codex.CodexUsagePreview `json:"preview,omitempty"`
	Error     string                   `json:"error,omitempty"`
}

type codexUsageProvider interface {
	GetUsagePreview(ctx context.Context, forceRefresh bool) (*codex.CodexUsagePreview, error)
	GetUsageSnapshot(ctx context.Context, forceRefresh bool) (*codex.CodexUsageSnapshot, error)
}

type codexUsagePreviewTarget struct {
	ResultIndex int
	ChannelID   int
	Provider    codexUsageProvider
}

var (
	loadCodexUsageChannelByID  = model.GetChannelById
	loadCodexUsageChannelsByID = model.GetChannelsByIDs
	createCodexUsageProvider   = func(channel *model.Channel) (codexUsageProvider, error) {
		provider, ok := codex.CodexProviderFactory{}.Create(channel).(*codex.CodexProvider)
		if !ok || provider == nil {
			return nil, errors.New("failed to initialize Codex provider")
		}
		return provider, nil
	}
)

func GetCodexChannelUsage(c *gin.Context) {
	channelID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	provider, err := buildCodexUsageProviderByChannelID(channelID)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	refreshRequested := parseRefreshQuery(c.Query("refresh"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), codexUsageDetailFetchTimeout)
	defer cancel()

	snapshot, err := provider.GetUsageSnapshot(ctx, refreshRequested)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
			"data":    snapshot,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    snapshot,
	})
}

func GetCodexUsagePreviews(c *gin.Context) {
	var req codexUsagePreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	ids, err := normalizeCodexUsagePreviewIDs(req.IDs)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	targets, results, err := buildCodexUsagePreviewTargets(ids)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	sem := make(chan struct{}, codexUsagePreviewConcurrency)
	var wg sync.WaitGroup

	for _, target := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(target codexUsagePreviewTarget) {
			defer wg.Done()
			defer func() {
				<-sem
			}()

			ctx, cancel := context.WithTimeout(c.Request.Context(), codexUsagePreviewTimeout)
			defer cancel()

			preview, previewErr := target.Provider.GetUsagePreview(ctx, false)
			if previewErr != nil {
				results[target.ResultIndex] = codexUsagePreviewItem{
					ChannelID: target.ChannelID,
					Error:     previewErr.Error(),
				}
				return
			}

			results[target.ResultIndex] = codexUsagePreviewItem{
				ChannelID: target.ChannelID,
				OK:        true,
				Preview:   preview,
			}
		}(target)
	}

	wg.Wait()

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"items": results,
		},
	})
}

func buildCodexUsageProviderByChannelID(channelID int) (codexUsageProvider, error) {
	channel, err := loadCodexUsageChannel(channelID)
	if err != nil {
		return nil, err
	}
	return createCodexUsageProvider(channel)
}

func buildCodexUsageProvider(channel *model.Channel) (codexUsageProvider, error) {
	if err := validateCodexUsageChannel(channel); err != nil {
		return nil, err
	}
	return createCodexUsageProvider(channel)
}

func loadCodexUsageChannel(channelID int) (*model.Channel, error) {
	channel, err := loadCodexUsageChannelByID(channelID)
	if err != nil {
		return nil, err
	}
	if err = validateCodexUsageChannel(channel); err != nil {
		return nil, err
	}
	return channel, nil
}

func buildCodexUsagePreviewTargets(ids []int) ([]codexUsagePreviewTarget, []codexUsagePreviewItem, error) {
	channels, err := loadCodexUsageChannelsByID(ids)
	if err != nil {
		return nil, nil, err
	}

	channelMap := make(map[int]*model.Channel, len(channels))
	for _, channel := range channels {
		if channel != nil {
			channelMap[channel.Id] = channel
		}
	}

	results := make([]codexUsagePreviewItem, len(ids))
	targets := make([]codexUsagePreviewTarget, 0, len(ids))

	for index, channelID := range ids {
		channel, ok := channelMap[channelID]
		if !ok {
			results[index] = codexUsagePreviewItem{
				ChannelID: channelID,
				Error:     "channel not found",
			}
			continue
		}

		provider, err := buildCodexUsageProvider(channel)
		if err != nil {
			results[index] = codexUsagePreviewItem{
				ChannelID: channelID,
				Error:     err.Error(),
			}
			continue
		}

		targets = append(targets, codexUsagePreviewTarget{
			ResultIndex: index,
			ChannelID:   channelID,
			Provider:    provider,
		})
	}

	return targets, results, nil
}

// Tag is grouping metadata for real child channels. A tagged Codex channel still
// represents a concrete credential set and remains a valid usage query target.
func validateCodexUsageChannel(channel *model.Channel) error {
	if channel == nil {
		return errors.New("channel not found")
	}
	if channel.Type != config.ChannelTypeCodex {
		return errors.New("channel type is not Codex")
	}
	if strings.TrimSpace(channel.Key) == "" {
		return errors.New("Codex channel credentials are empty")
	}
	return nil
}

func normalizeCodexUsagePreviewIDs(ids []int) ([]int, error) {
	if len(ids) == 0 {
		return nil, errors.New("ids不能为空")
	}

	normalized := make([]int, 0, len(ids))
	seen := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}

	if len(normalized) == 0 {
		return nil, errors.New("ids不能为空")
	}
	if len(normalized) > codexUsagePreviewMaxIDs {
		return nil, errors.New("ids不能超过100个")
	}

	return normalized, nil
}

func parseRefreshQuery(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
