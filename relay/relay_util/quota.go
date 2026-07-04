package relay_util

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"one-api/common"
	"one-api/common/authutil"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/common/utils"
	"one-api/internal/billing"
	"one-api/model"
	"one-api/types"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

type Quota struct {
	modelName              string
	promptTokens           int
	price                  model.Price
	groupName              string
	tokenGroupName         string
	isBackupGroup          bool // 新增字段记录是否使用备用分组
	backupGroupName        string
	routingGroupSource     string
	groupRatio             float64
	inputRatio             float64
	outputRatio            float64
	preConsumedQuota       int
	cacheQuota             int
	userId                 int
	channelId              int
	tokenId                int
	callerNS               string
	unlimitedQuota         bool
	PreconsumeTruthApplied bool
	PreconsumeCacheApplied bool

	startTime         time.Time
	firstResponseTime time.Time
	requestDuration   time.Duration
	requestFrozen     bool
	extraBillingData  map[string]ExtraBillingData
	affinityMeta      map[string]any
	requestContext    context.Context
	tokenName         string
	sourceIP          string
	userAgent         string
	forcePreConsume   bool
	logProtocol       string
}

const (
	LogProtocolHTTP        = "http"
	LogProtocolHTTPStream  = "http_stream"
	LogProtocolRealtimeWS  = "realtime_ws"
	LogProtocolResponsesWS = "responses_ws"
)

func NewQuota(c *gin.Context, modelName string, promptTokens int) *Quota {
	isBackupGroup := c.GetBool("is_backupGroup")
	requestContext := context.Background()
	if c != nil && c.Request != nil {
		requestContext = detachQuotaContext(c.Request.Context())
	}

	quota := &Quota{
		modelName:      modelName,
		promptTokens:   promptTokens,
		userId:         c.GetInt("id"),
		channelId:      c.GetInt("channel_id"),
		tokenId:        c.GetInt("token_id"),
		callerNS:       readQuotaCallerNamespace(c),
		unlimitedQuota: c.GetBool("token_unlimited_quota"),
		isBackupGroup:  isBackupGroup, // 记录是否使用备用分组
		requestContext: requestContext,
		tokenName:      c.GetString("token_name"),
		sourceIP:       c.GetString(config.GinResponsesWSClientIPKey),
	}
	if quota.sourceIP == "" {
		quota.sourceIP = c.ClientIP()
	}
	if c.Request != nil {
		quota.userAgent = c.GetString(config.GinResponsesWSUserAgentKey)
		if quota.userAgent == "" {
			quota.userAgent = utils.NormalizeUserAgent(c.Request.UserAgent())
		}
	}
	if meta, ok := c.Get(config.GinChannelAffinityMetaKey); ok {
		if typed, ok := meta.(map[string]any); ok && len(typed) > 0 {
			quota.affinityMeta = make(map[string]any, len(typed))
			for key, value := range typed {
				quota.affinityMeta[key] = value
			}
		}
	}

	quota.price = *model.PricingInstance.GetPrice(quota.modelName)
	quota.groupName = groupctx.CurrentRoutingGroup(c)
	quota.tokenGroupName = groupctx.DeclaredTokenGroup(c)
	quota.backupGroupName = groupctx.BackupGroup(c)
	quota.routingGroupSource = groupctx.CurrentRoutingGroupSource(c)
	quota.groupRatio = c.GetFloat64("group_ratio") // 这里的倍率已经在 common.go 中正确设置了
	quota.inputRatio = quota.price.GetInput() * quota.groupRatio
	quota.outputRatio = quota.price.GetOutput() * quota.groupRatio

	return quota
}

func (q *Quota) ModelName() string {
	if q == nil {
		return ""
	}
	return q.modelName
}

func (q *Quota) PreConsumedQuota() int {
	if q == nil {
		return 0
	}
	return q.preConsumedQuota
}

func readQuotaCallerNamespace(c *gin.Context) string {
	if c != nil {
		if tokenID := c.GetInt("token_id"); tokenID > 0 {
			return "token:" + strconv.Itoa(tokenID)
		}
		if userID := c.GetInt("id"); userID > 0 {
			return "user:" + strconv.Itoa(userID)
		}
		if namespace := authutil.StableRequestCredentialNamespace(c.Request); namespace != "" {
			return namespace
		}
	}
	return "anonymous"
}

