package model

import (
	"errors"
	"one-api/common/utils"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ModelInfo struct {
	Id               int    `json:"id" gorm:"index"`
	Model            string `json:"model" gorm:"type:varchar(100);uniqueIndex"`
	OwnedByID        *int   `json:"owned_by_id" gorm:"index"`
	Name             string `json:"name" gorm:"type:varchar(100)"`
	Description      string `json:"description" gorm:"type:text"`
	ContextLength    int    `json:"context_length"`
	MaxTokens        int    `json:"max_tokens"`
	InputModalities  string `json:"input_modalities" gorm:"type:text"`
	OutputModalities string `json:"output_modalities" gorm:"type:text"`
	Tags             string `json:"tags" gorm:"type:text"`
	SupportUrl       string `json:"support_url" gorm:"type:text"`
	CreatedAt        int64  `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt        int64  `json:"updated_at" gorm:"autoUpdateTime"`
}

type ModelInfoResponse struct {
	Model            string   `json:"model"`
	OwnedByID        *int     `json:"owned_by_id"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	ContextLength    int      `json:"context_length"`
	MaxTokens        int      `json:"max_tokens"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	Tags             []string `json:"tags"`
	SupportUrl       []string `json:"support_url"`
	CreatedAt        int64    `json:"created_at"`
	UpdatedAt        int64    `json:"updated_at"`
}

func (m *ModelInfo) ToResponse() *ModelInfoResponse {
	res := &ModelInfoResponse{
		Model:         m.Model,
		OwnedByID:     m.OwnedByID,
		Name:          m.Name,
		Description:   m.Description,
		ContextLength: m.ContextLength,
		MaxTokens:     m.MaxTokens,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}

	res.InputModalities, _ = utils.UnmarshalString[[]string](m.InputModalities)
	res.OutputModalities, _ = utils.UnmarshalString[[]string](m.OutputModalities)
	res.Tags, _ = utils.UnmarshalString[[]string](m.Tags)

	var err error
	res.SupportUrl, err = utils.UnmarshalString[[]string](m.SupportUrl)
	if err != nil {
		if m.SupportUrl != "" {
			res.SupportUrl = []string{m.SupportUrl}
		} else {
			res.SupportUrl = []string{}
		}
	}

	return res
}

func (m *ModelInfo) TableName() string {
	return "model_info"
}

func validateModelInfo(modelInfo *ModelInfo) error {
	if modelInfo == nil || strings.TrimSpace(modelInfo.Model) == "" || utf8.RuneCountInString(modelInfo.Model) > 100 {
		return errors.New("模型标识不能为空且不能超过 100 个字符")
	}
	return nil
}

func CreateModelInfo(modelInfo *ModelInfo) error {
	if err := validateModelInfo(modelInfo); err != nil {
		return err
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if modelInfo.OwnedByID != nil {
			if err := lockModelOwnedBy(tx, *modelInfo.OwnedByID); err != nil {
				return err
			}
		}
		return tx.Create(modelInfo).Error
	})
}

func UpdateModelInfo(modelInfo *ModelInfo) error {
	if err := validateModelInfo(modelInfo); err != nil {
		return err
	}
	if modelInfo.Id <= 0 {
		return gorm.ErrRecordNotFound
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if modelInfo.OwnedByID != nil {
			if err := lockModelOwnedBy(tx, *modelInfo.OwnedByID); err != nil {
				return err
			}
		}
		var current ModelInfo
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, modelInfo.Id).Error; err != nil {
			return err
		}
		return tx.Model(&ModelInfo{}).Where("id = ?", modelInfo.Id).
			Select("*").Omit("id", "created_at").Updates(modelInfo).Error
	})
}

// GetModelInfoResponses 按对外精确模型名装配目录；缺失与查询失败分别返回。
func GetModelInfoResponses(modelNames []string) (map[string]*ModelInfoResponse, error) {
	responses := make(map[string]*ModelInfoResponse, len(modelNames))
	if len(modelNames) == 0 {
		return responses, nil
	}
	// 分批限制绑定参数数量，兼容 SQLite 的参数上限。
	const batchSize = 500
	for start := 0; start < len(modelNames); start += batchSize {
		end := start + batchSize
		if end > len(modelNames) {
			end = len(modelNames)
		}
		var rows []*ModelInfo
		if err := DB.Where("model IN ?", modelNames[start:end]).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			responses[row.Model] = row.ToResponse()
		}
	}
	return responses, nil
}

func GetModelInfo(id int) (*ModelInfo, error) {
	modelInfo := &ModelInfo{}
	err := DB.Where("id = ?", id).First(modelInfo).Error
	if err != nil {
		return nil, err
	}
	return modelInfo, nil
}

func GetModelInfoByModel(model string) (*ModelInfo, error) {
	modelInfo := &ModelInfo{}
	err := DB.Where("model = ?", model).First(modelInfo).Error
	if err != nil {
		return nil, err
	}
	return modelInfo, nil
}

func GetAllModelInfo() ([]*ModelInfo, error) {
	var modelInfos []*ModelInfo
	err := DB.Order("id desc").Find(&modelInfos).Error
	if err != nil {
		return nil, err
	}
	return modelInfos, nil
}

func DeleteModelInfo(id int) error {
	err := DB.Delete(&ModelInfo{}, id).Error
	if err != nil {
		return err
	}
	return nil
}
