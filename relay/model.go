package relay

import (
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/common/utils"
	"one-api/model"
	"one-api/providers/claude"
	"one-api/providers/gemini"
	"one-api/types"
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/gin-gonic/gin"
)

// https://platform.openai.com/docs/api-reference/models/list
type OpenAIModels struct {
	Id      string  `json:"id"`
	Object  string  `json:"object"`
	Created int     `json:"created"`
	OwnedBy *string `json:"owned_by"`
}

func modelsGroupName(c *gin.Context) string {
	// Model-list handlers should expose the request's effective routing scope.
	// Backup groups remain a fallback path and are not advertised unless they are
	// already the current routing truth for this request.
	return groupctx.CurrentRoutingGroup(c)
}

func ListModelsByToken(c *gin.Context) {
	groupName := modelsGroupName(c)

	if groupName == "" {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, "分组不存在")
		return
	}

	models, err := model.ChannelGroup.GetGroupModels(groupName)
	if err != nil {
		c.JSON(200, gin.H{
			"object": "list",
			"data":   []string{},
		})
		return
	}
	sort.Strings(models)

	catalog, err := loadModelDisplayCatalog(models)
	if err != nil {
		abortModelCatalogRead(c, err)
		return
	}
	groupOpenAIModels := make([]*OpenAIModels, 0, len(models))
	for _, modelName := range models {
		groupOpenAIModels = append(groupOpenAIModels, catalog.openAIModel(modelName))
	}

	sort.Slice(groupOpenAIModels, func(i, j int) bool {
		if *groupOpenAIModels[i].OwnedBy == *groupOpenAIModels[j].OwnedBy {
			return groupOpenAIModels[i].Id < groupOpenAIModels[j].Id
		}
		return *groupOpenAIModels[i].OwnedBy < *groupOpenAIModels[j].OwnedBy
	})

	c.JSON(200, gin.H{
		"object": "list",
		"data":   groupOpenAIModels,
	})
}

// https://generativelanguage.googleapis.com/v1beta/models?key=xxxxxxx
func ListGeminiModelsByToken(c *gin.Context) {
	groupName := modelsGroupName(c)

	if groupName == "" {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, "分组不存在")
		return
	}

	models, err := model.ChannelGroup.GetGroupModels(groupName)
	if err != nil {
		c.JSON(200, gemini.ModelListResponse{
			Models: []gemini.ModelDetails{},
		})
		return
	}
	sort.Strings(models)

	var geminiModels []gemini.ModelDetails
	for _, modelName := range models {
		if model.ChannelGroup.ModelHasChannel(groupName, modelName, model.FilterChannelTypes(AllowGeminiChannelType)) {
			geminiModels = append(geminiModels, gemini.ModelDetails{
				Name:        fmt.Sprintf("models/%s", modelName),
				DisplayName: cases.Title(language.Und).String(strings.ReplaceAll(modelName, "-", " ")),
				SupportedGenerationMethods: []string{
					"generateContent",
				},
			})
		}
	}

	c.JSON(200, gemini.ModelListResponse{
		Models: geminiModels,
	})
}

func ListClaudeModelsByToken(c *gin.Context) {
	groupName := modelsGroupName(c)

	if groupName == "" {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, "分组不存在")
		return
	}

	models, err := model.ChannelGroup.GetGroupModels(groupName)
	if err != nil {
		c.JSON(200, claude.ModelListResponse{
			Data: []claude.Model{},
		})
		return
	}
	sort.Strings(models)

	var claudeModelsData []claude.Model
	for _, modelName := range models {
		if isClaudeModelForGroup(groupName, modelName) {
			claudeModelsData = append(claudeModelsData, claude.Model{
				ID:   modelName,
				Type: "model",
			})
		}
	}

	c.JSON(200, claude.ModelListResponse{
		Data: claudeModelsData,
	})
}

func isClaudeModelForGroup(groupName string, modelName string) bool {
	return model.ChannelGroup.ModelHasChannel(groupName, modelName, model.FilterFunc(filterNonClaudeRouteEligibleChannel))
}

func ListModelsForAdmin(c *gin.Context) {
	prices := model.PricingInstance.GetAllPrices()
	modelNames := make([]string, 0, len(prices))
	for name := range prices {
		modelNames = append(modelNames, name)
	}
	catalog, err := loadModelDisplayCatalog(modelNames)
	if err != nil {
		abortModelCatalogRead(c, err)
		return
	}
	openAIModels := make([]*OpenAIModels, 0, len(modelNames))
	for _, name := range modelNames {
		openAIModels = append(openAIModels, catalog.openAIModel(name))
	}
	sort.Slice(openAIModels, func(i, j int) bool {
		if *openAIModels[i].OwnedBy == *openAIModels[j].OwnedBy {
			return openAIModels[i].Id < openAIModels[j].Id
		}
		return *openAIModels[i].OwnedBy < *openAIModels[j].OwnedBy
	})

	c.JSON(http.StatusOK, gin.H{"object": "list", "data": openAIModels})
}

