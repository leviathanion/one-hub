package xunfei

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/common/utils"
	"one-api/common/wsconn"
	"one-api/types"
	"runtime/debug"
	"strings"
	"sync"
)

type xunfeiHandler struct {
	Usage   *types.Usage
	Request *types.ChatCompletionRequest
}

type xunfeiWSReader[T any] struct {
	ctx            context.Context
	conn           *wsconn.ManagedConn
	handlerPrefix  requester.HandlerPrefix[T]
	DataChan       chan T
	ErrChan        chan error
	frameChan      chan []byte
	startOnce      sync.Once
	closeFrameOnce sync.Once
	finishOnce     sync.Once
	done           chan struct{}
}

func (p *XunfeiProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	wsConn, xunfeiRequest, errWithCode := p.getChatRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := &xunfeiHandler{
		Usage:   p.Usage,
		Request: request,
	}

	stream, errWithCode := sendXunfeiWSJSONRequest[XunfeiChatResponse](p.LogContext(), wsConn, xunfeiRequest, chatHandler.handlerNotStream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return chatHandler.convertToChatOpenai(p.LogContext(), stream)
}

func (p *XunfeiProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	wsConn, xunfeiRequest, errWithCode := p.getChatRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := &xunfeiHandler{
		Usage:   p.Usage,
		Request: request,
	}

	return sendXunfeiWSJSONRequest[string](p.LogContext(), wsConn, xunfeiRequest, chatHandler.handlerStream)
}

func (p *XunfeiProvider) getChatRequest(request *types.ChatCompletionRequest) (*wsconn.ManagedConn, *XunfeiChatRequest, *types.OpenAIErrorWithStatusCode) {
	requestCtx := p.LogContext()
	if err := requestCtx.Err(); err != nil {
		return nil, nil, common.ErrorWrapperLocal(err, "ws_request_failed", http.StatusInternalServerError)
	}
	_, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatCompletions)
	if errWithCode != nil {
		return nil, nil, errWithCode
	}

	authUrl := p.GetFullRequestURL(request.Model)
	xunfeiRequest, err := p.convertFromChatOpenai(request)
	if err != nil {
		return nil, nil, common.ErrorWrapperLocal(err, "unsupported_capability", http.StatusBadRequest)
	}

	proxyAddr := ""
	if p != nil && p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	requestctx.SetProviderCredentials(p.Context, providerresponse.ConnectionCredentials(authUrl, nil))
	dialCtx, cancel := context.WithTimeout(requestCtx, config.ConnectTimeout())
	defer cancel()
	wsConn, err := wsconn.DialManaged(dialCtx, authUrl, nil, xunfeiWSConfig(),
		wsconn.WithHandshakeTimeout(config.ConnectTimeout()),
		wsconn.WithProxyURL(proxyAddr),
	)
	if err != nil {
		return nil, nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}

	return wsConn, xunfeiRequest, nil
}

func xunfeiWSConfig() wsconn.Config {
	return wsconn.Config{
		Label:     "xunfei-chat-upstream",
		ReadLimit: config.RealtimeWebsocketReadLimit(),
	}
}

func sendXunfeiWSJSONRequest[T any](ctx context.Context, conn *wsconn.ManagedConn, data any, handlerPrefix requester.HandlerPrefix[T]) (*xunfeiWSReader[T], *types.OpenAIErrorWithStatusCode) {
	closeOnCancel := func() {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "ctx_done", Err: ctx.Err()})
	}
	if err := ctx.Err(); err != nil {
		closeOnCancel()
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}
	stopCancellation := context.AfterFunc(ctx, closeOnCancel)
	defer stopCancellation()
	payload, err := json.Marshal(data)
	if err != nil {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "xunfei_request_build_failed"})
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}
	if err := conn.WriteMessage(wsconn.TextMessage, payload); err != nil {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "xunfei_request_write_failed", Err: err})
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}
	return &xunfeiWSReader[T]{
		ctx:           ctx,
		conn:          conn,
		handlerPrefix: handlerPrefix,
		DataChan:      make(chan T, 1),
		ErrChan:       make(chan error, 1),
		frameChan:     make(chan []byte, 128),
		done:          make(chan struct{}),
	}, nil
}

func (stream *xunfeiWSReader[T]) Recv() (<-chan T, <-chan error) {
	stream.startOnce.Do(func() {
		go stream.runPump()
		go stream.processFrames()
	})
	return stream.DataChan, stream.ErrChan
}

