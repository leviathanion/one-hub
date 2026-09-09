package relay_util

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/utils"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type responsesHandler func(response *types.OpenAIResponsesStreamResponses)

type OpenAIResponsesStreamConverter struct {
	sequenceNumber    int
	lastResponseType  string
	outputIndex       int
	contentIndex      int
	summaryIndex      int
	responses         *types.OpenAIResponsesResponses
	item              *types.ResponsesOutput
	part              *types.ContentResponses
	content           []types.ContentResponses
	itemID            string
	isFirstResponse   bool
	isCompleted       bool
	isTerminal        bool
	terminalErr       error
	c                 *gin.Context
	nowStatus         string
	lastToolCallIndex int
	usage             *types.Usage
	partTextBuilder   strings.Builder
	argsBuilder       strings.Builder
	processedBytes    int64
	maxProcessedBytes int64
}

const responsesStreamConverterMaxProcessedBytes int64 = 64 << 20

var errResponsesStreamConverterStateLimit = errors.New("responses stream converter state limit exceeded")

func NewOpenAIResponsesStreamConverter(c *gin.Context, request *types.OpenAIResponsesRequest, usage *types.Usage) *OpenAIResponsesStreamConverter {
	converter := &OpenAIResponsesStreamConverter{
		sequenceNumber:    0,
		outputIndex:       0,
		contentIndex:      0,
		summaryIndex:      0,
		isFirstResponse:   true,
		c:                 c,
		lastToolCallIndex: -1,
		usage:             usage,
		maxProcessedBytes: responsesStreamConverterMaxProcessedBytes,
	}

	converter.initializeResponse(request)

	return converter
}

func (converter *OpenAIResponsesStreamConverter) initializeResponse(request *types.OpenAIResponsesRequest) {
	text := any(types.TextResponses{
		Format: struct {
			Type string `json:"type"`
		}{
			Type: "text",
		},
	})
	if request.Text != nil {
		text = request.Text
	}

	converter.responses = &types.OpenAIResponsesResponses{
		Object:               "response",
		Text:                 text,
		MaxOutputTokens:      request.MaxOutputTokens,
		MaxToolCalls:         request.MaxToolCalls,
		Background:           request.Background,
		Conversation:         request.Conversation,
		Instructions:         request.Instructions,
		Metadata:             request.Metadata,
		ParallelToolCalls:    request.ParallelToolCalls,
		PreviousResponseID:   request.PreviousResponseID,
		Prompt:               request.Prompt,
		PromptCacheKey:       request.PromptCacheKey,
		PromptCacheRetention: request.PromptCacheRetention,
		Reasoning:            request.Reasoning,
		Temperature:          request.Temperature,
		SafetyIdentifier:     request.SafetyIdentifier,
		ProcessingClass:      request.ProcessingClass,
		Store:                request.Store,
		ToolChoice:           request.ToolChoice,
		TopP:                 request.TopP,
		Truncation:           request.Truncation,
		Tools:                request.Tools,
		Output:               make([]types.ResponsesOutput, 0),
		Status:               "in_progress",
	}
}