func (q *Quota) Clone() *Quota {
	if q == nil {
		return nil
	}

	cloned := *q
	cloned.price = cloneQuotaPrice(q.price)
	cloned.preConsumedQuota = 0
	cloned.cacheQuota = 0
	cloned.PreconsumeTruthApplied = false
	cloned.PreconsumeCacheApplied = false
	cloned.startTime = time.Time{}
	cloned.firstResponseTime = time.Time{}
	cloned.requestDuration = 0
	cloned.requestFrozen = false
	cloned.extraBillingData = nil
	cloned.requestContext = detachQuotaContext(q.requestContext)

	return &cloned
}

// Detached async tasks must hold quota synchronously because final settlement can
// be delayed or retried outside the submit request lifecycle.
func (q *Quota) ForcePreConsume() {
	if q == nil {
		return
	}
	q.forcePreConsume = true
}

func (q *Quota) PreQuotaConsumption() *types.OpenAIErrorWithStatusCode {
	return q.PreQuotaConsumptionRollbackable()
}

func (q *Quota) PreQuotaConsumptionRollbackable() *types.OpenAIErrorWithStatusCode {
	if q == nil {
		return common.StringErrorWrapperLocal("quota transaction is required", "quota_transaction_missing", http.StatusInternalServerError)
	}
	if q.HasPreConsumedSideEffect() {
		return nil
	}
	q.preparePreConsumedQuota()
	if q.preConsumedQuota == 0 {
		return nil
	}

	userQuota, err := model.CacheGetUserQuota(q.userId)
	if err != nil {
		return common.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota < q.preConsumedQuota {
		return common.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusPaymentRequired)
	}
	if !q.forcePreConsume && userQuota > 100*q.preConsumedQuota {
		q.preConsumedQuota = 0
		return nil
	}

	token, err := model.GetTokenById(q.tokenId)
	if err != nil {
		return common.ErrorWrapper(err, "pre_consume_token_quota_failed", http.StatusForbidden)
	}
	if !token.UnlimitedQuota && token.RemainQuota < q.preConsumedQuota {
		return common.ErrorWrapper(errors.New("令牌额度不足"), "pre_consume_token_quota_failed", http.StatusForbidden)
	}

	if err := model.ApplyTokenUserQuotaDeltaDirect(q.tokenId, q.userId, q.unlimitedQuota, q.preConsumedQuota); err != nil {
		return common.ErrorWrapper(err, "pre_consume_token_quota_failed", http.StatusForbidden)
	}
	q.PreconsumeTruthApplied = true

	if err := model.CacheDecreaseUserQuota(q.userId, q.preConsumedQuota); err != nil {
		rollbackErr := q.undoSynchronouslyWithContext(detachQuotaContext(q.requestContext))
		if rollbackErr != nil {
			return common.ErrorWrapper(errors.New(err.Error()+"; rollback failed: "+rollbackErr.Error()), "decrease_user_quota_failed", http.StatusInternalServerError)
		}
		return common.ErrorWrapper(err, "decrease_user_quota_failed", http.StatusInternalServerError)
	}
	q.PreconsumeCacheApplied = true
	return nil
}

func (q *Quota) HasPreConsumedSideEffect() bool {
	return q != nil && (q.PreconsumeTruthApplied || q.PreconsumeCacheApplied)
}

func (q *Quota) preparePreConsumedQuota() {
	if q == nil {
		return
	}
	q.preConsumedQuota = 0
	if q.price.Type == model.TimesPriceType {
		q.preConsumedQuota = int(1000 * q.inputRatio)
	} else if q.price.Input != 0 || q.price.Output != 0 {
		q.preConsumedQuota = int(float64(q.promptTokens)*q.inputRatio) + config.PreConsumedQuota
	}
}

// 更新用户实时配额
func (q *Quota) UpdateUserRealtimeQuota(usage *types.UsageEvent, nowUsage *types.UsageEvent) error {
	usage.Merge(nowUsage)

	if q.price.Type == model.TimesPriceType {
		return nil
	}

	// 不开启Redis，则不更新实时配额
	if !config.RedisEnabled {
		return q.checkNonRedisRealtimeHardCap(usage)
	}

	increaseQuota := q.GetTotalQuotaByUsageEvent(nowUsage)

	cacheQuota, err := model.CacheIncreaseUserRealtimeQuota(q.userId, increaseQuota)
	if err != nil {
		return errors.New("error update user realtime quota cache: " + err.Error())
	}

	q.cacheQuota += increaseQuota
	userQuota, err := model.CacheGetUserQuota(q.userId)
	if err != nil {
		return errors.New("error get user quota cache: " + err.Error())
	}

	if cacheQuota >= int64(userQuota) {
		return errors.New("user quota is not enough")
	}

	return nil
}

