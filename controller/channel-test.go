package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/notify"
	"one-api/common/utils"
	"one-api/model"
	"one-api/providers"
	providers_base "one-api/providers/base"
	"one-api/types"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	embeddingsRegex = regexp.MustCompile(`(?:^text-|embed|Embed|rerank|davinci|babbage|bge-|e5-|LLM2Vec|retrieval|uae-|gte-|jina-clip|jina-embeddings)`)
	imageRegex      = regexp.MustCompile(`flux|diffusion|stabilityai|sd-|dall|cogview|janus|image`)
	responseRegex   = regexp.MustCompile(`(?:^o[1-9])`)
	noSupportRegex  = regexp.MustCompile(`(?:^tts|rerank|whisper|speech|^mj_|^chirp)`)

	probeChannelFunc = probeChannel
	currentTimeFunc  = time.Now
)

var (
	fullChannelProbeRunningErr = errors.New("通道测试已在运行中")
)

type channelProbeResult struct {
	openaiErr    *types.OpenAIErrorWithStatusCode
	err          error
	milliseconds int64
}

func (result channelProbeResult) consumedSeconds() float64 {
	return float64(result.milliseconds) / 1000.0
}

func (result channelProbeResult) isHealthy() bool {
	return result.err == nil && result.openaiErr == nil
}

func (result channelProbeResult) exceedsThreshold(disableThreshold int64) bool {
	return result.milliseconds > disableThreshold
}

func testChannel(channel *model.Channel, testModel string) (openaiErr *types.OpenAIErrorWithStatusCode, err error) {
	if testModel == "" {
		testModel = channel.TestModel
		if testModel == "" {
			return nil, errors.New("请填写测速模型后再试")
		}
	}

	channelType := getModelType(testModel)
	channel.SetProxy()

	var url string
	switch channelType {
	case "embeddings":
		url = "/v1/embeddings"
	case "image":
		url = "/v1/images/generations"
	case "chat":
		url = "/v1/chat/completions"
	case "response":
		url = "/v1/responses"
	default:
		return nil, errors.New("不支持的模型类型")
	}

	// 创建测试上下文
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	// 获取并验证provider
	provider := providers.GetProvider(channel, c)
	if provider == nil {
		return nil, errors.New("channel not implemented")
	}

	newModelName, err := provider.ModelMappingHandler(testModel)
	if err != nil {
		return nil, err
	}

	newModelName = strings.TrimPrefix(newModelName, "+")

	usage := &types.Usage{}
	provider.SetUsage(usage)

	// 执行测试请求
	var response any
	var openAIErrorWithStatusCode *types.OpenAIErrorWithStatusCode

	switch channelType {
	case "embeddings":
		embeddingsProvider, ok := provider.(providers_base.EmbeddingsInterface)
		if !ok {
			return nil, errors.New("channel not implemented")
		}
		testRequest := &types.EmbeddingRequest{
			Model: newModelName,
			Input: "hi",
		}
		response, openAIErrorWithStatusCode = embeddingsProvider.CreateEmbeddings(testRequest)
	case "image":
		imageProvider, ok := provider.(providers_base.ImageGenerationsInterface)
		if !ok {
			return nil, errors.New("channel not implemented")
		}

		testRequest := &types.ImageRequest{
			Model:  newModelName,
			Prompt: "A cute cat",
			N:      1,
		}
		response, openAIErrorWithStatusCode = imageProvider.CreateImageGenerations(testRequest)
	case "response":
		responseProvider, ok := provider.(providers_base.ResponsesInterface)
		if !ok {
			return nil, errors.New("channel not implemented")
		}

		testRequest := &types.OpenAIResponsesRequest{
			Input:  "You just need to output 'hi' next.",
			Model:  newModelName,
			Stream: false,
		}

		response, openAIErrorWithStatusCode = responseProvider.CreateResponses(testRequest)
	case "chat":
		chatProvider, ok := provider.(providers_base.ChatInterface)
		if !ok {
			return nil, errors.New("channel not implemented")
		}
		testRequest := &types.ChatCompletionRequest{
			Messages: []types.ChatCompletionMessage{
				{
					Role:    "user",
					Content: "You just need to output 'hi' next.",
				},
			},
			Model:  newModelName,
			Stream: false,
		}

		response, openAIErrorWithStatusCode = chatProvider.CreateChatCompletion(testRequest)
	default:
		return nil, errors.New("不支持的模型类型")
	}

	if openAIErrorWithStatusCode != nil {
		return openAIErrorWithStatusCode, errors.New(openAIErrorWithStatusCode.Message)
	}

	// 转换为JSON字符串
	jsonBytes, _ := json.Marshal(response)
	logger.SysLog(fmt.Sprintf("测试渠道 %s : %s 返回内容为：%s", channel.Name, newModelName, string(jsonBytes)))

	return nil, nil
}

