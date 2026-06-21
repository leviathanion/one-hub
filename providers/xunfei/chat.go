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
	conn           *wsconn.ManagedConn
	handlerPrefix  requester.HandlerPrefix[T]
	DataChan       chan T
	ErrChan        chan error
	frameChan      chan []byte
	startOnce      sync.Once
	closeFrameOnce sync.Once
}

func (p *XunfeiProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	wsConn, errWithCode := p.getChatRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	xunfeiRequest := p.convertFromChatOpenai(request)

	chatHandler := &xunfeiHandler{
		Usage:   p.Usage,
		Request: request,
	}

	stream, errWithCode := sendXunfeiWSJSONRequest[XunfeiChatResponse](wsConn, xunfeiRequest, chatHandler.handlerNotStream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return chatHandler.convertToChatOpenai(stream)
}

func (p *XunfeiProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	wsConn, errWithCode := p.getChatRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	xunfeiRequest := p.convertFromChatOpenai(request)

	chatHandler := &xunfeiHandler{
		Usage:   p.Usage,
		Request: request,
	}

	return sendXunfeiWSJSONRequest[string](wsConn, xunfeiRequest, chatHandler.handlerStream)
}

func (p *XunfeiProvider) getChatRequest(request *types.ChatCompletionRequest) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	_, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatCompletions)
	if errWithCode != nil {
		return nil, errWithCode
	}

	authUrl := p.GetFullRequestURL(request.Model)

	proxyAddr := ""
	if p != nil && p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), config.ConnectTimeout())
	defer cancel()
	wsConn, err := wsconn.DialManaged(dialCtx, authUrl, nil, xunfeiWSConfig(),
		wsconn.WithHandshakeTimeout(config.ConnectTimeout()),
		wsconn.WithProxyURL(proxyAddr),
	)
	if err != nil {
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}

	return wsConn, nil
}

func xunfeiWSConfig() wsconn.Config {
	return wsconn.Config{
		Label:     "xunfei-chat-upstream",
		ReadLimit: config.RealtimeWebsocketReadLimit(),
	}
}

func sendXunfeiWSJSONRequest[T any](conn *wsconn.ManagedConn, data any, handlerPrefix requester.HandlerPrefix[T]) (*xunfeiWSReader[T], *types.OpenAIErrorWithStatusCode) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}
	if err := conn.WriteMessage(wsconn.TextMessage, payload); err != nil {
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}
	return &xunfeiWSReader[T]{
		conn:          conn,
		handlerPrefix: handlerPrefix,
		DataChan:      make(chan T, 1),
		ErrChan:       make(chan error, 1),
		frameChan:     make(chan []byte, 128),
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
	if stream == nil || stream.conn == nil {
		return
	}
	stream.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "xunfei_reader_close"})
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
			stream.closeFrameChan()
			if info.Kind == wsconn.CloseKindNormal {
				return
			}
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
		},
	}.Run(context.Background())
}

func (stream *xunfeiWSReader[T]) processFrames() {
	if stream == nil {
		return
	}
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

func (stream *xunfeiWSReader[T]) closeFrameChan() {
	if stream == nil {
		return
	}
	stream.closeFrameOnce.Do(func() {
		close(stream.frameChan)
	})
}

func (p *XunfeiProvider) convertFromChatOpenai(request *types.ChatCompletionRequest) *XunfeiChatRequest {
	messages := make([]XunfeiMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.FunctionCall != nil || message.ToolCalls != nil {
			useToolName := ""
			useToolArgs := ""
			if message.ToolCalls != nil {
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
	return &xunfeiRequest
}

func (h *xunfeiHandler) convertToChatOpenai(stream requester.StreamReaderInterface[XunfeiChatResponse]) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	var content string
	var xunfeiResponse XunfeiChatResponse
	dataChan, errChan := stream.Recv()
	defer stream.Close()

	appendResponse := func(response XunfeiChatResponse) {
		if len(response.Payload.Choices.Text) == 0 {
			return
		}
		xunfeiResponse = response
		content += xunfeiResponse.Payload.Choices.Text[0].Content
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
		Usage:   &xunfeiResponse.Payload.Usage.Text,
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

	h.Usage.PromptTokens = xunfeiChatResponse.Payload.Usage.Text.PromptTokens
	h.Usage.CompletionTokens = xunfeiChatResponse.Payload.Usage.Text.CompletionTokens
	h.Usage.TotalTokens = xunfeiChatResponse.Payload.Usage.Text.TotalTokens

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
