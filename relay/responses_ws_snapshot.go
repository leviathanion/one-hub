package relay

import (
	"context"
	"net/http"
	"one-api/common/config"
	"one-api/common/utils"

	"github.com/gin-gonic/gin"
)

type ResponsesWSRequestSnapshot struct {
	Request   *http.Request
	Values    map[string]any
	ClientIP  string
	UserAgent string
}

func NewResponsesWSRequestSnapshot(c *gin.Context) *ResponsesWSRequestSnapshot {
	snapshot := &ResponsesWSRequestSnapshot{}
	snapshot.RefreshFromContext(c)
	return snapshot
}

func (s *ResponsesWSRequestSnapshot) Clone() *ResponsesWSRequestSnapshot {
	if s == nil {
		return nil
	}
	return &ResponsesWSRequestSnapshot{
		Request:   cloneResponsesWSRequest(s.Request),
		Values:    cloneResponsesWSContextValues(s.Values),
		ClientIP:  s.ClientIP,
		UserAgent: s.UserAgent,
	}
}

func (s *ResponsesWSRequestSnapshot) RefreshFromContext(c *gin.Context) {
	if s == nil || c == nil {
		return
	}
	copied := c.Copy()
	s.Request = cloneResponsesWSRequest(copied.Request)
	s.Values = cloneResponsesWSContextValues(copied.Keys)
	if clientIP := copied.GetString(config.GinResponsesWSClientIPKey); clientIP != "" {
		s.ClientIP = clientIP
	} else if copied.Request != nil {
		s.ClientIP = c.ClientIP()
	}
	if userAgent := copied.GetString(config.GinResponsesWSUserAgentKey); userAgent != "" {
		s.UserAgent = userAgent
	} else if copied.Request != nil {
		s.UserAgent = utils.NormalizeUserAgent(copied.Request.UserAgent())
	}
	s.Values[config.GinResponsesWSClientIPKey] = s.ClientIP
	s.Values[config.GinResponsesWSUserAgentKey] = s.UserAgent
}

func (s *ResponsesWSRequestSnapshot) Context() *gin.Context {
	if s == nil {
		return nil
	}
	values := cloneResponsesWSContextValues(s.Values)
	values[config.GinResponsesWSClientIPKey] = s.ClientIP
	values[config.GinResponsesWSUserAgentKey] = s.UserAgent
	return &gin.Context{
		Request: cloneResponsesWSRequest(s.Request),
		Keys:    values,
	}
}

func (s *ResponsesWSRequestSnapshot) Set(key string, value any) {
	if s == nil || key == "" {
		return
	}
	if s.Values == nil {
		s.Values = map[string]any{}
	}
	s.Values[key] = cloneResponsesWSContextValue(value)
}

func (s *ResponsesWSRequestSnapshot) Get(key string) (any, bool) {
	if s == nil || s.Values == nil {
		return nil, false
	}
	value, ok := s.Values[key]
	return value, ok
}

func (s *ResponsesWSRequestSnapshot) GetString(key string) string {
	if s == nil {
		return ""
	}
	value, ok := s.Get(key)
	if !ok {
		return ""
	}
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

func (s *ResponsesWSRequestSnapshot) Delete(keys ...string) {
	if s == nil || s.Values == nil {
		return
	}
	for _, key := range keys {
		delete(s.Values, key)
	}
}

func cloneResponsesWSRequest(req *http.Request) *http.Request {
	if req == nil {
		return nil
	}
	reqCtx := req.Context()
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	cloned := req.Clone(context.WithoutCancel(reqCtx))
	cloned.Header = req.Header.Clone()
	return cloned
}

func cloneResponsesWSContextValues(values map[string]any) map[string]any {
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = cloneResponsesWSContextValue(value)
	}
	return cloned
}

func cloneResponsesWSContextValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, value := range typed {
			copied[key] = value
		}
		return copied
	case []int:
		return append([]int(nil), typed...)
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}