func (q *Quota) checkNonRedisRealtimeHardCap(usage *types.UsageEvent) error {
	if q == nil || q.unlimitedQuota || usage == nil || q.userId <= 0 {
		return nil
	}
	currentQuota := q.GetTotalQuotaByUsageEvent(usage)
	if currentQuota <= 0 {
		return nil
	}
	userQuota, err := model.GetUserQuota(q.userId)
	if err != nil {
		return err
	}
	if userQuota < currentQuota {
		return errors.New("user quota is not enough")
	}
	return nil
}

func (q *Quota) Undo(c *gin.Context) {
	if q == nil || !q.HasPreConsumedSideEffect() {
		return
	}
	// Roll back synchronously even though this keeps failure paths on the
	// request's critical path. The alternative is a short window where a retry
	// or resubmission can observe double pre-consumption before the goroutine
	// returns quota.
	ctx := q.rollbackContext(c)
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.LogError(ctx, fmt.Sprintf("panic returning pre-consumed quota: %v", recovered))
			logger.LogError(ctx, "stacktrace from panic: "+string(debug.Stack()))
		}
	}()
	if err := q.undoSynchronouslyWithContext(ctx); err != nil {
		logger.LogError(ctx, "error return pre-consumed quota: "+err.Error())
	}
}

func (q *Quota) UndoSynchronously(c *gin.Context) error {
	if q == nil {
		return nil
	}
	return q.undoSynchronouslyWithContext(q.rollbackContext(c))
}

func (q *Quota) undoSynchronouslyWithContext(ctx context.Context) error {
	if q == nil || !q.HasPreConsumedSideEffect() {
		return nil
	}
	if q.PreconsumeTruthApplied {
		if err := model.ApplyTokenUserQuotaDeltaDirect(q.tokenId, q.userId, q.unlimitedQuota, -q.preConsumedQuota); err != nil {
			return err
		}
	}
	q.PreconsumeTruthApplied = false
	if err := model.CacheUpdateUserQuota(q.userId); err != nil {
		logger.LogError(ctx, "error refresh user quota cache after rollback: "+err.Error())
		if delErr := model.CacheInvalidateUserQuota(q.userId); delErr != nil {
			model.EnqueueUserQuotaCacheRepair(q.userId, "rollback_refresh_and_invalidate_failed")
			return errors.New("error refresh user quota cache: " + err.Error() + "; error invalidate user quota cache: " + delErr.Error())
		}
	}
	q.PreconsumeCacheApplied = false
	return nil
}

func (q *Quota) rollbackContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return detachQuotaContext(c.Request.Context())
	}
	return detachQuotaContext(q.requestContext)
}

func (q *Quota) Consume(c *gin.Context, usage *types.Usage, isStream bool) {
	q.prepareConsumeContext(c)
	q.ConsumeUsage(usage, isStream)
}

func (q *Quota) ConsumeAtLeastPreConsumed(c *gin.Context, usage *types.Usage, isStream bool) {
	q.prepareConsumeContext(c)
	q.ConsumeUsageAtLeastPreConsumed(usage, isStream)
}

func (q *Quota) ConsumeAtLeastPreConsumedWithIdentity(c *gin.Context, usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) error {
	q.prepareConsumeContext(c)
	return q.ConsumeUsageAtLeastPreConsumedWithIdentity(usage, isStream, requestKind, identity, deduplicate)
}

func (q *Quota) ConsumeWithIdentity(c *gin.Context, usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) error {
	q.prepareConsumeContext(c)
	return q.ConsumeUsageWithIdentity(usage, isStream, requestKind, identity, deduplicate)
}

func (q *Quota) ConsumeFixedFinalQuotaWithIdentity(c *gin.Context, finalQuota int64, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) (int64, string, error) {
	return q.ConsumeFixedFinalQuotaWithUsageIdentity(c, finalQuota, nil, requestKind, identity, deduplicate)
}

