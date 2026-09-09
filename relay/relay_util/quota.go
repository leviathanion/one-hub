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
	"one-api/common/utils"
	"one-api/internal/billing"
	"one-api/model"
	"one-api/types"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

type Quota struct {
	modelName                    string
	promptTokens                 int
	price                        model.Price
	groupName                    string
	tokenGroupName               string
	isBackupGroup                bool // 新增字段记录是否使用备用分组
	backupGroupName              string
	routingGroupSource           string
	groupRatio                   float64
	inputRatio                   float64
	outputRatio                  float64
	freezeRequestPricePolicy     bool
	preConsumedQuota             int
	userId                       int
	channelId                    int
	tokenId                      int
	callerNS                     string
	PreconsumeTruthApplied       bool
	PreconsumeTruthIndeterminate bool
	preconsumeTokenApplied       bool

	startTime                  time.Time
	firstResponseTime          time.Time
	requestDuration            time.Duration
	requestFrozen              bool
	extraBillingData           map[string]ExtraBillingData
	affinityMeta               map[string]any
	billingDiagnostics         map[string]bool
	settlementModel            string
	settlementPrice            *model.Price
	settlementPriceVersion     int64
	settlementGroupRatio       float64
	settlementPolicyResolved   bool
	settlementTier             string
	settlementSpeed            string
	settlementRuleResult       model.PriceRuleResult
	settlementInputMultiplier  float64
	settlementOutputMultiplier float64
	settlementInputRatio       float64
	settlementOutputRatio      float64
	settlementTokenBilling     *tokenBillingDetails
	requestContext             context.Context
	tokenName                  string
	sourceIP                   string
	userAgent                  string
	logProtocol                string
}