func (converter *OpenAIResponsesStreamConverter) ProcessStreamData(jsonStr string) error {
	if converter == nil || converter.isTerminal {
		if converter == nil {
			return nil
		}
		return converter.terminalErr
	}
	if jsonStr == "[DONE]" {
		converter.finalizeStream()
		return nil
	}
	incomingBytes := int64(len(jsonStr))
	if incomingBytes > converter.maxProcessedBytes-converter.processedBytes {
		return converter.sendError("provider_usage_state_limit", "provider stream state limit exceeded", errResponsesStreamConverterStateLimit)
	}
	converter.processedBytes += incomingBytes

	var response types.ChatCompletionStreamResponse
	if err := json.Unmarshal([]byte(jsonStr), &response); err != nil {
		return converter.sendError("invalid_provider_response", "stream interrupted", errors.New("chat completion stream payload is malformed"))
	}
	if err := converter.validateChatStreamChoicesForResponses(response.Choices); err != nil {
		return converter.sendError("invalid_provider_response", "stream interrupted", err)
	}

	if strings.TrimSpace(response.Model) != "" {
		converter.responses.Model = response.Model
	}
	if strings.TrimSpace(response.ServiceTier) != "" {
		converter.responses.ServiceTier = response.ServiceTier
	}

	// 第一次响应创建response.created
	if converter.isFirstResponse {
		converter.responses.ID = response.ID
		converter.responses.CreatedAt = response.Created
		converter.sendStreamResponse("response.created", converter.populateResponseData)
		converter.sendStreamResponse("response.in_progress", converter.populateResponseData)
		converter.isFirstResponse = false
	}

	converter.processChoices(response.Choices)
	return nil
}

func ResponsesStreamFailureCode(err error) string {
	if errors.Is(err, errResponsesStreamConverterStateLimit) || errors.Is(err, requester.ErrSSEEventTooLarge) || errors.Is(err, requester.ErrStreamLineTooLarge) {
		return "provider_usage_state_limit"
	}
	var apiErr *types.OpenAIErrorWithStatusCode
	if errors.As(err, &apiErr) && apiErr != nil {
		if code, ok := apiErr.Code.(string); ok {
			switch strings.TrimSpace(code) {
			case "provider_usage_state_limit", "provider_protocol_error":
				return strings.TrimSpace(code)
			}
		}
	}
	return "invalid_provider_response"
}

func (converter *OpenAIResponsesStreamConverter) validateChatStreamChoicesForResponses(choices []types.ChatCompletionStreamChoice) error {
	if len(choices) > 1 {
		return errors.New("chat completion stream returned multiple choices")
	}
	for _, choice := range choices {
		if choice.Index != 0 {
			return errors.New("chat completion stream returned an unsupported choice index")
		}
		if role := strings.TrimSpace(choice.Delta.Role); role != "" && role != "assistant" {
			return errors.New("chat completion stream returned an unsupported output role")
		}
		if choice.Delta.FunctionCall != nil || choice.Delta.Reasoning != "" || len(choice.Delta.Image) > 0 || len(choice.Delta.Images) > 0 {
			return errors.New("chat completion stream contains an unsupported delta shape")
		}
		payloadVariants := 0
		if choice.Delta.Content != "" {
			payloadVariants++
		}
		if choice.Delta.Refusal != "" {
			payloadVariants++
		}
		if choice.Delta.ReasoningContent != "" {
			payloadVariants++
		}
		if len(choice.Delta.ToolCalls) > 0 {
			payloadVariants++
		}
		if payloadVariants > 1 {
			return errors.New("chat completion stream delta contains multiple output variants")
		}
		if len(choice.Delta.ToolCalls) == 0 {
			continue
		}
		if len(choice.Delta.ToolCalls) != 1 {
			return errors.New("chat completion stream returned multiple tool calls in one chunk")
		}
		tool := choice.Delta.ToolCalls[0]
		if tool == nil || tool.Function == nil || tool.Custom != nil || strings.TrimSpace(tool.Type) != "" && strings.TrimSpace(tool.Type) != "function" {
			return errors.New("chat completion stream contains an unsupported tool call")
		}
		if tool.Index < 0 {
			return errors.New("chat completion stream contains an invalid tool call index")
		}
		// A tool call that follows a different output item starts a new
		// function-call item. Require its identity up front, while keeping the
		// index and identity checks strict for deltas of the active call.
		if converter.lastResponseType != types.InputTypeFunctionCall {
			if strings.TrimSpace(tool.Id) == "" || strings.TrimSpace(tool.Function.Name) == "" {
				return errors.New("chat completion stream tool call requires call_id and function name")
			}
			continue
		}
		if tool.Index != converter.lastToolCallIndex {
			// Claude's content_block_start is projected as a tool chunk with
			// the new call's identity and an empty initial argument delta. A
			// changed index carrying arguments is still an interleaved delta of
			// the active call and must be rejected.
			if strings.TrimSpace(tool.Id) == "" || strings.TrimSpace(tool.Function.Name) == "" || tool.Function.Arguments != "" {
				return errors.New("chat completion stream returned multiple or interleaved tool calls")
			}
			continue
		}
		if converter.item != nil {
			if id := strings.TrimSpace(tool.Id); id != "" && converter.item.CallID != "" && id != converter.item.CallID {
				return errors.New("chat completion stream tool call id changed")
			}
			if name := strings.TrimSpace(tool.Function.Name); name != "" && converter.item.Name != "" && name != converter.item.Name {
				return errors.New("chat completion stream function name changed")
			}
		}
	}
	return nil
}