func (q *Quota) ConsumeFixedFinalQuotaWithUsageIdentity(c *gin.Context, finalQuota int64, usage *types.Usage, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) (int64, string, error) {
	q.prepareConsumeContext(c)
	appliedQuota, err := q.consumeFixedFinalQuotaSettlement(finalQuota, usage, true, requestKind, identity, deduplicate)
	return appliedQuota, identity, err
}

func (q *Quota) prepareConsumeContext(c *gin.Context) {
	if q == nil {
		return
	}
	if c != nil {
		if q.requestContext == nil && c.Request != nil {
			q.requestContext = detachQuotaContext(c.Request.Context())
		}
		if q.tokenName == "" {
			q.tokenName = c.GetString("token_name")
		}
		if q.sourceIP == "" {
			q.sourceIP = c.GetString(config.GinResponsesWSClientIPKey)
			if q.sourceIP == "" {
				q.sourceIP = c.ClientIP()
			}
		}
		if q.userAgent == "" && c.Request != nil {
			q.userAgent = c.GetString(config.GinResponsesWSUserAgentKey)
			if q.userAgent == "" {
				q.userAgent = utils.NormalizeUserAgent(c.Request.UserAgent())
			}
		}
		if q.startTime.IsZero() {
			q.startTime = c.GetTime("requestStartTime")
		}
	}
}

func (q *Quota) ConsumeUsage(usage *types.Usage, isStream bool) {
	if err := q.consumeUsageSettlement(usage, isStream, billing.SettlementRequestKindUnary, "", false); err != nil {
		logger.LogError(q.requestContext, err.Error())
	}
}

func (q *Quota) ConsumeUsageAtLeastPreConsumed(usage *types.Usage, isStream bool) {
	if q == nil {
		return
	}
	if err := q.consumeUsageSettlementWithFinalQuotaFloor(usage, isStream, billing.SettlementRequestKindUnary, "", false, q.preConsumedQuota); err != nil {
		logger.LogError(q.requestContext, err.Error())
	}
}

func (q *Quota) ConsumeUsageAtLeastPreConsumedWithIdentity(usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) error {
	if q == nil {
		return nil
	}
	return q.consumeUsageSettlementWithFinalQuotaFloor(usage, isStream, requestKind, identity, deduplicate, q.preConsumedQuota)
}

func (q *Quota) ConsumeUsageWithIdentity(usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) error {
	return q.consumeUsageSettlement(usage, isStream, requestKind, identity, deduplicate)
}

func (q *Quota) BuildSettlementEnvelope(usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) *billing.SettlementEnvelope {
	return q.buildSettlementEnvelopeWithFinalQuotaFloor(usage, isStream, requestKind, identity, deduplicate, 0)
}

func (q *Quota) buildSettlementEnvelopeWithFixedFinalQuota(finalQuota int, usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) *billing.SettlementEnvelope {
	envelope := q.buildSettlementEnvelopeWithFinalQuotaFloor(usage, isStream, requestKind, identity, deduplicate, 0)
	if envelope == nil {
		return nil
	}
	envelope.Command.FinalQuota = finalQuota
	return envelope
}

func (q *Quota) buildSettlementEnvelopeWithFinalQuotaFloor(usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool, finalQuotaFloor int) *billing.SettlementEnvelope {
	if q == nil {
		return nil
	}

	if usage == nil {
		usage = &types.Usage{}
	}

	finalQuota := q.GetTotalQuotaByUsage(usage)
	if finalQuota < finalQuotaFloor {
		finalQuota = finalQuotaFloor
	}
	return &billing.SettlementEnvelope{
		Command: billing.SettlementCommand{
			Identity:         identity,
			RequestKind:      requestKind,
			UserID:           q.userId,
			TokenID:          q.tokenId,
			ChannelID:        q.channelId,
			ModelName:        q.modelName,
			PreConsumedQuota: q.preConsumedQuota,
			FinalQuota:       finalQuota,
			UsageSummary:     billing.NewUsageSummary(usage),
			UnlimitedQuota:   q.unlimitedQuota,
		},
		Options: billing.SettlementOptions{
			Deduplicate: deduplicate,
			Cleanup: billing.SettlementCleanup{
				RealtimeQuotaDelta:    q.cacheQuota,
				RefreshUserQuotaCache: config.RedisEnabled,
			},
			Projection: billing.SettlementProjection{
				TokenName:   q.tokenName,
				RequestTime: q.getRequestTime(),
				IsStream:    isStream,
				Metadata:    q.getLogMetaForSettlement(usage, isStream),
				SourceIP:    q.sourceIP,
			},
		},
	}
}