func getModelType(modelName string) string {
	if noSupportRegex.MatchString(modelName) {
		return "noSupport"
	}

	if embeddingsRegex.MatchString(modelName) {
		return "embeddings"
	}

	if imageRegex.MatchString(modelName) {
		return "image"
	}

	if responseRegex.MatchString(modelName) {
		return "response"
	}

	return "chat"
}

func probeChannel(channel *model.Channel, testModel string) channelProbeResult {
	tik := currentTimeFunc()
	openaiErr, err := testChannel(channel, testModel)
	tok := currentTimeFunc()

	return channelProbeResult{
		openaiErr:    openaiErr,
		err:          err,
		milliseconds: tok.Sub(tik).Milliseconds(),
	}
}

func channelDisableThresholdMilliseconds() int64 {
	disableThreshold := int64(config.ChannelDisableThreshold * 1000)
	if disableThreshold <= 0 {
		return 10000000 // an impossible value in practice
	}
	return disableThreshold
}

func startFullChannelProbeTask() error {
	channelProbeStateLock.Lock()
	defer channelProbeStateLock.Unlock()

	if fullChannelProbeRunning {
		return fullChannelProbeRunningErr
	}
	fullChannelProbeRunning = true
	return nil
}

func finishFullChannelProbeTask() {
	channelProbeStateLock.Lock()
	fullChannelProbeRunning = false
	channelProbeStateLock.Unlock()
}

func isFullChannelProbeRunning() bool {
	channelProbeStateLock.Lock()
	defer channelProbeStateLock.Unlock()

	return fullChannelProbeRunning
}

func TestChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	channel, err := model.GetChannelById(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	testModel := c.Query("model")
	result := probeChannelFunc(channel, testModel)

	success := false
	msg := ""
	if result.openaiErr != nil {
		if ShouldDisableChannel(channel.Type, result.openaiErr) {
			updated, err := AutoDisableChannel(channel.Id, channel.Name, result.err.Error(), false)
			if err != nil {
				logger.SysError(fmt.Sprintf("failed to auto-disable channel #%d(%s) in single-channel test: %s", channel.Id, channel.Name, err.Error()))
				msg = fmt.Sprintf("测速失败，原因：%s", result.err.Error())
			} else if updated {
				msg = fmt.Sprintf("测速失败，已被禁用，原因：%s", result.err.Error())
			} else {
				msg = fmt.Sprintf("测速失败，原因：%s", result.err.Error())
			}
		} else {
			msg = fmt.Sprintf("测速失败，原因：%s", result.err.Error())
		}
	} else if result.err != nil {
		msg = fmt.Sprintf("测速失败，原因：%s", result.err.Error())
	} else {
		success = true
		msg = "测速成功"
		go channel.UpdateResponseTime(result.milliseconds)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": success,
		"message": msg,
		"time":    result.consumedSeconds(),
	})
}

var channelProbeStateLock sync.Mutex
var fullChannelProbeRunning bool = false