func (converter *OpenAIResponsesStreamConverter) ProcessStreamError() error {
	return converter.sendError("invalid_provider_response", "stream interrupted", errors.New("chat completion stream interrupted"))
}

// 处理choices
func (converter *OpenAIResponsesStreamConverter) processChoices(choices []types.ChatCompletionStreamChoice) {
	for _, choice := range choices {
		nowStatus, ok := choice.FinishReason.(string)
		if ok {
			converter.nowStatus = types.ConvertChatStatusToResponses(nowStatus)
		}

		if isEmptyChoiceDelta(&choice) {
			continue
		}

		currentType := converter.GetResponseType(&choice)
		// 检查是否需要创建新的output_item
		needNewOutputItem := false
		if converter.lastResponseType != currentType {
			needNewOutputItem = true
		}

		if currentType == types.InputTypeFunctionCall {
			if len(choice.Delta.ToolCalls) > 0 && converter.lastToolCallIndex != choice.Delta.ToolCalls[0].Index {
				needNewOutputItem = true
				converter.lastToolCallIndex = choice.Delta.ToolCalls[0].Index
			}
		}

		if needNewOutputItem {
			converter.createNewItem(choice, currentType)
		}

		// 处理不同类型的内容
		switch currentType {
		case types.InputTypeReasoning:
			converter.processReasoning(choice)
		case types.InputTypeFunctionCall:
			converter.processFunctionCall(choice)
		default:
			converter.processMessage(choice)
		}

		converter.lastResponseType = currentType
	}
}

// 创建新的输出项
func (converter *OpenAIResponsesStreamConverter) createNewItem(choice types.ChatCompletionStreamChoice, currentType string) {
	// 如果是新的输出类型，先结束上一个输出
	if converter.item != nil {
		converter.done()
	}

	// 生成新的itemID
	converter.generateResponseItemID(currentType)

	response := converter.buildStreamResponse("response.output_item.added")

	converter.item = &types.ResponsesOutput{
		ID:     converter.itemID,
		Type:   currentType,
		Status: "in_progress",
	}

	switch currentType {
	case types.InputTypeFunctionCall:
		converter.argsBuilder.Reset()
		if len(choice.Delta.ToolCalls) > 0 && choice.Delta.ToolCalls[0].Function != nil {
			converter.item.CallID = choice.Delta.ToolCalls[0].Id
			converter.item.Name = choice.Delta.ToolCalls[0].Function.Name
		}
	case types.InputTypeReasoning:
		converter.item.Summary = types.SummaryResponsesList{}
	default:
		converter.item.Role = "assistant"
		converter.item.Content = []types.ContentResponses{}
	}

	response.Item = converter.item

	converter.sendStreamEvent(response, "response.output_item.added")
}

