package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/utils"
	"one-api/model"
	"one-api/providers/codex"

	"github.com/gin-gonic/gin"
)

type codexAuthFileResponse struct {
	CredentialsJSON string `json:"credentials"`
	Email           string `json:"email,omitempty"`
	AccountID       string `json:"account_id,omitempty"`
	SuggestedName   string `json:"suggested_name,omitempty"`
}

// ParseCodexAuthFile normalizes a Codex auth file and returns channel-ready credentials JSON.
func ParseCodexAuthFile(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("file is required"))
		return
	}

	parsed, err := parseUploadedCodexAuthFile(fileHeader)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	credentialsJSON, err := parsed.CredentialsJSON()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": codexAuthFileResponse{
			CredentialsJSON: credentialsJSON,
			Email:           parsed.Email,
			AccountID:       parsed.Credentials.AccountID,
			SuggestedName:   parsed.DisplayLabel(),
		},
	})
}

// ImportCodexAuthFiles creates Codex channels directly from uploaded auth files.
func ImportCodexAuthFiles(c *gin.Context) {
	templateRaw, parsedFiles, err := readCodexAuthFilesImportParts(c)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	template, err := parseCodexChannelTemplate(templateRaw)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	template.Type = config.ChannelTypeCodex
	template.CreatedTime = utils.GetTimestamp()

	channels := make([]model.Channel, 0, len(parsedFiles))
	for index, parsed := range parsedFiles {
		credentialsJSON, err := parsed.CredentialsJSON()
		if err != nil {
			common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("%s: %w", parsed.FileName, err))
			return
		}

		channel := template
		channel.Key = credentialsJSON
		channel.Name = buildImportedCodexChannelName(template.Name, parsed, index, len(parsedFiles))
		channels = append(channels, channel)
	}

	if err := model.BatchInsertChannels(channels); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"count": len(channels),
		},
	})
}

func parseCodexChannelTemplate(raw string) (model.Channel, error) {
	var channel model.Channel
	if strings.TrimSpace(raw) == "" {
		return channel, fmt.Errorf("channel template is required")
	}
	if err := json.Unmarshal([]byte(raw), &channel); err != nil {
		return channel, fmt.Errorf("invalid channel template: %w", err)
	}

	channel.Id = 0
	channel.Key = ""
	channel.Type = config.ChannelTypeCodex
	channel.CreatedTime = 0
	channel.TestTime = 0
	channel.ResponseTime = 0
	channel.Balance = 0
	channel.BalanceUpdatedTime = 0
	channel.UsedQuota = 0

	if !hasConfiguredModels(channel.Models) {
		return channel, fmt.Errorf("models cannot be empty")
	}
	if err := channel.CanonicalizeRuntimeConfigJSON(); err != nil {
		return channel, err
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		return channel, err
	}

	return channel, nil
}

func hasConfiguredModels(models string) bool {
	for _, modelName := range strings.Split(models, ",") {
		if strings.TrimSpace(modelName) != "" {
			return true
		}
	}
	return false
}

func readCodexAuthFilesImportParts(c *gin.Context) (string, []*codex.ParsedAuthFile, error) {
	reader, err := c.Request.MultipartReader()
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse upload form: %w", err)
	}

	templateRaw := ""
	parsedFiles := make([]*codex.ParsedAuthFile, 0)

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("failed to read upload form: %w", err)
		}

		formName := part.FormName()
		fileName := part.FileName()

		switch {
		case formName == "channel" && fileName == "":
			if templateRaw != "" {
				_ = part.Close()
				continue
			}
			data, err := io.ReadAll(part)
			closeErr := part.Close()
			if err != nil {
				return "", nil, fmt.Errorf("failed to read channel template: %w", err)
			}
			if closeErr != nil {
				return "", nil, fmt.Errorf("failed to close channel template part: %w", closeErr)
			}
			templateRaw = string(data)

		case (formName == "files" || formName == "file") && fileName != "":
			data, err := io.ReadAll(part)
			closeErr := part.Close()
			if err != nil {
				return "", nil, fmt.Errorf("%s: failed to read auth file: %w", fileName, err)
			}
			if closeErr != nil {
				return "", nil, fmt.Errorf("%s: failed to close auth file part: %w", fileName, closeErr)
			}
			parsed, err := parseCodexAuthFileBytes(data, fileName)
			if err != nil {
				return "", nil, fmt.Errorf("%s: %w", fileName, err)
			}
			parsedFiles = append(parsedFiles, parsed)

		default:
			_ = part.Close()
		}
	}

	if strings.TrimSpace(templateRaw) == "" {
		return "", nil, fmt.Errorf("channel template is required")
	}
	if len(parsedFiles) == 0 {
		return "", nil, fmt.Errorf("at least one auth file is required")
	}

	return templateRaw, parsedFiles, nil
}

func parseUploadedCodexAuthFile(fileHeader *multipart.FileHeader) (*codex.ParsedAuthFile, error) {
	if fileHeader == nil {
		return nil, fmt.Errorf("file is required")
	}

	file, err := fileHeader.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open auth file: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read auth file: %w", err)
	}

	return parseCodexAuthFileBytes(data, fileHeader.Filename)
}

func parseCodexAuthFileBytes(data []byte, filename string) (*codex.ParsedAuthFile, error) {
	return codex.ParseAuthFile(data, filename)
}

func buildImportedCodexChannelName(baseName string, parsed *codex.ParsedAuthFile, index int, total int) string {
	baseName = strings.TrimSpace(baseName)
	label := ""
	fileName := ""
	if parsed != nil {
		label = strings.TrimSpace(parsed.DisplayLabel())
		fileName = strings.TrimSpace(parsed.FileName)
	}

	if baseName == "" {
		switch {
		case label != "":
			return label
		case total > 1:
			return "Codex_" + strconv.Itoa(index+1)
		default:
			return "Codex"
		}
	}

	if total > 1 {
		if label == "" {
			label = strings.TrimSpace(strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName)))
		}
		if label != "" {
			return baseName + "_" + label
		}
		return baseName + "_" + strconv.Itoa(index+1)
	}

	return baseName
}
