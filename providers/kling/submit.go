package kling

import (
	"context"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/types"
)

func (s *KlingProvider) Submit(ctx context.Context, class, action string, request *KlingTask) (data *KlingResponse[KlingTaskData], errWithCode *types.OpenAIErrorWithStatusCode) {
	submitUri := fmt.Sprintf(s.Generations, class, action)

	fullRequestURL := s.GetFullRequestURL(submitUri, "")
	headers := s.GetRequestHeaders()

	// 创建请求
	req, err := s.Requester.NewRequest(http.MethodPost, fullRequestURL, s.Requester.WithContext(ctx), s.Requester.WithHeader(headers), s.Requester.WithBody(request))

	if err != nil {
		apiErr := common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
		apiErr.UpstreamNotAttempted = true
		return nil, apiErr
	}

	data = &KlingResponse[KlingTaskData]{}
	_, errWithCode = s.Requester.SendRequest(req, data, false)

	return data, errWithCode
}