// 结束
func (converter *OpenAIResponsesStreamConverter) done() {
	switch converter.lastResponseType {
	case types.InputTypeMessage:
		if converter.part != nil {
			converter.doneMessagePart()
		}
	case types.InputTypeReasoning:
		if converter.part != nil {
			converter.doneReasoningPart()
		}
	case types.InputTypeFunctionCall:
		converter.doneFunctionCall()
	}

	response := converter.buildStreamResponse("response.output_item.done")
	response.OutputIndex = &converter.outputIndex

	converter.item.Status = converter.nowStatus
	if converter.lastResponseType == types.InputTypeMessage {
		converter.item.Content = converter.content
	}
	response.Item = converter.item

	if converter.item.Status == "" {
		converter.item.Status = types.ResponseStatusCompleted
	}

	converter.responses.Output = append(converter.responses.Output, *converter.item)

	converter.sendStreamEvent(response, "response.output_item.done")
	// 清空 item
	converter.item = nil
	// 清空 content
	converter.content = nil

	converter.outputIndex++
	converter.contentIndex = 0
	converter.summaryIndex = 0
}

// 处理message类型的内容
func (converter *OpenAIResponsesStreamConverter) processMessage(choice types.ChatCompletionStreamChoice) {
	isRefusal := choice.Delta.Refusal != ""
	partType := types.ContentTypeOutputText
	if isRefusal {
		partType = types.ContentTypeRefusal
	}
	// 检查是否需要创建content_part.added
	if converter.part != nil && converter.part.Type != partType {
		// 先结束掉上一个part
		converter.doneMessagePart()
	}

	if converter.part == nil {
		// 创建新的part
		converter.part = &types.ContentResponses{
			Type: partType,
			Text: "",
		}
		converter.partTextBuilder.Reset()

		response := converter.buildStreamResponseWithItemID("response.content_part.added")
		response.ContentIndex = &converter.contentIndex
		response.Part = converter.part
		converter.sendStreamEvent(response, "response.content_part.added")
	}

	// 处理文本内容
	eventType := "response.output_text.delta"
	delta := choice.Delta.Content
	if isRefusal {
		eventType = "response.refusal.delta"
		delta = choice.Delta.Refusal
	}
	response := converter.buildStreamResponseWithItemID(eventType)
	response.ContentIndex = &converter.contentIndex
	response.Delta = delta
	converter.sendStreamEvent(response, eventType)

	// 处理文本增量
	converter.partTextBuilder.WriteString(delta)
}

// 结束message part
func (converter *OpenAIResponsesStreamConverter) doneMessagePart() {
	// 先结束掉 response.output_text.done
	eventType := "response.output_text.done"
	if converter.part.Type == types.ContentTypeRefusal {
		eventType = "response.refusal.done"
	}
	response := converter.buildStreamResponseWithItemID(eventType)
	response.ContentIndex = &converter.contentIndex
	if converter.part.Type == types.ContentTypeRefusal {
		converter.part.Refusal = converter.partTextBuilder.String()
		refusal := converter.part.Refusal
		response.Refusal = &refusal
	} else {
		converter.part.Text = converter.partTextBuilder.String()
		text := converter.part.Text
		response.Text = &text
	}
	converter.sendStreamEvent(response, eventType)

	// 结束 part
	response = converter.buildStreamResponseWithItemID("response.content_part.done")
	response.ContentIndex = &converter.contentIndex
	part := *converter.part
	response.Part = &part
	converter.sendStreamEvent(response, "response.content_part.done")

	// contentIndex 递增
	converter.contentIndex++
	// 需要将数据添加到content中
	converter.addContent()
	// 清空 part
	converter.part = nil
}

// 处理reasoning类型的内容
func (converter *OpenAIResponsesStreamConverter) processReasoning(choice types.ChatCompletionStreamChoice) {
	if converter.part == nil {
		// 创建新的part
		converter.part = &types.ContentResponses{
			Type: types.ContentTypeSummaryText,
			Text: "",
		}
		converter.partTextBuilder.Reset()

		response := converter.buildStreamResponseWithItemID("response.reasoning_summary_part.added")
		response.SummaryIndex = &converter.summaryIndex
		response.Part = converter.part
		converter.sendStreamEvent(response, "response.reasoning_summary_part.added")
	}

	// 处理推理内容
	response := converter.buildStreamResponseWithItemID("response.reasoning_summary_text.delta")
	response.SummaryIndex = &converter.summaryIndex
	response.Delta = choice.Delta.ReasoningContent
	converter.sendStreamEvent(response, "response.reasoning_summary_text.delta")

	// 处理文本增量
	converter.partTextBuilder.WriteString(choice.Delta.ReasoningContent)
}

