package providers

import (
	"encoding/json"
	"fmt"
	"one-api/common/config"
	"one-api/model"
	"one-api/providers/ali"
	"one-api/providers/azure"
	azurespeech "one-api/providers/azureSpeech"
	"one-api/providers/azure_v1"
	"one-api/providers/azuredatabricks"
	"one-api/providers/baichuan"
	"one-api/providers/baidu"
	"one-api/providers/base"
	"one-api/providers/bedrock"
	"one-api/providers/claude"
	"one-api/providers/cloudflareAI"
	"one-api/providers/codex"
	"one-api/providers/cohere"
	"one-api/providers/coze"
	"one-api/providers/deepseek"
	"one-api/providers/gemini"
	"one-api/providers/github"
	"one-api/providers/groq"
	"one-api/providers/hunyuan"
	"one-api/providers/jina"
	"one-api/providers/kling"
	"one-api/providers/lingyi"
	"one-api/providers/midjourney"
	"one-api/providers/minimax"
	"one-api/providers/mistral"
	"one-api/providers/moonshot"
	"one-api/providers/ollama"
	"one-api/providers/openai"
	"one-api/providers/openrouter"
	"one-api/providers/recraftAI"
	"one-api/providers/replicate"
	"one-api/providers/siliconflow"
	"one-api/providers/stabilityAI"
	"one-api/providers/suno"
	"one-api/providers/tencent"
	"one-api/providers/vertexai"
	"one-api/providers/xAI"
	"one-api/providers/xunfei"
	"one-api/providers/zhipu"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

// 定义供应商工厂接口
type ProviderFactory interface {
	Create(Channel *model.Channel) base.ProviderInterface
}

// chatRemoteMediaFactoryPolicy is deliberately implemented by provider-local
// factories rather than provider instances. Candidate assessment therefore
// cannot construct requesters, parse credentials, log, or acquire other I/O
// capabilities. An unimplemented policy is an explicit Reject.
type chatRemoteMediaFactoryPolicy interface {
	AssessChatRemoteMedia(channel *model.Channel, request *types.ChatCompletionRequest, summary base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error)
}

type nativeClaudeRemoteMediaFactoryPolicy interface {
	AssessNativeClaudeRemoteMedia(channel *model.Channel, request *claude.ClaudeRequest, summary claude.NativeRemoteMediaSummary) (base.RemoteMediaMode, error)
}

type nativeClaudeRemoteMediaMaterializer interface {
	MaterializeNativeClaudeRemoteMedia(request *claude.ClaudeRequest, fetcher base.RemoteMediaFetcher) (*claude.ClaudeRequest, error)
}

type openAICompatibleProviderFactory struct{}

func (openAICompatibleProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	return openai.CreateOpenAIProvider(channel, channel.GetBaseURL())
}

func (openAICompatibleProviderFactory) AssessChatRemoteMedia(_ *model.Channel, _ *types.ChatCompletionRequest, _ base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error) {
	return base.RemoteMediaPassURL, nil
}

func (openAICompatibleProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return openai.AssessChatRequestForChannel(channel, canonicalModel, request, channel.GetBaseURL())
}

func (openAICompatibleProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	return openai.ValidateChatRequestForChannel(channel, canonicalModel, request, fields)
}

func (openAICompatibleProviderFactory) AssessSpeechRequest(channel *model.Channel, request *types.SpeechAudioRequest) error {
	return openai.AssessSpeechRequestForChannel(channel, request, channel.GetBaseURL())
}

func (openAICompatibleProviderFactory) AssessTranscriptionRequest(channel *model.Channel, request *types.AudioRequest) error {
	return openai.AssessTranscriptionRequestForChannel(channel, request, channel.GetBaseURL())
}

func (openAICompatibleProviderFactory) ResponsesSupport(channel *model.Channel) base.OperationSupport {
	return openai.ResponsesSupportForChannel(channel, channel.GetBaseURL())
}

func (openAICompatibleProviderFactory) ValidateResponsesRequest(channel *model.Channel, operation base.Operation, fields map[string]json.RawMessage, modelName string, applyPreAdd bool) error {
	return openai.ValidateResponsesRequestForChannel(channel, operation, fields, modelName, applyPreAdd)
}

// 创建全局的供应商工厂映射
var providerFactories = make(map[int]ProviderFactory)

// 在程序启动时，添加所有的供应商工厂
func init() {
	providerFactories = map[int]ProviderFactory{
		config.ChannelTypeOpenAI:    openai.OpenAIProviderFactory{},
		config.ChannelTypeAzure:     azure.AzureProviderFactory{},
		config.ChannelTypeAli:       ali.AliProviderFactory{},
		config.ChannelTypeTencent:   tencent.TencentProviderFactory{},
		config.ChannelTypeBaidu:     baidu.BaiduProviderFactory{},
		config.ChannelTypeAnthropic: claude.ClaudeProviderFactory{},
		// config.ChannelTypePaLM:            palm.PalmProviderFactory{},
		config.ChannelTypeZhipu:           zhipu.ZhipuProviderFactory{},
		config.ChannelTypeXunfei:          xunfei.XunfeiProviderFactory{},
		config.ChannelTypeAzureSpeech:     azurespeech.AzureSpeechProviderFactory{},
		config.ChannelTypeGemini:          gemini.GeminiProviderFactory{},
		config.ChannelTypeBaichuan:        baichuan.BaichuanProviderFactory{},
		config.ChannelTypeMiniMax:         minimax.MiniMaxProviderFactory{},
		config.ChannelTypeDeepseek:        deepseek.DeepseekProviderFactory{},
		config.ChannelTypeMistral:         mistral.MistralProviderFactory{},
		config.ChannelTypeGroq:            groq.GroqProviderFactory{},
		config.ChannelTypeBedrock:         bedrock.BedrockProviderFactory{},
		config.ChannelTypeMidjourney:      midjourney.MidjourneyProviderFactory{},
		config.ChannelTypeCloudflareAI:    cloudflareAI.CloudflareAIProviderFactory{},
		config.ChannelTypeCodex:           codex.CodexProviderFactory{},
		config.ChannelTypeCohere:          cohere.CohereProviderFactory{},
		config.ChannelTypeStabilityAI:     stabilityAI.StabilityAIProviderFactory{},
		config.ChannelTypeCoze:            coze.CozeProviderFactory{},
		config.ChannelTypeOllama:          ollama.OllamaProviderFactory{},
		config.ChannelTypeMoonshot:        moonshot.MoonshotProviderFactory{},
		config.ChannelTypeLingyi:          lingyi.LingyiProviderFactory{},
		config.ChannelTypeHunyuan:         hunyuan.HunyuanProviderFactory{},
		config.ChannelTypeSuno:            suno.SunoProviderFactory{},
		config.ChannelTypeVertexAI:        vertexai.VertexAIProviderFactory{},
		config.ChannelTypeSiliconflow:     siliconflow.SiliconflowProviderFactory{},
		config.ChannelTypeJina:            jina.JinaProviderFactory{},
		config.ChannelTypeKling:           kling.KlingProviderFactory{},
		config.ChannelTypeGithub:          github.GithubProviderFactory{},
		config.ChannelTypeRecraft:         recraftAI.RecraftProviderFactory{},
		config.ChannelTypeReplicate:       replicate.ReplicateProviderFactory{},
		config.ChannelTypeOpenRouter:      openrouter.OpenRouterProviderFactory{},
		config.ChannelTypeAzureDatabricks: azuredatabricks.AzureDatabricksProviderFactory{},
		config.ChannelTypeAzureV1:         azure_v1.AzureV1ProviderFactory{},
		config.ChannelTypeXAI:             xAI.XAIProviderFactory{},
	}
}

// 获取供应商
func GetProvider(channel *model.Channel, c *gin.Context) base.ProviderInterface {
	provider := createProvider(channel)
	if provider == nil {
		return nil
	}
	provider.SetContext(c)

	return provider
}

func createProvider(channel *model.Channel) base.ProviderInterface {
	factory := providerFactoryForChannel(channel)
	if factory == nil {
		return nil
	}
	return factory.Create(channel)
}

func providerFactoryForChannel(channel *model.Channel) ProviderFactory {
	if channel == nil {
		return nil
	}
	if factory, ok := providerFactories[channel.Type]; ok {
		return factory
	}
	if channel.GetBaseURL() == "" {
		return nil
	}
	return openAICompatibleProviderFactory{}
}

func nativeClaudeProviderFactoryForChannel(channel *model.Channel) ProviderFactory {
	if channel == nil {
		return nil
	}
	if channel.Type == config.ChannelTypeCustom {
		if !channel.CustomClaudeRelayEnabled() {
			return nil
		}
		return claude.ClaudeProviderFactory{}
	}
	return providerFactoryForChannel(channel)
}

// AssessChatRemoteMedia asks the concrete provider adapter how it represents
// Chat media. It is safe for candidate evaluation: factories and policies must
// not perform network I/O, and no media means no provider construction at all.
func AssessChatRemoteMedia(channel *model.Channel, request *types.ChatCompletionRequest) (base.RemoteMediaMode, base.ChatRemoteMediaSummary, error) {
	summary, err := base.SummarizeChatRemoteMedia(request)
	if err != nil {
		return base.RemoteMediaReject, summary, remoteMediaCapabilityError(err)
	}
	if !summary.HasMedia() {
		return base.RemoteMediaReject, summary, nil
	}
	factory := providerFactoryForChannel(channel)
	if factory == nil {
		return base.RemoteMediaReject, summary, remoteMediaCapabilityError(fmt.Errorf("selected provider is unavailable"))
	}
	return assessFactoryChatRemoteMedia(factory, channel, request, summary)
}

// PrepareChatRemoteMedia validates or materializes media only after a provider
// has been selected. PassURL adapters never receive a fetcher call.
func PrepareChatRemoteMedia(provider base.ProviderInterface, request *types.ChatCompletionRequest, fetcher base.RemoteMediaFetcher) error {
	if provider != nil && provider.GetChannel() != nil {
		if normalizer, ok := provider.(base.ChatRemoteMediaHistoryNormalizer); ok {
			if err := normalizer.NormalizeChatRemoteMedia(request); err != nil {
				return remoteMediaCapabilityError(err)
			}
		}
	}
	summary, err := base.SummarizeChatRemoteMedia(request)
	if err != nil {
		return remoteMediaCapabilityError(err)
	}
	if !summary.HasMedia() {
		return nil
	}
	if provider == nil || provider.GetChannel() == nil {
		return remoteMediaCapabilityError(fmt.Errorf("selected provider is unavailable"))
	}
	factory := providerFactoryForChannel(provider.GetChannel())
	mode, _, err := assessFactoryChatRemoteMedia(factory, provider.GetChannel(), request, summary)
	if err != nil {
		return err
	}
	switch mode {
	case base.RemoteMediaPassURL:
		if err := base.ValidateChatRemoteMedia(request); err != nil {
			return remoteMediaCapabilityError(err)
		}
		return nil
	case base.RemoteMediaMaterialize:
		materializer, ok := provider.(base.ChatRemoteMediaMaterializer)
		if !ok {
			return remoteMediaCapabilityError(fmt.Errorf("selected provider declares media materialization without implementing it"))
		}
		if err := materializer.MaterializeChatRemoteMedia(request, fetcher); err != nil {
			return remoteMediaCapabilityError(err)
		}
		return nil
	default:
		return remoteMediaCapabilityError(fmt.Errorf("selected provider does not support chat media"))
	}
}

func assessFactoryChatRemoteMedia(factory ProviderFactory, channel *model.Channel, request *types.ChatCompletionRequest, summary base.ChatRemoteMediaSummary) (base.RemoteMediaMode, base.ChatRemoteMediaSummary, error) {
	policy, ok := factory.(chatRemoteMediaFactoryPolicy)
	if !ok {
		return base.RemoteMediaReject, summary, remoteMediaCapabilityError(fmt.Errorf("selected provider does not declare chat media support"))
	}
	mode, err := policy.AssessChatRemoteMedia(channel, request, summary)
	if err != nil {
		return base.RemoteMediaReject, summary, remoteMediaCapabilityError(err)
	}
	switch mode {
	case base.RemoteMediaPassURL, base.RemoteMediaMaterialize:
		return mode, summary, nil
	case base.RemoteMediaReject:
		return mode, summary, remoteMediaCapabilityError(fmt.Errorf("selected provider does not support chat media"))
	default:
		return base.RemoteMediaReject, summary, remoteMediaCapabilityError(fmt.Errorf("selected provider returned an invalid chat media mode"))
	}
}

func remoteMediaCapabilityError(err error) error {
	if err == nil {
		return nil
	}
	if capabilityErr, ok := err.(*base.RequestCapabilityError); ok {
		return capabilityErr
	}
	return &base.RequestCapabilityError{Param: "messages", Message: err.Error()}
}

// AssessNativeClaudeRemoteMedia is candidate-safe: it only calls a pure
// provider-factory policy and never constructs a provider or requester.
func AssessNativeClaudeRemoteMedia(channel *model.Channel, request *claude.ClaudeRequest) (base.RemoteMediaMode, claude.NativeRemoteMediaSummary, error) {
	summary := claude.SummarizeNativeRemoteMedia(request)
	if !summary.HasURLSources() {
		return base.RemoteMediaReject, summary, nil
	}
	factory := nativeClaudeProviderFactoryForChannel(channel)
	policy, ok := factory.(nativeClaudeRemoteMediaFactoryPolicy)
	if !ok {
		return base.RemoteMediaReject, summary, remoteMediaCapabilityError(fmt.Errorf("selected provider does not declare native Claude media support"))
	}
	mode, err := policy.AssessNativeClaudeRemoteMedia(channel, request, summary)
	if err != nil {
		return base.RemoteMediaReject, summary, remoteMediaCapabilityError(err)
	}
	if mode == base.RemoteMediaReject {
		return mode, summary, remoteMediaCapabilityError(fmt.Errorf("selected provider does not support native Claude URL media"))
	}
	return mode, summary, nil
}

// PrepareNativeClaudeRemoteMedia returns the exact typed request to use for the
// selected attempt. PassURL keeps the input projection; Materialize returns a
// detached deep copy so the direct Claude raw body remains untouched.
func PrepareNativeClaudeRemoteMedia(provider base.ProviderInterface, request *claude.ClaudeRequest, fetcher base.RemoteMediaFetcher) (*claude.ClaudeRequest, error) {
	summary := claude.SummarizeNativeRemoteMedia(request)
	if !summary.HasURLSources() {
		return request, nil
	}
	if provider == nil || provider.GetChannel() == nil {
		return nil, remoteMediaCapabilityError(fmt.Errorf("selected provider is unavailable"))
	}
	mode, _, err := AssessNativeClaudeRemoteMedia(provider.GetChannel(), request)
	if err != nil {
		return nil, err
	}
	switch mode {
	case base.RemoteMediaPassURL:
		return request, nil
	case base.RemoteMediaMaterialize:
		materializer, ok := provider.(nativeClaudeRemoteMediaMaterializer)
		if !ok {
			return nil, remoteMediaCapabilityError(fmt.Errorf("selected provider declares native Claude media materialization without implementing it"))
		}
		prepared, err := materializer.MaterializeNativeClaudeRemoteMedia(request, fetcher)
		if err != nil {
			return nil, remoteMediaCapabilityError(err)
		}
		return prepared, nil
	default:
		return nil, remoteMediaCapabilityError(fmt.Errorf("selected provider does not support native Claude URL media"))
	}
}