func RetrieveModel(c *gin.Context) {
	modelName := c.Param("model")
	if !model.ChannelGroup.ModelHasCandidate(modelsGroupName(c), modelName) {
		c.JSON(http.StatusOK, gin.H{"error": types.OpenAIError{
			Message: fmt.Sprintf("The model '%s' does not exist", modelName),
			Type:    "invalid_request_error", Param: "model", Code: "model_not_found",
		}})
		return
	}
	catalog, err := loadModelDisplayCatalog([]string{modelName})
	if err != nil {
		abortModelCatalogRead(c, err)
		return
	}
	c.JSON(http.StatusOK, catalog.openAIModel(modelName))
}

type modelDisplayCatalog struct {
	info   map[string]*model.ModelInfoResponse
	owners map[int]*model.ModelOwnedBy
}

func loadModelDisplayCatalog(names []string) (*modelDisplayCatalog, error) {
	catalog := &modelDisplayCatalog{}
	if len(names) == 0 {
		return catalog, nil
	}
	var err error
	catalog.info, err = model.GetModelInfoResponses(names)
	if err != nil {
		return nil, err
	}
	catalog.owners, err = model.GetModelOwnedByMap()
	if err != nil {
		return nil, err
	}
	return catalog, nil
}

func (catalog *modelDisplayCatalog) ownedBy(name string) string {
	if info := catalog.info[name]; info != nil && info.OwnedByID != nil {
		if owner := catalog.owners[*info.OwnedByID]; owner != nil && owner.Name != "" {
			return owner.Name
		}
	}
	return model.UnknownOwnedBy
}

func (catalog *modelDisplayCatalog) openAIModel(name string) *OpenAIModels {
	owner := catalog.ownedBy(name)
	return &OpenAIModels{Id: name, Object: "model", Created: 1677649963, OwnedBy: &owner}
}

func abortModelCatalogRead(c *gin.Context, err error) {
	logger.LogError(c.Request.Context(), "读取模型目录失败: "+err.Error())
	common.AbortWithMessage(c, http.StatusInternalServerError, "读取模型目录失败")
}

func GetModelOwnedBy(c *gin.Context) {
	owners, err := model.GetModelOwnedByMap()
	if err != nil {
		common.APIRespondWithError(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    owners,
	})
}

type ModelPrice struct {
	Type   string  `json:"type"`
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

type AvailableModelResponse struct {
	Groups  []string     `json:"groups"`
	OwnedBy string       `json:"owned_by"`
	Price   *model.Price `json:"price"`
}

func AvailableModel(c *gin.Context) {
	groupName := c.GetString("group")

	models, err := GetAvailableModels(groupName)
	if err != nil {
		logger.LogError(c.Request.Context(), "读取模型目录失败: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "读取模型目录失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": models})
}

func GetAvailableModels(groupName string) (map[string]*AvailableModelResponse, error) {
	publicModels := model.ChannelGroup.GetModelsGroups()
	publicGroups := append([]string(nil), model.GlobalUserGroupRatio.GetPublicGroupList()...)
	if groupName != "" && !utils.Contains(groupName, publicGroups) {
		publicGroups = append(publicGroups, groupName)
	}

	availableModels := make(map[string]*AvailableModelResponse, len(publicModels))

	for modelName, group := range publicModels {
		groups := []string{}
		for _, publicGroup := range publicGroups {
			if group[publicGroup] {
				groups = append(groups, publicGroup)
			}
		}

		if len(groups) == 0 {
			continue
		}

		if _, ok := availableModels[modelName]; !ok {
			price, priced := model.PricingInstance.FindPrice(modelName)
			if !priced {
				continue
			}
			// FindPrice 返回策略副本；只在查询边界附加精确模型的展示信息。
			availableModels[modelName] = &AvailableModelResponse{Groups: groups, Price: price}
		}
	}

	modelNames := make([]string, 0, len(availableModels))
	for name := range availableModels {
		modelNames = append(modelNames, name)
	}
	catalog, err := loadModelDisplayCatalog(modelNames)
	if err != nil {
		return nil, err
	}
	for name, item := range availableModels {
		item.OwnedBy = catalog.ownedBy(name)
		item.Price.ModelInfo = catalog.info[name]
	}
	return availableModels, nil
}