func (q *Quota) consumeUsageSettlement(usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) error {
	return q.consumeUsageSettlementWithFinalQuotaFloor(usage, isStream, requestKind, identity, deduplicate, 0)
}

func (q *Quota) consumeUsageSettlementWithFinalQuotaFloor(usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool, finalQuotaFloor int) error {
	if q == nil {
		return nil
	}

	q.requestContext = detachQuotaContext(q.requestContext)
	if q.startTime.IsZero() {
		q.startTime = time.Now()
	}

	envelope := q.buildSettlementEnvelopeWithFinalQuotaFloor(usage, isStream, requestKind, identity, deduplicate, finalQuotaFloor)
	if envelope == nil {
		return nil
	}

	result, err := billing.ApplySettlement(q.requestContext, envelope.Command, &envelope.Options)
	if err != nil {
		// Settlement is called after the provider-side request is considered
		// admitted. If applying the final delta fails, keep any pre-consumed
		// truth quota instead of refunding completed upstream work. Explicit
		// no-send paths still use Undo/RollbackBeforeLocalWriteOK before this
		// point, where provider absence is provable.
		q.reconcileRealtimeQuotaCache()
		return errors.New("error applying settlement: " + err.Error())
	}
	if result.Deduplicated {
		return nil
	}
	if result.TruthApplied {
		if q.cacheQuota > 0 && !result.CleanupFailed {
			q.cacheQuota = 0
		}
		q.PreconsumeTruthApplied = false
		q.PreconsumeCacheApplied = false
		return nil
	}
	q.reconcileRealtimeQuotaCache()
	return nil
}

func (q *Quota) consumeFixedFinalQuotaSettlement(finalQuota int64, usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind, identity string, deduplicate bool) (int64, error) {
	if q == nil {
		return 0, nil
	}
	if finalQuota < 0 {
		return 0, errors.New("fixed final quota cannot be negative")
	}
	maxInt := int64(int(^uint(0) >> 1))
	if finalQuota > maxInt {
		return 0, errors.New("fixed final quota exceeds platform int range")
	}

	q.requestContext = detachQuotaContext(q.requestContext)
	if q.startTime.IsZero() {
		q.startTime = time.Now()
	}

	envelope := q.buildSettlementEnvelopeWithFixedFinalQuota(int(finalQuota), usage, isStream, requestKind, identity, deduplicate)
	if envelope == nil {
		return 0, nil
	}

	result, err := billing.ApplySettlement(q.requestContext, envelope.Command, &envelope.Options)
	if err != nil {
		q.reconcileRealtimeQuotaCache()
		return 0, errors.New("error applying settlement: " + err.Error())
	}
	if result.Deduplicated {
		if result.FingerprintConflict {
			return 0, errors.New("settlement identity fingerprint conflict")
		}
		return int64(envelope.Command.FinalQuota), nil
	}
	if result.TruthApplied {
		if q.cacheQuota > 0 && !result.CleanupFailed {
			q.cacheQuota = 0
		}
		q.PreconsumeTruthApplied = false
		q.PreconsumeCacheApplied = false
		return int64(envelope.Command.FinalQuota), nil
	}
	q.reconcileRealtimeQuotaCache()
	return int64(envelope.Command.FinalQuota), nil
}

func detachQuotaContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func (q *Quota) reconcileRealtimeQuotaCache() {
	if q == nil || q.cacheQuota <= 0 {
		return
	}

	if _, err := model.CacheDecreaseUserRealtimeQuota(q.userId, q.cacheQuota); err != nil {
		logger.LogError(q.requestContext, "error reconcile realtime quota cache: "+err.Error())
		model.EnqueueUserRealtimeQuotaCacheDecreaseRepair(q.userId, q.cacheQuota, "realtime_quota_reconcile_failed")
		return
	}
	q.cacheQuota = 0
}