func (stream *xunfeiWSReader[T]) Close() {
	if stream == nil {
		return
	}
	if stream.conn == nil {
		stream.closeFrameChan()
		return
	}
	stream.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "xunfei_reader_close"})
}

func (stream *xunfeiWSReader[T]) CloseAndDrain() {
	if stream == nil {
		return
	}
	data, streamErrors := stream.Recv()
	stream.Close()
	for {
		select {
		case <-data:
		case <-streamErrors:
		case <-stream.done:
			return
		}
	}
}

func (stream *xunfeiWSReader[T]) runPump() {
	if stream == nil || stream.conn == nil {
		return
	}
	wsconn.Pump{
		Conn: stream.conn,
		Handle: func(_ context.Context, _ wsconn.MessageType, payload []byte) {
			frame := append([]byte(nil), payload...)
			select {
			case stream.frameChan <- frame:
			default:
				stream.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindBackpressure, Code: wsconn.CloseTryAgainLater, Reason: "xunfei_frame_backpressure"})
			}
		},
		OnClose: func(info wsconn.CloseInfo) {
			if info.Kind != wsconn.CloseKindNormal {
				var err error
				if info.Err != nil {
					err = info.Err
				} else {
					err = io.EOF
				}
				select {
				case stream.ErrChan <- err:
				default:
				}
			}
			stream.closeFrameChan()
		},
	}.Run(stream.ctx)
}

func (stream *xunfeiWSReader[T]) processFrames() {
	if stream == nil {
		return
	}
	defer stream.finish()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.SysError(fmt.Sprintf("xunfei websocket handler panic: %v", recovered))
			logger.SysError(fmt.Sprintf("stacktrace from panic: %s", string(debug.Stack())))
			select {
			case stream.ErrChan <- errors.New("xunfei websocket handler failed"):
			default:
			}
			if stream.conn != nil {
				stream.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindHandlerPanic, Reason: "xunfei_handler_panic", Err: fmt.Errorf("%v", recovered)})
			}
		}
	}()
	for msg := range stream.frameChan {
		stream.handlerPrefix(&msg, stream.DataChan, stream.ErrChan)
		if bytes.Equal(msg, requester.StreamClosed) {
			stream.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseNormalClosure, Reason: "stream completed"})
			return
		}
	}
}

func (stream *xunfeiWSReader[T]) finish() {
	if stream == nil {
		return
	}
	stream.finishOnce.Do(func() {
		if stream.done != nil {
			close(stream.done)
		}
	})
}

func (stream *xunfeiWSReader[T]) closeFrameChan() {
	if stream == nil {
		return
	}
	stream.closeFrameOnce.Do(func() {
		close(stream.frameChan)
	})
}

func (p *XunfeiProvider) convertFromChatOpenai(request *types.ChatCompletionRequest) (*XunfeiChatRequest, error) {
	if err := validateXunfeiChatRequest(request); err != nil {
		return nil, err
	}
	messages := make([]XunfeiMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.FunctionCall != nil || len(message.ToolCalls) > 0 {
			useToolName := ""
			useToolArgs := ""
			if len(message.ToolCalls) > 0 {
				useToolName = message.ToolCalls[0].Function.Name
				useToolArgs = message.ToolCalls[0].Function.Arguments
			} else {
				useToolName = message.FunctionCall.Name
				useToolArgs = message.FunctionCall.Arguments
			}
			messages = append(messages, XunfeiMessage{
				Role:    message.Role,
				Content: fmt.Sprintf("使用工具：%s，参数：%s", useToolName, useToolArgs),
			})
		} else if message.Role == types.ChatMessageRoleFunction || message.Role == types.ChatMessageRoleTool {
			messages = append(messages, XunfeiMessage{
				Role:    types.ChatMessageRoleUser,
				Content: "这是函数调用返回的内容，请回答之前的问题：\n" + message.StringContent(),
			})
		} else {
			messages = append(messages, XunfeiMessage{
				Role:    message.Role,
				Content: message.StringContent(),
			})
		}
	}

	xunfeiRequest := XunfeiChatRequest{}

	if request.Tools != nil {
		functions := make([]*types.ChatCompletionFunction, 0, len(request.Tools))
		for _, tool := range request.Tools {
			functions = append(functions, &tool.Function)
		}
		xunfeiRequest.Payload.Functions = &XunfeiChatPayloadFunctions{}
		xunfeiRequest.Payload.Functions.Text = functions
	} else if request.Functions != nil {
		xunfeiRequest.Payload.Functions = &XunfeiChatPayloadFunctions{}
		xunfeiRequest.Payload.Functions.Text = request.Functions
	}

	xunfeiRequest.Header.AppId = p.apiId
	xunfeiRequest.Parameter.Chat.Domain = p.domain
	xunfeiRequest.Parameter.Chat.Temperature = request.Temperature
	xunfeiRequest.Parameter.Chat.TopK = request.N
	xunfeiRequest.Parameter.Chat.MaxTokens = request.MaxCompletionTokens
	xunfeiRequest.Payload.Message.Text = messages
	return &xunfeiRequest, nil
}