// 结束reasoning part
func (converter *OpenAIResponsesStreamConverter) doneReasoningPart() {
	// 先结束掉 response.reasoning_summary_text.done
	response := converter.buildStreamResponseWithItemID("response.reasoning_summary_text.done")
	response.SummaryIndex = &converter.summaryIndex
	converter.part.Text = converter.partTextBuilder.String()
	text := converter.part.Text
	response.Text = &text
	converter.sendStreamEvent(response, "response.reasoning_summary_text.done")

	// 结束 part
	response = converter.buildStreamResponseWithItemID("response.reasoning_summary_part.done")
	response.SummaryIndex = &converter.summaryIndex
	part := *converter.part
	response.Part = &part
	converter.sendStreamEvent(response, "response.reasoning_summary_part.done")

	converter.addSummary()
	converter.summaryIndex++
	converter.part = nil
}

// 处理function call类型的内容
func (converter *OpenAIResponsesStreamConverter) processFunctionCall(choice types.ChatCompletionStreamChoice) {
	response := converter.buildStreamResponseWithItemID("response.function_call_arguments.delta")

	if len(choice.Delta.ToolCalls) > 0 && choice.Delta.ToolCalls[0].Function != nil {
		tool := choice.Delta.ToolCalls[0]
		if converter.item.CallID == "" && strings.TrimSpace(tool.Id) != "" {
			converter.item.CallID = tool.Id
		}
		if converter.item.Name == "" && strings.TrimSpace(tool.Function.Name) != "" {
			converter.item.Name = tool.Function.Name
		}
		response.Delta = tool.Function.Arguments
		converter.argsBuilder.WriteString(tool.Function.Arguments)
	}

	converter.sendStreamEvent(response, "response.function_call_arguments.delta")
}

// 结束function call
func (converter *OpenAIResponsesStreamConverter) doneFunctionCall() {
	response := converter.buildStreamResponseWithItemID("response.function_call_arguments.done")
	if converter.item != nil {
		arguments := converter.argsBuilder.String()
		converter.item.Arguments = &arguments
	}
	response.Arguments = converter.item.Arguments

	converter.sendStreamEvent(response, "response.function_call_arguments.done")
}

func (converter *OpenAIResponsesStreamConverter) addContent() {
	if converter.part == nil {
		return
	}

	if converter.content == nil {
		converter.content = make([]types.ContentResponses, 0)
	}

	converter.content = append(converter.content, *converter.part)
}

func (converter *OpenAIResponsesStreamConverter) addSummary() {
	if converter.part == nil || converter.item == nil {
		return
	}

	converter.item.Summary = append(converter.item.Summary, types.SummaryResponses{
		Type: converter.part.Type,
		Text: converter.part.Text,
	})
}

// 输出最终的数据
func (converter *OpenAIResponsesStreamConverter) finalizeStream() {
	if converter == nil || converter.isTerminal {
		return
	}
	if converter.item != nil {
		converter.done()
	}

	respType := "response.completed"
	finalStatus := converter.nowStatus

	switch finalStatus {
	case types.ResponseStatusFailed:
		respType = "response.failed"
	case types.ResponseStatusIncomplete:
		respType = "response.incomplete"
	default:
		finalStatus = types.ResponseStatusCompleted
	}

	response := converter.buildStreamResponse(respType)
	response.Response = converter.responses
	response.Response.Status = finalStatus

	if converter.usage != nil && converter.usage.HasProviderUsage() {
		response.Response.Usage = converter.usage.ToResponsesUsage()
	}
	converter.isCompleted = true
	converter.isTerminal = true

	converter.sendStreamEvent(response, respType)
}

