package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"one-api/common/providerresponse"
)

// GetModelList 只返回本次上游发现结果，不修改渠道模型配置或发布静态目录。
func (p *AntigravityProvider) GetModelList() ([]string, error) {
	ctx, cancel := context.WithTimeout(p.LogContext(), 30*time.Second)
	defer cancel()
	if err := p.prepareCredentials(ctx); err != nil {
		return nil, err
	}
	if p.Requester == nil {
		return nil, errors.New("Antigravity 模型发现缺少 HTTP requester")
	}
	req, err := p.Requester.NewRequest(http.MethodPost, p.upstreamURL("fetchAvailableModels"),
		p.Requester.WithContext(ctx),
		p.Requester.WithHeader(p.GetRequestHeaders()),
		p.Requester.WithBody(map[string]string{"project": p.credentials.ProjectID}),
	)
	if err != nil {
		return nil, fmt.Errorf("无法构造 Antigravity 模型发现请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, apiErr := p.Requester.SendRequestRawCheckedNoRedirect(req)
	if apiErr != nil {
		return nil, providerresponse.SanitizeAPIError(apiErr, p.credentialValues()...)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, controlBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("无法读取 Antigravity 模型目录: %w", err)
	}
	if len(body) > controlBodyLimit {
		return nil, errors.New("Antigravity 模型目录超过 4 MiB 限制")
	}
	var catalog struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil || catalog.Models == nil {
		return nil, errors.New("Antigravity 模型目录缺少有效的 models 对象")
	}
	models := make([]string, 0, len(catalog.Models))
	for id := range catalog.Models {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("Antigravity 模型目录包含空模型 ID")
		}
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}