func validateXunfeiChatRequest(request *types.ChatCompletionRequest) error {
	if request == nil {
		return errors.New("Xunfei requires a Chat request")
	}
	for _, message := range request.Messages {
		if len(message.ToolCalls) == 0 {
			continue
		}
		if len(message.ToolCalls) != 1 || message.ToolCalls[0] == nil || message.ToolCalls[0].Function == nil ||
			(message.ToolCalls[0].Type != "" && message.ToolCalls[0].Type != types.ToolChoiceTypeFunction) || message.ToolCalls[0].Custom != nil {
			return errors.New("Xunfei cannot represent these Chat tool calls")
		}
	}
	for _, tool := range request.Tools {
		if tool == nil || tool.Function.Name == "" || (tool.Type != "" && tool.Type != types.ToolChoiceTypeFunction) {
			return errors.New("Xunfei cannot represent this Chat tool")
		}
	}
	return nil
}

func (h *xunfeiHandler) convertToChatOpenai(ctx context.Context, stream requester.StreamReaderInterface[XunfeiChatResponse]) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	var content string
	var xunfeiResponse XunfeiChatResponse
	dataChan, errChan := stream.Recv()
	defer requester.CloseAndDrainStream(stream)

	appendResponse := func(response XunfeiChatResponse) {
		if len(response.Payload.Choices.Text) > 0 {
			content += response.Payload.Choices.Text[0].Content
		} else {
			response.Payload.Choices.Text = xunfeiResponse.Payload.Choices.Text
		}
		xunfeiResponse = response
	}
	drainBufferedData := func() {
		for dataChan != nil {
			select {
			case response, ok := <-dataChan:
				if !ok {
					dataChan = nil
					return
				}
				appendResponse(response)
			default:
				return
			}
		}
	}

	stop := false
	for !stop {
		select {
		case <-ctx.Done():
			return nil, common.ErrorWrapper(ctx.Err(), "xunfei_failed", http.StatusInternalServerError)
		case response, ok := <-dataChan:
			if !ok {
				dataChan = nil
				if errChan == nil {
					stop = true
				}
				continue
			}
			appendResponse(response)
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				if dataChan == nil {
					stop = true
				}
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, common.ErrorWrapper(err, "xunfei_failed", http.StatusInternalServerError)
			}

			if errors.Is(err, io.EOF) {
				drainBufferedData()
				stop = true
			}
		}
	}

	if len(xunfeiResponse.Payload.Choices.Text) == 0 {
		xunfeiResponse.Payload.Choices.Text = []XunfeiChatResponseTextItem{{}}
	}
	xunfeiResponse.Payload.Choices.Text[0].Content = content

	choice := types.ChatCompletionChoice{
		Index:        0,
		FinishReason: types.FinishReasonStop,
	}

	xunfeiText := xunfeiResponse.Payload.Choices.Text[0]

	if xunfeiText.FunctionCall != nil {
		choice.Message = types.ChatCompletionMessage{
			Role: "assistant",
		}

		if h.Request.Tools != nil {
			choice.Message.ToolCalls = []*types.ChatCompletionToolCalls{
				{
					Id:       xunfeiResponse.Header.Sid,
					Type:     "function",
					Function: xunfeiText.FunctionCall,
				},
			}
			choice.FinishReason = types.FinishReasonToolCalls
		} else {
			choice.Message.FunctionCall = xunfeiText.FunctionCall
			choice.FinishReason = types.FinishReasonFunctionCall
		}
	} else {
		choice.Message = types.ChatCompletionMessage{
			Role:    "assistant",
			Content: xunfeiText.Content,
		}
	}

	fullTextResponse := &types.ChatCompletionResponse{
		ID:      xunfeiResponse.Header.Sid,
		Object:  "chat.completion",
		Model:   h.Request.Model,
		Created: utils.GetTimestamp(),
		Choices: []types.ChatCompletionChoice{choice},
		Usage:   xunfeiResponse.Payload.Usage.Text,
	}

	return fullTextResponse, nil
}