func cloneQuotaPrice(price model.Price) model.Price {
	cloned := price
	if price.ExtraRatios != nil {
		raw := price.ExtraRatios.Data()
		copied := make(map[string]float64, len(raw))
		for key, value := range raw {
			copied[key] = value
		}
		extraRatios := datatypes.NewJSONType(copied)
		cloned.ExtraRatios = &extraRatios
	}
	if price.ModelInfo != nil {
		modelInfo := *price.ModelInfo
		modelInfo.InputModalities = append([]string(nil), price.ModelInfo.InputModalities...)
		modelInfo.OutputModalities = append([]string(nil), price.ModelInfo.OutputModalities...)
		modelInfo.Tags = append([]string(nil), price.ModelInfo.Tags...)
		modelInfo.SupportUrl = append([]string(nil), price.ModelInfo.SupportUrl...)
		cloned.ModelInfo = &modelInfo
	}
	return cloned
}

func (q *Quota) GetInputRatio() float64 {
	return q.inputRatio
}

func (q *Quota) SetLogProtocol(protocol string) {
	if q == nil {
		return
	}
	q.logProtocol = strings.TrimSpace(protocol)
}

func logProtocolForStream(isStream bool) string {
	if isStream {
		return LogProtocolHTTPStream
	}
	return LogProtocolHTTP
}

func (q *Quota) GetLogMeta(usage *types.Usage) map[string]any {
	meta := map[string]any{
		"group_name":           q.groupName,
		"using_group":          q.groupName,
		"token_group":          q.tokenGroupName,
		"backup_group_name":    q.backupGroupName,
		"routing_group_source": q.routingGroupSource,
		"is_backup_group":      q.isBackupGroup, // 添加是否使用备用分组的标识
		"price_type":           q.price.Type,
		"group_ratio":          q.groupRatio,
		"input_ratio":          q.price.GetInput(),
		"output_ratio":         q.price.GetOutput(),
	}

	if protocol := strings.TrimSpace(q.logProtocol); protocol != "" {
		meta["protocol"] = protocol
	}

	firstResponseTime := q.GetFirstResponseTime()
	if firstResponseTime > 0 {
		meta["first_response"] = firstResponseTime
	}

	if usage != nil {
		extraTokens := usage.GetExtraTokens()

		for key, value := range extraTokens {
			meta[key] = value
			extraRatio := q.price.GetExtraRatio(key)
			meta[key+"_ratio"] = extraRatio
		}
	}

	if q.extraBillingData != nil {
		meta["extra_billing"] = q.extraBillingData
	}
	if len(q.affinityMeta) > 0 {
		for key, value := range q.affinityMeta {
			meta[key] = value
		}
	}

	// Display-only trade-off: keep user-agent in JSON metadata until query use cases justify a dedicated column.
	meta = utils.AppendUserAgentMetadata(meta, q.userAgent)
	return meta
}

func (q *Quota) getLogMetaForSettlement(usage *types.Usage, isStream bool) map[string]any {
	meta := q.GetLogMeta(usage)
	if _, ok := meta["protocol"]; !ok {
		meta["protocol"] = logProtocolForStream(isStream)
	}
	return meta
}

func (q *Quota) getRequestTime() int {
	if q.requestFrozen {
		if q.requestDuration < 0 {
			return 0
		}
		return int(q.requestDuration.Milliseconds())
	}
	if q.startTime.IsZero() {
		return 0
	}
	return int(time.Since(q.startTime).Milliseconds())
}

// 通过 token 数获取消费配额
func (q *Quota) GetTotalQuota(promptTokens, completionTokens int, extraBilling map[string]types.ExtraBilling) (quota int) {
	if q.price.Type == model.TimesPriceType {
		quota = int(1000 * q.inputRatio)
	} else {
		quota = int(math.Ceil((float64(promptTokens) * q.inputRatio) + (float64(completionTokens) * q.outputRatio)))
	}

	q.GetExtraBillingData(extraBilling)
	extraBillingQuota := 0
	if q.extraBillingData != nil {
		for _, value := range q.extraBillingData {
			extraBillingQuota += int(math.Ceil(
				float64(value.Price)*float64(config.QuotaPerUnit),
			)) * value.CallCount
		}
	}

	if extraBillingQuota > 0 {
		quota += int(math.Ceil(
			float64(extraBillingQuota) * q.groupRatio,
		))
	}

	if q.inputRatio != 0 && quota <= 0 {
		quota = 1
	}
	totalTokens := promptTokens + completionTokens
	if totalTokens == 0 {
		// in this case, must be some error happened
		// we cannot just return, because we may have to return the pre-consumed quota
		quota = 0
	}

	return quota
}