func (converter *OpenAIResponsesStreamConverter) FinalResponse() *types.OpenAIResponsesResponses {
	if converter == nil || !converter.isCompleted || converter.responses == nil {
		return nil
	}

	response := *converter.responses
	return &response
}

// 获取响应流字符串
func (converter *OpenAIResponsesStreamConverter) sendStreamEvent(resp any, responseType string) {
	respStr, err := json.Marshal(resp)
	if err != nil {
		return
	}

	writer := GetStreamWriter(converter.c)
	_, _ = writer.WriteString("event: ")
	_, _ = writer.WriteString(responseType)
	_, _ = writer.WriteString("\ndata: ")
	_, _ = writer.Write(respStr)
	_, _ = writer.WriteString("\n\n")
}

// 错误响应
func (converter *OpenAIResponsesStreamConverter) sendError(code, message string, terminalErr error) error {
	if converter == nil {
		return terminalErr
	}
	if converter.isTerminal {
		return converter.terminalErr
	}
	converter.isTerminal = true
	converter.terminalErr = terminalErr
	response := commonresponses.NewStreamErrorEvent(int64(converter.sequenceNumber), code, message)
	converter.sequenceNumber++
	converter.sendStreamEvent(response, "error")
	return terminalErr
}

func (converter *OpenAIResponsesStreamConverter) generateResponseItemID(responseType string) {
	prefix := ""
	switch responseType {
	case types.InputTypeFunctionCall:
		prefix = "fc"
	case types.InputTypeReasoning:
		prefix = "rs"
	default:
		prefix = "msg"
	}

	converter.itemID = fmt.Sprintf("%s_%s", prefix, utils.GetRandomString(48))
}

func (converter *OpenAIResponsesStreamConverter) buildStreamResponse(responseType string) *types.OpenAIResponsesStreamResponses {
	response := &types.OpenAIResponsesStreamResponses{
		Type:           responseType,
		SequenceNumber: converter.sequenceNumber,
	}

	converter.sequenceNumber++

	return response
}

func (converter *OpenAIResponsesStreamConverter) buildStreamResponseWithItemID(responseType string) *types.OpenAIResponsesStreamResponses {
	response := converter.buildStreamResponse(responseType)
	response.ItemID = converter.itemID
	response.OutputIndex = &converter.outputIndex
	return response
}

func (converter *OpenAIResponsesStreamConverter) sendStreamResponse(responseType string, fn responsesHandler) {
	response := converter.buildStreamResponse(responseType)
	if fn != nil {
		fn(response)
	}

	converter.sendStreamEvent(response, responseType)
}

func (converter *OpenAIResponsesStreamConverter) populateResponseData(response *types.OpenAIResponsesStreamResponses) {
	response.Response = converter.responses
}

func (converter *OpenAIResponsesStreamConverter) GetResponseType(choice *types.ChatCompletionStreamChoice) string {
	if len(choice.Delta.ToolCalls) > 0 {
		return types.InputTypeFunctionCall
	}

	if choice.Delta.ReasoningContent != "" {
		return types.InputTypeReasoning
	}

	return types.InputTypeMessage
}

func isEmptyChoiceDelta(choice *types.ChatCompletionStreamChoice) bool {
	if choice == nil {
		return true
	}

	return choice.Delta.Content == "" &&
		choice.Delta.Refusal == "" &&
		choice.Delta.FunctionCall == nil &&
		len(choice.Delta.ToolCalls) == 0 &&
		choice.Delta.ReasoningContent == "" &&
		choice.Delta.Reasoning == "" &&
		len(choice.Delta.Image) == 0 &&
		len(choice.Delta.Images) == 0
}