var (
	applyBillingReserve    = model.ApplyBillingReserve
	checkBillingAdmission  = model.CheckBillingAdmission
	applyBillingRefund     = model.ApplyBillingRefund
	applyBillingSettlement = billing.ApplySettlement
)

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
		modelName:                modelName,
		promptTokens:             promptTokens,
		userId:                   c.GetInt("id"),
		channelId:                c.GetInt("channel_id"),
		tokenId:                  c.GetInt("token_id"),
		callerNS:                 readQuotaCallerNamespace(c),
		isBackupGroup:            isBackupGroup, // 记录是否使用备用分组
		freezeRequestPricePolicy: c.GetBool("billing_original_model"),
		requestContext:           requestContext,
		tokenName:                c.GetString("token_name"),
		sourceIP:                 c.GetString(config.GinResponsesWSClientIPKey),
		startTime:                c.GetTime("requestStartTime"),
	}
	if quota.startTime.IsZero() {
		quota.startTime = time.Now()
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

	quota.groupName = groupctx.CurrentRoutingGroup(c)
	quota.tokenGroupName = groupctx.DeclaredTokenGroup(c)
	quota.backupGroupName = groupctx.BackupGroup(c)
	quota.routingGroupSource = groupctx.CurrentRoutingGroupSource(c)
	quota.groupRatio = c.GetFloat64("group_ratio") // 这里的倍率已经在 common.go 中正确设置了

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

// ReservationQuota returns the established small pre-consume amount without
// applying it. Durable owners use it to reserve quota and create their owner
// row in one SQL transaction.
func (q *Quota) ReservationQuota() (int, error) {
	if q == nil {
		return 0, errors.New("quota is required")
	}
	if err := q.preparePreConsumedQuota(); err != nil {
		return 0, err
	}
	return q.preConsumedQuota, nil
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

func (q *Quota) preQuotaConsumptionWithContext(ctx context.Context) *types.OpenAIErrorWithStatusCode {
	if q == nil {
		return common.StringErrorWrapperLocal("quota transaction is required", "quota_transaction_missing", http.StatusInternalServerError)
	}
	if ctx == nil {
		ctx = q.requestContext
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if q.PreconsumeTruthIndeterminate {
		return common.StringErrorWrapperLocal("pre-consumption commit outcome is indeterminate", "pre_consume_commit_indeterminate", http.StatusInternalServerError)
	}
	if q.HasPreConsumedSideEffect() {
		return nil
	}
	var (
		applyResult model.BillingBalanceResult
		err         error
	)
	if priceErr := q.preparePreConsumedQuota(); priceErr != nil {
		return common.ErrorWrapperLocal(priceErr, "price_policy_unavailable", http.StatusServiceUnavailable)
	}
	if q.preConsumedQuota > 0 {
		applyResult, err = applyBillingReserve(ctx, q.userId, q.tokenId, int64(q.preConsumedQuota))
	} else {
		applyResult, err = checkBillingAdmission(ctx, q.userId, q.tokenId)
	}
	if err != nil {
		if applyResult.Outcome == model.BillingBalanceCommitUnknown {
			q.PreconsumeTruthIndeterminate = true
			return common.ErrorWrapper(err, "pre_consume_commit_indeterminate", http.StatusInternalServerError)
		}
		return billingAdmissionError(err)
	}
	q.PreconsumeTruthApplied = q.preConsumedQuota > 0
	q.preconsumeTokenApplied = applyResult.TokenQuotaApplied
	return nil
}

func billingAdmissionError(err error) *types.OpenAIErrorWithStatusCode {
	switch {
	case errors.Is(err, model.ErrBillingUserQuotaInsufficient):
		return common.ErrorWrapperLocal(err, "insufficient_user_quota", http.StatusPaymentRequired)
	case errors.Is(err, model.ErrTokenQuotaInsufficient):
		return common.ErrorWrapperLocal(err, "insufficient_token_quota", http.StatusForbidden)
	case errors.Is(err, model.ErrBillingUserUnavailable), errors.Is(err, model.ErrBillingTokenUnavailable), errors.Is(err, model.ErrBillingOwnership):
		return common.ErrorWrapperLocal(err, "billing_principal_unavailable", http.StatusForbidden)
	default:
		return common.ErrorWrapperLocal(err, "billing_admission_unavailable", http.StatusServiceUnavailable)
	}
}

func (q *Quota) HasPreConsumedSideEffect() bool {
	return q != nil && (q.PreconsumeTruthApplied || q.PreconsumeTruthIndeterminate)
}

func (q *Quota) preparePreConsumedQuota() error {
	if q == nil {
		return errors.New("quota is required")
	}
	if err := q.refreshCurrentRequestPrice(); err != nil {
		return err
	}
	q.preConsumedQuota = 0
	if q.price.Type == model.TimesPriceType {
		q.preConsumedQuota = saturatingTruncToInt(1000 * q.inputRatio)
	} else if q.groupRatio > 0 && (q.price.Input != 0 || q.price.Output != 0) {
		q.preConsumedQuota = saturatingAddInt(saturatingTruncToInt(float64(q.promptTokens)*q.inputRatio), q.effectivePreConsumedQuotaBase())
	}
	if q.preConsumedQuota < 0 {
		return errors.New("pre-consumed quota cannot be negative")
	}
	return nil
}

func (q *Quota) effectivePreConsumedQuotaBase() int {
	return config.GlobalOption.RuntimeSnapshot().Int("PreConsumedQuota", config.PreConsumedQuota)
}

func (q *Quota) effectiveQuotaPerUnit() float64 {
	return config.GlobalOption.RuntimeSnapshot().Float64("QuotaPerUnit", config.QuotaPerUnit)
}

func (q *Quota) refreshCurrentRequestPrice() error {
	if q == nil || model.PricingInstance == nil {
		return errors.New("prices are not initialized")
	}
	price, ok := model.PricingInstance.FindPrice(q.modelName)
	if !ok {
		return fmt.Errorf("no price policy matches model %q", q.modelName)
	}
	q.price = cloneQuotaPrice(*price)
	groupRatio, err := q.currentGroupRatio()
	if err != nil {
		return err
	}
	q.groupRatio = groupRatio
	q.inputRatio = q.price.GetInput() * groupRatio
	q.outputRatio = q.price.GetOutput() * groupRatio
	return nil
}

func (q *Quota) currentGroupRatio() (float64, error) {
	if q == nil {
		return 0, errors.New("quota is required")
	}
	if strings.TrimSpace(q.groupName) == "" {
		return 0, errors.New("billing group is required")
	}
	group := model.GlobalUserGroupRatio.GetBySymbol(q.groupName)
	if group == nil || math.IsNaN(group.Ratio) || math.IsInf(group.Ratio, 0) || group.Ratio < 0 {
		return 0, fmt.Errorf("billing group %q is unavailable", q.groupName)
	}
	return group.Ratio, nil
}

func (q *Quota) undoSynchronouslyWithContext(ctx context.Context) error {
	if q == nil || !q.HasPreConsumedSideEffect() {
		return nil
	}
	if ctx == nil {
		ctx = q.requestContext
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if q.PreconsumeTruthIndeterminate {
		return errors.New("pre-consumption commit outcome is indeterminate; automatic rollback is unsafe")
	}
	if q.PreconsumeTruthApplied {
		var lastErr error
		for attempt := 0; attempt < 2; attempt++ {
			result, err := applyBillingRefund(ctx, q.userId, q.tokenId, q.preconsumeTokenApplied, int64(q.preConsumedQuota))
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = err
			if result.Outcome == model.BillingBalanceCommitUnknown {
				q.PreconsumeTruthApplied = false
				q.PreconsumeTruthIndeterminate = true
				return fmt.Errorf("billing rollback commit outcome is unknown: %w", err)
			}
			if result.Outcome != model.BillingBalanceDefinitelyRolledBack || ctx.Err() != nil {
				return err
			}
		}
		if lastErr != nil {
			return fmt.Errorf("billing rollback remained unsettled: %w", lastErr)
		}
		q.PreconsumeTruthApplied = false
		q.preconsumeTokenApplied = false
	}
	return nil
}

func (q *Quota) buildSettlementEnvelope(finalQuota int, usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind) *billing.SettlementEnvelope {
	if q == nil {
		return nil
	}

	if usage == nil {
		usage = &types.Usage{}
	}

	return &billing.SettlementEnvelope{
		Command: billing.SettlementCommand{
			RequestKind:            requestKind,
			UserID:                 q.userId,
			TokenID:                q.tokenId,
			ChannelID:              q.channelId,
			ModelName:              q.modelName,
			PreConsumedQuota:       q.preConsumedQuota,
			FinalQuota:             finalQuota,
			UsageSummary:           billing.NewUsageSummary(usage),
			PreconsumeTokenApplied: q.preconsumeTokenApplied,
		},
		Options: billing.SettlementOptions{
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

func (q *Quota) consumeFinalQuota(operationCtx context.Context, finalQuota int64, usage *types.Usage, isStream bool, requestKind billing.SettlementRequestKind) (int64, error) {
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

	if operationCtx == nil {
		operationCtx = detachQuotaContext(q.requestContext)
	}
	envelope := q.buildSettlementEnvelope(int(finalQuota), usage, isStream, requestKind)
	if envelope == nil {
		return 0, nil
	}

	var (
		result billing.SettlementResult
		err    error
	)
	for attempt := 0; attempt < 2; attempt++ {
		result, err = applyBillingSettlement(operationCtx, envelope.Command, &envelope.Options)
		if err == nil {
			break
		}
		if result.BalanceOutcome == model.BillingBalanceCommitUnknown {
			break
		}
		if result.BalanceOutcome != model.BillingBalanceDefinitelyRolledBack || operationCtx.Err() != nil {
			break
		}
	}
	if err != nil {
		if result.BalanceOutcome == model.BillingBalanceCommitUnknown {
			return 0, fmt.Errorf("billing settlement commit outcome is unknown: %w", err)
		}
		if operationCtx.Err() != nil {
			return 0, fmt.Errorf("billing settlement deadline exhausted: %w", errors.Join(err, operationCtx.Err()))
		}
		return 0, fmt.Errorf("error applying settlement: %w", err)
	}
	if result.TruthApplied {
		q.PreconsumeTruthApplied = false
		q.PreconsumeTruthIndeterminate = false
		return int64(envelope.Command.FinalQuota), nil
	}
	return int64(envelope.Command.FinalQuota), nil
}

func detachQuotaContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
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
	if price.RateRules != nil {
		rateRules := datatypes.NewJSONType(model.ClonePriceRateRules(price.RateRules.Data()))
		cloned.RateRules = &rateRules
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
	price := &q.price
	if q.settlementPrice != nil {
		price = q.settlementPrice
	}
	inputRatio := price.GetInput()
	outputRatio := price.GetOutput()
	if q.settlementPolicyResolved {
		inputRatio = q.settlementInputRatio
		outputRatio = q.settlementOutputRatio
	}
	groupRatio := q.groupRatio
	if q.settlementPolicyResolved {
		groupRatio = q.settlementGroupRatio
	}
	meta := map[string]any{
		"group_name":           q.groupName,
		"using_group":          q.groupName,
		"token_group":          q.tokenGroupName,
		"backup_group_name":    q.backupGroupName,
		"routing_group_source": q.routingGroupSource,
		"is_backup_group":      q.isBackupGroup, // 添加是否使用备用分组的标识
		"price_type":           price.Type,
		"group_ratio":          groupRatio,
		"input_ratio":          inputRatio,
		"output_ratio":         outputRatio,
	}
	for _, key := range []string{"input_ratio", "output_ratio"} {
		if value, ok := meta[key].(float64); ok && (math.IsNaN(value) || math.IsInf(value, 0)) {
			delete(meta, key)
			q.addBillingDiagnostic("token_billing_rates_unrepresentable")
		}
	}
	if q.settlementPriceVersion > 0 {
		meta["price_version"] = q.settlementPriceVersion
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
			extraRatio := price.GetExtraRatio(key)
			meta[key+"_ratio"] = extraRatio
		}
		for key, value := range usage.ExtraUsageUnits {
			meta[key+"_units"] = value
			meta[key+"_ratio"] = price.GetExtraRatio(key)
		}
	}

	if q.extraBillingData != nil {
		meta["extra_billing"] = q.extraBillingData
	}
	if q.settlementModel != "" {
		meta["billing_model"] = q.settlementModel
	}
	if q.settlementSpeed != "" {
		meta["effective_speed"] = q.settlementSpeed
	}
	if q.settlementTier != "" {
		meta["effective_service_tier"] = q.settlementTier
	}
	if q.settlementPolicyResolved {
		meta["billing_input_multiplier"] = q.settlementInputMultiplier
	}
	if q.settlementPolicyResolved {
		meta["billing_output_multiplier"] = q.settlementOutputMultiplier
	}
	if q.settlementTokenBilling != nil {
		effectiveExtras := make(map[string]float64)
		for key, prompt := range model.ExtraKeyIsPrompt {
			base := price.GetOutput()
			if prompt {
				base = price.GetInput()
			}
			rate := base * price.GetExtraRatio(key) * q.settlementRuleResult.For(key, prompt)
			if !math.IsNaN(rate) && !math.IsInf(rate, 0) {
				effectiveExtras[key] = rate
			}
		}
		meta["effective_extra_ratios"] = effectiveExtras
		details := *q.settlementTokenBilling
		details.Rules = append([]billingRateRule{}, details.Rules...)
		meta["token_billing"] = details
	}
	if len(q.billingDiagnostics) > 0 {
		diagnostics := make([]string, 0, len(q.billingDiagnostics))
		for diagnostic := range q.billingDiagnostics {
			diagnostics = append(diagnostics, diagnostic)
		}
		sort.Strings(diagnostics)
		meta["billing_diagnostics"] = diagnostics
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

type quotaPricePolicy struct {
	price            model.Price
	modelName        string
	serviceTier      string
	inputMultiplier  float64
	outputMultiplier float64
	missing          bool
	version          int64
	groupRatio       float64
	rates            model.PriceRuleResult
	ruleStatus       PriceComponentStatus
}

func (q *Quota) addBillingDiagnostic(diagnostic string) {
	if q == nil || strings.TrimSpace(diagnostic) == "" {
		return
	}
	if q.billingDiagnostics == nil {
		q.billingDiagnostics = make(map[string]bool)
	}
	q.billingDiagnostics[diagnostic] = true
}

func (q *Quota) clearCurrentPolicyDiagnostics() {
	if q == nil || len(q.billingDiagnostics) == 0 {
		return
	}
	for diagnostic := range q.billingDiagnostics {
		switch diagnostic {
		case "billing_group_unavailable", "billing_actual_model_price_missing", "billing_model_price_type_mismatch", "price_policy_missing_at_settlement", "billing_tier_unknown", "billing_speed_unknown", "token_billing_rates_unrepresentable", "token_billing_units_unrepresentable":
			delete(q.billingDiagnostics, diagnostic)
		default:
			if strings.HasPrefix(diagnostic, "billing_rule_") || strings.HasPrefix(diagnostic, "billing_rate_rules_") || strings.HasPrefix(diagnostic, "independent_unit_price_missing:") || strings.HasPrefix(diagnostic, "unit_component_price_missing:") {
				delete(q.billingDiagnostics, diagnostic)
			}
		}
	}
}

func (q *Quota) resolveQuotaPricePolicy(actualModel, serviceTier string, usage *types.Usage) quotaPricePolicy {
	q.clearCurrentPolicyDiagnostics()
	q.settlementTokenBilling = nil
	policy := quotaPricePolicy{
		modelName:        q.modelName,
		serviceTier:      strings.ToLower(strings.TrimSpace(serviceTier)),
		inputMultiplier:  1,
		outputMultiplier: 1,
	}
	if groupRatio, err := q.currentGroupRatio(); err == nil {
		policy.groupRatio = groupRatio
	} else {
		policy.missing = true
		q.addBillingDiagnostic("billing_group_unavailable")
	}
	actualModel = strings.TrimSpace(actualModel)
	if model.PricingInstance != nil {
		prices, version := model.PricingInstance.FindPricesWithVersion(q.modelName, actualModel)
		policy.version = version
		if requestPrice, ok := prices[q.modelName]; ok {
			policy.price = cloneQuotaPrice(requestPrice)
		}
		if !q.freezeRequestPricePolicy && actualModel != "" {
			if actualPrice, ok := prices[actualModel]; !ok {
				q.addBillingDiagnostic("billing_actual_model_price_missing")
			} else if policy.price.Model == "" || actualPrice.Type == policy.price.Type {
				policy.price = cloneQuotaPrice(actualPrice)
				policy.modelName = actualModel
				policy.version = version
			} else {
				q.addBillingDiagnostic("billing_model_price_type_mismatch")
			}
		}
	}
	policy.missing = policy.missing || policy.price.Model == ""
	if policy.missing {
		policy.price = model.Price{Model: policy.modelName, Type: model.TokensPriceType}
		q.addBillingDiagnostic("price_policy_missing_at_settlement")
	}

	return q.applyRateRules(policy, usage)
}

func (q *Quota) applyRateRules(policy quotaPricePolicy, usage *types.Usage) quotaPricePolicy {
	// rate_rules multiply token rates. A times price is already the complete
	// per-call amount, independent of token count, service tier, and context size.
	if policy.price.Type == model.TokensPriceType {
		q.settlementTokenBilling = &tokenBillingDetails{
			UnitsIncludeRules: true,
			BaseInputRatio:    policy.price.GetInput(),
			BaseOutputRatio:   policy.price.GetOutput(),
			Rules:             make([]billingRateRule, 0, 2),
		}
		facts := model.PriceRuleFacts{ServiceTier: policy.serviceTier, Speed: usage.Speed, SpeedConflict: usage.SpeedConflict, StartedAt: q.startTime}
		if usage.HasProviderBaseUsage() {
			inputTokens := usage.PromptTokens
			facts.InputTokens = &inputTokens
		}
		policy.rates = policy.price.EffectiveRateRules().Evaluate(facts)
		policy.inputMultiplier = policy.rates.Input
		policy.outputMultiplier = policy.rates.Output
		if policy.rates.Missing {
			policy.ruleStatus = PriceComponentMissingEvidence
		}
		if policy.rates.Conflict {
			policy.ruleStatus = PriceComponentConflictingEvidence
		}
		for _, diagnostic := range policy.rates.Diagnostics {
			q.addBillingDiagnostic(diagnostic)
		}
		q.settlementTokenBilling.Rules = policy.rates.Matches
		q.settlementTokenBilling.Facts = facts
		q.settlementTokenBilling.ExtraMultipliers = policy.rates.Extra
		q.settlementRuleResult = policy.rates
		q.settlementSpeed = usage.Speed

	}

	q.settlementModel = policy.modelName
	settlementPrice := cloneQuotaPrice(policy.price)
	q.settlementPrice = &settlementPrice
	q.settlementPriceVersion = policy.version
	q.settlementGroupRatio = policy.groupRatio
	q.settlementPolicyResolved = true
	q.settlementTier = policy.serviceTier
	if q.settlementTier == "" || q.settlementTier == "auto" {
		q.settlementTier = "default"
	}
	q.settlementInputMultiplier = policy.inputMultiplier
	q.settlementOutputMultiplier = policy.outputMultiplier
	q.settlementInputRatio = policy.price.GetInput() * policy.inputMultiplier
	q.settlementOutputRatio = policy.price.GetOutput() * policy.outputMultiplier
	return policy
}

func (q *Quota) getTotalQuotaWithPolicyUnits(policy quotaPricePolicy, promptTokenUnits, completionTokenUnits float64, extraBilling map[string]types.ExtraBilling) (quota int) {
	inputRatio := policy.price.GetInput() * policy.groupRatio
	outputRatio := policy.price.GetOutput() * policy.groupRatio
	if policy.price.Type == model.TimesPriceType {
		quota = saturatingCeilToInt(1000 * inputRatio)
	} else {
		inputAmount, outputAmount := 0.0, 0.0
		if inputRatio != 0 {
			inputAmount = promptTokenUnits * inputRatio
		}
		if outputRatio != 0 {
			outputAmount = completionTokenUnits * outputRatio
		}
		quota = saturatingCeilToInt(inputAmount + outputAmount)
	}

	q.getExtraBillingDataForModel(extraBilling, policy.modelName)
	extraBillingQuota := 0
	if q.extraBillingData != nil {
		for _, value := range q.extraBillingData {
			extraBillingQuota += int(math.Ceil(
				float64(value.Price)*q.effectiveQuotaPerUnit(),
			)) * value.CallCount
		}
	}

	if extraBillingQuota > 0 {
		quota = saturatingAddInt(quota, saturatingCeilToInt(
			float64(extraBillingQuota)*policy.groupRatio,
		))
	}

	if inputRatio != 0 && quota <= 0 && (promptTokenUnits > 0 || completionTokenUnits > 0) {
		quota = 1
	}
	if policy.price.Type == model.TokensPriceType && promptTokenUnits == 0 && completionTokenUnits == 0 && extraBillingQuota == 0 {
		// in this case, must be some error happened
		// we cannot just return, because we may have to return the pre-consumed quota
		quota = 0
	}

	return quota
}

func saturatingCeilToInt(value float64) int {
	if math.IsNaN(value) {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	if value >= float64(maxInt) {
		return maxInt
	}
	if value <= float64(minInt) {
		return minInt
	}
	return int(math.Ceil(value))
}

func saturatingTruncToInt(value float64) int {
	if math.IsNaN(value) {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	if value >= float64(maxInt) {
		return maxInt
	}
	if value <= float64(minInt) {
		return minInt
	}
	return int(value)
}

func saturatingAddInt(left, right int) int {
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	if right > 0 && left > maxInt-right {
		return maxInt
	}
	if right < 0 && left < minInt-right {
		return minInt
	}
	return left + right
}

func weightedTokenUnits(baseTokens int, extraTokens map[string]int, extraUsageUnits map[string]float64, independentUnits map[string]bool, policy quotaPricePolicy, prompt bool) float64 {
	side := policy.inputMultiplier
	if !prompt {
		side = policy.outputMultiplier
	}
	units := float64(baseTokens) * side
	for key, value := range extraTokens {
		if model.GetExtraPriceIsPrompt(key) != prompt || value <= 0 {
			continue
		}
		ratio := policy.price.GetExtraRatio(key) * policy.rates.For(key, prompt)
		if math.IsInf(ratio, 1) {
			return math.Inf(1)
		}
		if math.IsNaN(ratio) || ratio < 0 {
			return math.NaN()
		}
		units += float64(value) * (ratio - side)
	}
	for key, value := range extraUsageUnits {
		if independentUnits[key] {
			continue
		}
		if model.GetExtraPriceIsPrompt(key) != prompt || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		ratio := policy.price.GetExtraRatio(key) * policy.rates.For(key, prompt)
		if math.IsInf(ratio, 1) {
			return math.Inf(1)
		}
		if math.IsNaN(ratio) || ratio < 0 {
			return math.NaN()
		}
		// Extra usage units are not already present in the base token bucket.
		units += value * ratio
	}
	return units
}

func (q *Quota) getComputeTokenUnitsByUsageWithPrice(usage *types.Usage, policy quotaPricePolicy, partitionAdjustment tokenPartitionAdjustment) (float64, float64) {
	extraTokens := usage.GetExtraTokens()
	return weightedTokenUnits(usage.PromptTokens, extraTokens, usage.ExtraUsageUnits, usage.ProviderIndependentUsageUnits, policy, true) + partitionAdjustment.prompt,
		weightedTokenUnits(usage.CompletionTokens, extraTokens, usage.ExtraUsageUnits, usage.ProviderIndependentUsageUnits, policy, false) + partitionAdjustment.completion
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

func (q *Quota) getExtraBillingDataForModel(extraBilling map[string]types.ExtraBilling, modelName string) {
	if len(extraBilling) == 0 {
		q.extraBillingData = nil
		return
	}

	extraBillingData := make(map[string]ExtraBillingData)
	for billingKey, value := range extraBilling {
		serviceType := types.ResolveExtraBillingServiceType(billingKey, value)
		billingType := types.ResolveExtraBillingType(billingKey, value)
		price, diagnostic := getDefaultExtraServicePriceDecision(serviceType, modelName, billingType)
		if diagnostic != "" {
			q.addBillingDiagnostic(diagnostic)
		}
		extraBillingData[billingKey] = ExtraBillingData{
			ServiceType: serviceType,
			Type:        billingType,
			CallCount:   value.CallCount,
			Price:       price,
		}
	}

	if len(extraBillingData) == 0 {
		q.extraBillingData = nil
		return
	}

	q.extraBillingData = extraBillingData
}
