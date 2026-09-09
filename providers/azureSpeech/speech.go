package azureSpeech

import (
	"bytes"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/types"
	"strings"
)

var outputFormatMap = map[string]string{
	"mp3":  "audio-16khz-128kbitrate-mono-mp3",
	"opus": "audio-16khz-128kbitrate-mono-opus",
	"aac":  "audio-24khz-160kbitrate-mono-mp3",
	"flac": "audio-48khz-192kbitrate-mono-mp3",
}

func CreateSSML(text string, name string, role string) string {
	ssmlTemplate := `<speak version='1.0' xml:lang='en-US'>
        <voice xml:lang='en-US' %s name='%s'>
            %s
        </voice>
    </speak>`

	roleAttribute := ""
	if role != "" {
		roleAttribute = fmt.Sprintf("role='%s'", role)
	}

	return fmt.Sprintf(ssmlTemplate, roleAttribute, name, text)
}

func (p *AzureSpeechProvider) GetVoiceMap() map[string][]string {
	defaultVoiceMapping := map[string][]string{
		"alloy":   {"zh-CN-YunxiNeural"},
		"echo":    {"zh-CN-YunyangNeural"},
		"fable":   {"zh-CN-YunxiNeural", "Boy"},
		"onyx":    {"zh-CN-YunyeNeural"},
		"nova":    {"zh-CN-XiaochenNeural"},
		"shimmer": {"zh-CN-XiaohanNeural"},
	}

	if p.Channel.Plugin == nil {
		return defaultVoiceMapping
	}

	customVoiceMapping, ok := p.Channel.Plugin.Data()["voice"]
	if !ok {
		return defaultVoiceMapping
	}

	for key, value := range customVoiceMapping {
		if _, exists := defaultVoiceMapping[key]; !exists {
			continue
		}
		customVoiceValue, isString := value.(string)
		if !isString || customVoiceValue == "" {
			continue
		}
		customizeVoice := strings.Split(customVoiceValue, "|")
		defaultVoiceMapping[key] = customizeVoice
	}

	return defaultVoiceMapping
}

func (p *AzureSpeechProvider) getRequestBody(request *types.SpeechAudioRequest) *bytes.Buffer {
	var voice, role string
	voice, _ = request.VoiceString()
	voiceMap := p.GetVoiceMap()
	if mapped := voiceMap[voice]; mapped != nil {
		voice = mapped[0]
		if len(mapped) > 1 {
			role = mapped[1]
		}
	}

	ssml := CreateSSML(request.Input, voice, role)

	return bytes.NewBufferString(ssml)
}

func (p *AzureSpeechProvider) CreateSpeech(request *types.SpeechAudioRequest) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	if _, ok := request.VoiceString(); !ok {
		return nil, common.StringErrorWrapperLocal("selected provider requires a string voice", "unsupported_capability", http.StatusBadRequest)
	}
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeAudioSpeech)
	if errWithCode != nil {
		return nil, errWithCode
	}
	fullRequestURL := p.GetFullRequestURL(url)
	headers := p.GetRequestHeaders()
	responseFormatr := outputFormatMap[request.ResponseFormat]
	if responseFormatr == "" {
		responseFormatr = outputFormatMap["mp3"]
	}
	headers["X-Microsoft-OutputFormat"] = responseFormatr

	requestBody := p.getRequestBody(request)

	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(requestBody), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	defer req.Body.Close()

	var resp *http.Response
	resp, errWithCode = p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	p.Usage.TotalTokens = p.Usage.PromptTokens

	return resp, nil
}