// 获取计算的 token 数
func (q *Quota) getComputeTokensByUsage(usage *types.Usage) (promptTokens, completionTokens int) {
	promptTokens = usage.PromptTokens
	completionTokens = usage.CompletionTokens

	extraTokens := usage.GetExtraTokens()

	for key, value := range extraTokens {
		extraRatio := q.price.GetExtraRatio(key)
		if model.GetExtraPriceIsPrompt(key) {
			promptTokens += model.GetIncreaseTokens(value, extraRatio)
		} else {
			completionTokens += model.GetIncreaseTokens(value, extraRatio)
		}
	}

	return
}

func (q *Quota) getComputeTokensByUsageEvent(usage *types.UsageEvent) (promptTokens, completionTokens int) {
	promptTokens = usage.InputTokens
	completionTokens = usage.OutputTokens
	extraTokens := usage.GetExtraTokens()

	for key, value := range extraTokens {
		extraRatio := q.price.GetExtraRatio(key)
		if model.GetExtraPriceIsPrompt(key) {
			promptTokens += model.GetIncreaseTokens(value, extraRatio)
		} else {
			completionTokens += model.GetIncreaseTokens(value, extraRatio)
		}
	}

	return
}

// 通过 usage 获取消费配额
func (q *Quota) GetTotalQuotaByUsage(usage *types.Usage) (quota int) {
	if usage == nil {
		return q.GetTotalQuota(0, 0, nil)
	}
	promptTokens, completionTokens := q.getComputeTokensByUsage(usage)
	return q.GetTotalQuota(promptTokens, completionTokens, usage.ExtraBilling)
}

func (q *Quota) GetTotalQuotaByUsageEvent(usage *types.UsageEvent) (quota int) {
	if usage == nil {
		return q.GetTotalQuota(0, 0, nil)
	}
	promptTokens, completionTokens := q.getComputeTokensByUsageEvent(usage)
	return q.GetTotalQuota(promptTokens, completionTokens, usage.ExtraBilling)
}

func (q *Quota) GetFirstResponseTime() int64 {
	if q.startTime.IsZero() || q.firstResponseTime.IsZero() || q.firstResponseTime.Before(q.startTime) {
		return 0
	}

	return q.firstResponseTime.Sub(q.startTime).Milliseconds()
}

func (q *Quota) SeedTiming(startedAt, firstResponseAt, completedAt time.Time) {
	if q == nil {
		return
	}
	effectiveStart := q.startTime
	if !startedAt.IsZero() {
		q.startTime = startedAt
		effectiveStart = startedAt
	}
	if !firstResponseAt.IsZero() && (effectiveStart.IsZero() || !firstResponseAt.Before(effectiveStart)) {
		q.firstResponseTime = firstResponseAt
	}
	if !effectiveStart.IsZero() && !completedAt.IsZero() {
		duration := completedAt.Sub(effectiveStart)
		if duration < 0 {
			duration = 0
		}
		q.requestDuration = duration
		q.requestFrozen = true
	}
}

func (q *Quota) SetFirstResponseTime(firstResponseTime time.Time) {
	q.firstResponseTime = firstResponseTime
}

type ExtraBillingData struct {
	ServiceType string  `json:"service_type,omitempty"`
	Type        string  `json:"type"`
	CallCount   int     `json:"call_count"`
	Price       float64 `json:"price"`
}

func (q *Quota) GetExtraBillingData(extraBilling map[string]types.ExtraBilling) {
	if len(extraBilling) == 0 {
		q.extraBillingData = nil
		return
	}

	extraBillingData := make(map[string]ExtraBillingData)
	for billingKey, value := range extraBilling {
		serviceType := types.ResolveExtraBillingServiceType(billingKey, value)
		billingType := types.ResolveExtraBillingType(billingKey, value)
		extraBillingData[billingKey] = ExtraBillingData{
			ServiceType: serviceType,
			Type:        billingType,
			CallCount:   value.CallCount,
			Price:       getDefaultExtraServicePrice(serviceType, q.modelName, billingType),
		}
	}

	if len(extraBillingData) == 0 {
		q.extraBillingData = nil
		return
	}

	q.extraBillingData = extraBillingData
}
