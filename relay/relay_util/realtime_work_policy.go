package relay_util

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"one-api/common"
	"one-api/middleware"
	"one-api/model"
	runtimesession "one-api/runtime/session"

	"github.com/gin-gonic/gin"
)

type realtimeWorkPolicy struct {
	mu             sync.Mutex
	c              *gin.Context
	billingContext *gin.Context
	models         runtimesession.ModelBinding
	resolver       func(string) (runtimesession.ModelBinding, error)
}

func NewRealtimeWorkPolicy(c *gin.Context, models runtimesession.ModelBinding, resolver func(string) (runtimesession.ModelBinding, error)) runtimesession.RealtimeWorkPolicy {
	return &realtimeWorkPolicy{c: copyRealtimeContext(c), billingContext: copyRealtimeContext(c), models: models, resolver: resolver}
}

func (p *realtimeWorkPolicy) ResolveModel(requested string) (runtimesession.ModelBinding, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = p.models.RequestedModel
	}
	models := p.models
	if p.resolver != nil {
		var err error
		models, err = p.resolver(requested)
		if err != nil {
			return runtimesession.ModelBinding{}, err
		}
	} else if requested != models.RequestedModel {
		return runtimesession.ModelBinding{}, errors.New("realtime model override is not supported")
	}
	if err := p.CheckFutureWork(models, false); err != nil {
		return runtimesession.ModelBinding{}, err
	}
	return models, nil
}

func (p *realtimeWorkPolicy) CheckFutureWork(models runtimesession.ModelBinding, countWork bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.c == nil {
		return errors.New("realtime billing context is required")
	}
	if strings.TrimSpace(models.RequestedModel) == "" || strings.TrimSpace(models.BillingModel) == "" {
		return errors.New("realtime requested and billing models are required")
	}
	if apiErr := middleware.RefreshLongLivedPrincipal(p.c); apiErr != nil {
		return apiErr
	}
	if err := middleware.EnsureTokenModelAllowed(p.c, models.RequestedModel); err != nil {
		return common.ErrorWrapperLocal(err, "permission_denied", http.StatusForbidden)
	}
	if err := middleware.EnsureLongLivedChannelAllowed(p.c, models.RequestedModel); err != nil {
		return common.ErrorWrapperLocal(err, "permission_denied", http.StatusForbidden)
	}
	ctx, cancel := BoundedBillingAdmissionContext(realtimeBillingContext(p.c))
	defer cancel()
	if _, err := model.CheckBillingAdmission(ctx, p.c.GetInt("id"), p.c.GetInt("token_id")); err != nil {
		return billingAdmissionError(err)
	}
	if countWork {
		if apiErr := middleware.AllowCurrentUserRequest(p.c); apiErr != nil {
			return apiErr
		}
	} else if apiErr := middleware.EnsureCurrentUserRequestAllowed(p.c); apiErr != nil {
		return apiErr
	}
	p.billingContext = copyRealtimeContext(p.c)
	return nil
}

// 每个 attempt 冻结本次准入读到的主体投影，后续 policy 刷新不改写已发生工作。
func (p *realtimeWorkPolicy) snapshot() *gin.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 拒绝未来工作时保留最近的计费归属，供已经观察到的供应商工作结算。
	return copyRealtimeContext(p.billingContext)
}

func copyRealtimeContext(c *gin.Context) *gin.Context {
	if c == nil {
		return nil
	}
	snapshot := c.Copy()
	if c.Request != nil {
		snapshot.Request = c.Request.Clone(context.WithoutCancel(c.Request.Context()))
	}
	return snapshot
}