func (h *xunfeiHandler) handlerData(rawLine *[]byte, isFinished *bool) (*XunfeiChatResponse, error) {
	// 如果rawLine 前缀不为{，则直接返回
	if !strings.HasPrefix(string(*rawLine), "{") {
		*rawLine = nil
		return nil, nil
	}

	var xunfeiChatResponse XunfeiChatResponse
	err := json.Unmarshal(*rawLine, &xunfeiChatResponse)
	if err != nil {
		return nil, common.ErrorToOpenAIError(err)
	}

	aiError := errorHandle(&xunfeiChatResponse)
	if aiError != nil {
		return nil, aiError
	}

	if xunfeiChatResponse.Payload.Choices.Status == 2 {
		*isFinished = true
	}

	if *isFinished && xunfeiChatResponse.Payload.Usage.Text != nil && h.Usage != nil {
		usage := *xunfeiChatResponse.Payload.Usage.Text
		usage.MarkProviderReported()
		if usage.HasProviderUsage() {
			*h.Usage = usage
		}
	}

	return &xunfeiChatResponse, nil
}

func (h *xunfeiHandler) handlerNotStream(rawLine *[]byte, dataChan chan XunfeiChatResponse, errChan chan error) {
	isFinished := false
	xunfeiChatResponse, err := h.handlerData(rawLine, &isFinished)
	if err != nil {
		errChan <- err
		return
	}

	if *rawLine == nil {
		return
	}

	dataChan <- *xunfeiChatResponse

	if isFinished {
		errChan <- io.EOF
		*rawLine = requester.StreamClosed
	}
}

func (h *xunfeiHandler) handlerStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	isFinished := false
	xunfeiChatResponse, err := h.handlerData(rawLine, &isFinished)
	if err != nil {
		errChan <- err
		return
	}

	if *rawLine == nil {
		return
	}

	h.convertToOpenaiStream(xunfeiChatResponse, dataChan)

	if isFinished {
		errChan <- io.EOF
		*rawLine = requester.StreamClosed
	}
}

func (h *xunfeiHandler) convertToOpenaiStream(xunfeiChatResponse *XunfeiChatResponse, dataChan chan string) {
	if len(xunfeiChatResponse.Payload.Choices.Text) == 0 {
		xunfeiChatResponse.Payload.Choices.Text = []XunfeiChatResponseTextItem{{}}
	}

	choice := types.ChatCompletionStreamChoice{
		Index: 0,
		Delta: types.ChatCompletionStreamChoiceDelta{
			Role: types.ChatMessageRoleAssistant,
		},
	}
	xunfeiText := xunfeiChatResponse.Payload.Choices.Text[0]

	if xunfeiText.FunctionCall != nil {
		if h.Request.Tools != nil {
			choice.Delta.ToolCalls = []*types.ChatCompletionToolCalls{
				{
					Id:       xunfeiChatResponse.Header.Sid,
					Index:    0,
					Type:     "function",
					Function: xunfeiText.FunctionCall,
				},
			}
			choice.FinishReason = types.FinishReasonToolCalls
		} else {
			choice.Delta.FunctionCall = xunfeiText.FunctionCall
			choice.FinishReason = types.FinishReasonFunctionCall
		}
	} else {
		choice.Delta.Content = xunfeiChatResponse.Payload.Choices.Text[0].Content
		if xunfeiChatResponse.Payload.Choices.Status == 2 {
			choice.FinishReason = types.FinishReasonStop
		}
	}

	chatCompletion := types.ChatCompletionStreamResponse{
		ID:      xunfeiChatResponse.Header.Sid,
		Object:  "chat.completion.chunk",
		Created: utils.GetTimestamp(),
		Model:   h.Request.Model,
	}

	if xunfeiText.FunctionCall == nil {
		chatCompletion.Choices = []types.ChatCompletionStreamChoice{choice}
		responseBody, _ := json.Marshal(chatCompletion)
		dataChan <- string(responseBody)
	} else {
		choices := choice.ConvertOpenaiStream()
		for _, choice := range choices {
			chatCompletionCopy := chatCompletion
			chatCompletionCopy.Choices = []types.ChatCompletionStreamChoice{choice}
			responseBody, _ := json.Marshal(chatCompletionCopy)
			dataChan <- string(responseBody)
		}
	}
}
