package model

import (
	"errors"
	"one-api/common/config"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var UnknownOwnedBy = "未知"

const ModelOwnedByReserveID = 1000

type ModelOwnedBy struct {
	Id   int    `json:"id" gorm:"index"`
	Name string `json:"name" gorm:"type:varchar(100)"`
	Icon string `json:"icon" gorm:"type:text"`
}

func (m *ModelOwnedBy) TableName() string {
	return "model_owned_by"
}

func CreateModelOwnedBy(modelOwnedBy *ModelOwnedBy) error {
	if modelOwnedBy == nil || modelOwnedBy.Id <= ModelOwnedByReserveID {
		return errors.New("自定义归属 ID 必须大于 1000")
	}
	return DB.Create(modelOwnedBy).Error
}

func UpdateModelOwnedBy(modelOwnedBy *ModelOwnedBy) error {
	if modelOwnedBy == nil || modelOwnedBy.Id <= 0 {
		return gorm.ErrRecordNotFound
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := lockModelOwnedBy(tx, modelOwnedBy.Id); err != nil {
			return err
		}
		return tx.Model(&ModelOwnedBy{}).Where("id = ?", modelOwnedBy.Id).
			Select("name", "icon").Updates(modelOwnedBy).Error
	})
}

func GetModelOwnedBy(id int) (*ModelOwnedBy, error) {
	modelOwnedBy := &ModelOwnedBy{}
	err := DB.Where("id = ?", id).First(modelOwnedBy).Error
	if err != nil {
		return nil, err
	}
	return modelOwnedBy, nil
}

func GetAllModelOwnedBy() ([]*ModelOwnedBy, error) {
	var modelOwnedBies []*ModelOwnedBy
	err := DB.Find(&modelOwnedBies).Error
	if err != nil {
		return nil, err
	}
	return modelOwnedBies, nil
}

var ErrModelOwnedByInUse = errors.New("模型归属仍被模型引用，无法删除")

// 目录关联写入与分类删除锁定同一分类行，防止校验之后被并发删除。
func lockModelOwnedBy(tx *gorm.DB, id int) error {
	if tx.Dialector.Name() == "sqlite" {
		// SQLite 不支持行锁；先取得写锁，再读取分类及其引用。
		if err := tx.Model(&ModelOwnedBy{}).Where("id = ?", id).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
	}
	var ownedBy ModelOwnedBy
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ownedBy, "id = ?", id).Error
}

func DeleteModelOwnedBy(id int) error {
	if id <= ModelOwnedByReserveID {
		return errors.New("不能删除内置归属")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := lockModelOwnedBy(tx, id); err != nil {
			return err
		}
		var references int64
		if err := tx.Model(&ModelInfo{}).Where("owned_by_id = ?", id).Count(&references).Error; err != nil {
			return err
		}
		if references != 0 {
			return ErrModelOwnedByInUse
		}
		return tx.Delete(&ModelOwnedBy{}, id).Error
	})
}

func GetModelOwnedByMap() (map[int]*ModelOwnedBy, error) {
	rows, err := GetAllModelOwnedBy()
	if err != nil {
		return nil, err
	}
	result := make(map[int]*ModelOwnedBy, len(rows))
	for _, row := range rows {
		result[row.Id] = row
	}
	return result, nil
}

// InitModelOwnedBys 只补齐缺失的内置分类，不覆盖管理员编辑的名称和图标。
func InitModelOwnedBys() error {
	return DB.Clauses(clause.OnConflict{DoNothing: true}).Create(GetDefaultModelOwnedBy()).Error
}

func GetDefaultModelOwnedBy() []*ModelOwnedBy {
	return []*ModelOwnedBy{
		{Id: config.ChannelTypeOpenAI, Name: "OpenAI", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/openai.svg"},
		{Id: config.ChannelTypeCodex, Name: "Codex", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/openai.svg"},
		// {Id: config.ChannelTypePaLM, Name: "Google PaLM", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/gemini-color.svg"},
		{Id: config.ChannelTypeAnthropic, Name: "Anthropic", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/claude-color.svg"},
		{Id: config.ChannelTypeBaidu, Name: "Baidu", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/wenxin-color.svg"},
		{Id: config.ChannelTypeZhipu, Name: "Zhipu", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/zhipu-color.svg"},
		{Id: config.ChannelTypeAli, Name: "Qwen", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/qwen-color.svg"},
		{Id: config.ChannelTypeXunfei, Name: "Spark", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/spark-color.svg"},
		{Id: config.ChannelType360, Name: "360", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/ai360-color.svg"},
		{Id: config.ChannelTypeTencent, Name: "Tencent", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/hunyuan-color.svg"},
		{Id: config.ChannelTypeGemini, Name: "Google Gemini", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/gemini-color.svg"},
		{Id: config.ChannelTypeBaichuan, Name: "Baichuan", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/baichuan-color.svg"},
		{Id: config.ChannelTypeMiniMax, Name: "MiniMax", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/minimax-color.svg"},
		{Id: config.ChannelTypeDeepseek, Name: "Deepseek", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/deepseek-color.svg"},
		{Id: config.ChannelTypeMoonshot, Name: "Moonshot", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/moonshot.svg"},
		{Id: config.ChannelTypeMistral, Name: "Mistral", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/mistral-color.svg"},
		{Id: config.ChannelTypeGroq, Name: "Groq", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/groq.svg"},
		{Id: config.ChannelTypeLingyi, Name: "Yi", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/yi-color.svg"},
		{Id: config.ChannelTypeMidjourney, Name: "Midjourney", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/midjourney.svg"},
		{Id: config.ChannelTypeCloudflareAI, Name: "Cloudflare AI", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/cloudflare-color.svg"},
		{Id: config.ChannelTypeCohere, Name: "Cohere", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/cohere-color.svg"},
		{Id: config.ChannelTypeStabilityAI, Name: "Stability AI", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/stability-color.svg"},
		{Id: config.ChannelTypeCoze, Name: "Coze", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/coze.svg"},
		{Id: config.ChannelTypeOllama, Name: "Ollama", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/ollama.svg"},
		{Id: config.ChannelTypeHunyuan, Name: "Hunyuan", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/hunyuan-color.svg"},
		{Id: config.ChannelTypeSuno, Name: "Suno", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/suno.svg"},
		{Id: config.ChannelTypeLLAMA, Name: "Meta", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/meta-color.svg"},
		{Id: config.ChannelTypeIdeogram, Name: "Ideogram", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/ideogram.svg"},
		{Id: config.ChannelTypeSiliconflow, Name: "Siliconflow", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/siliconcloud-color.svg"},
		{Id: config.ChannelTypeFlux, Name: "Flux", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/flux.svg"},
		{Id: config.ChannelTypeJina, Name: "Jina", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/jina.svg"},
		{Id: config.ChannelTypeRerank, Name: "Rerank", Icon: ""},
		{Id: config.ChannelTypeRecraft, Name: "RecraftAI", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/recraft.svg"},
		{Id: config.ChannelTypeKling, Name: "Kling", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/kling-color.svg"},
		{Id: config.ChannelTypeOpenRouter, Name: "OpenRouter", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-svg/latest/files/icons/openrouter.svg"},
		{Id: config.ChannelTypeXAI, Name: "xAI", Icon: "https://registry.npmmirror.com/@lobehub/icons-static-webp/1.24.0/files/light/xai.webp"},
	}
}