func testAllChannels(isNotify bool) error {
	if err := startFullChannelProbeTask(); err != nil {
		return err
	}
	channels, err := model.GetAllChannels()
	if err != nil {
		finishFullChannelProbeTask()
		return err
	}
	disableThreshold := channelDisableThresholdMilliseconds()
	go func() {
		defer finishFullChannelProbeTask()

		var sendMessage string
		for _, channel := range channels {
			time.Sleep(config.RequestInterval)

			isChannelEnabled := channel.Status == config.ChannelStatusEnabled
			sendMessage += fmt.Sprintf("**通道 %s - #%d - %s** : \n\n", utils.EscapeMarkdownText(channel.Name), channel.Id, channel.StatusToStr())
			result := probeChannelFunc(channel, "")
			// 通道为禁用状态，并且还是请求错误 或者 响应时间超过阈值 直接跳过，也不需要更新响应时间。
			if !isChannelEnabled {
				if result.err != nil {
					sendMessage += fmt.Sprintf("- 测试报错: %s \n\n- 无需改变状态，跳过\n\n", utils.EscapeMarkdownText(result.err.Error()))
					continue
				}
				if result.exceedsThreshold(disableThreshold) {
					sendMessage += fmt.Sprintf("- 响应时间 %.2fs 超过阈值 %.2fs \n\n- 无需改变状态，跳过\n\n", result.consumedSeconds(), float64(disableThreshold)/1000.0)
					continue
				}
				// 如果已被禁用，但是请求成功，需要判断是否需要恢复
				// 手动禁用的通道，不会自动恢复
				if config.AutomaticEnableChannelEnabled && result.isHealthy() {
					if channel.Status == config.ChannelStatusAutoDisabled {
						updated, err := AutoEnableChannel(channel.Id, channel.Name, false)
						if err != nil {
							sendMessage += fmt.Sprintf("- 自动恢复失败: %s \n\n", utils.EscapeMarkdownText(err.Error()))
							continue
						}
						if !updated {
							sendMessage += "- 状态已变化，跳过自动恢复 \n\n"
							continue
						}
						sendMessage += "- 已被启用 \n\n"
					} else {
						sendMessage += "- 手动禁用的通道，不会自动恢复 \n\n"
					}
				}
			} else {
				// 如果通道启用状态，但是返回了错误 或者 响应时间超过阈值，需要判断是否需要禁用
				if result.exceedsThreshold(disableThreshold) {
					errMsg := fmt.Sprintf("响应时间 %.2fs 超过阈值 %.2fs ", result.consumedSeconds(), float64(disableThreshold)/1000.0)
					updated, err := AutoDisableChannel(channel.Id, channel.Name, errMsg, false)
					if err != nil {
						sendMessage += fmt.Sprintf("- %s \n\n- 自动禁用失败: %s\n\n", errMsg, utils.EscapeMarkdownText(err.Error()))
						continue
					}
					if !updated {
						sendMessage += fmt.Sprintf("- %s \n\n- 状态已变化，跳过自动禁用\n\n", errMsg)
						continue
					}
					sendMessage += fmt.Sprintf("- %s \n\n- 禁用\n\n", errMsg)
					continue
				}

				if ShouldDisableChannel(channel.Type, result.openaiErr) {
					errMsg := result.err.Error()
					updated, err := AutoDisableChannel(channel.Id, channel.Name, errMsg, false)
					if err != nil {
						sendMessage += fmt.Sprintf("- 自动禁用失败，原因：%s\n\n", utils.EscapeMarkdownText(err.Error()))
						continue
					}
					if !updated {
						sendMessage += fmt.Sprintf("- 原因：%s\n\n- 状态已变化，跳过自动禁用\n\n", utils.EscapeMarkdownText(errMsg))
						continue
					}
					sendMessage += fmt.Sprintf("- 已被禁用，原因：%s\n\n", utils.EscapeMarkdownText(errMsg))
					continue
				}

				if result.err != nil {
					sendMessage += fmt.Sprintf("- 测试报错: %s \n\n", utils.EscapeMarkdownText(result.err.Error()))
					continue
				}
			}
			channel.UpdateResponseTime(result.milliseconds)
			sendMessage += fmt.Sprintf("- 测试完成，耗时 %.2fs\n\n", result.consumedSeconds())
		}
		if isNotify {
			notify.Send("通道测试完成", sendMessage)
		}
	}()
	return nil
}

func TestAllChannels(c *gin.Context) {
	err := testAllChannels(true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func AutomaticallyTestChannels(frequency int) {
	if frequency <= 0 {
		return
	}

	for {
		time.Sleep(time.Duration(frequency) * time.Minute)
		logger.SysLog("testing all channels")
		err := testAllChannels(false)
		if err != nil {
			if errors.Is(err, fullChannelProbeRunningErr) {
				logger.SysLog("skip channel test: another full channel probe task is running")
				continue
			}
			logger.SysError("channel test failed to start: " + err.Error())
			continue
		}
		logger.SysLog("channel test started")
	}
}
