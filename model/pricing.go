package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/utils"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
	"gorm.io/gorm"
)

// PricingInstance is the Pricing instance
var PricingInstance *Pricing

type PriceUpdateMode string

const (
	PriceUpdateModeSystem    PriceUpdateMode = "system"
	PriceUpdateModeAdd       PriceUpdateMode = "add"
	PriceUpdateModeOverwrite PriceUpdateMode = "overwrite"
	PriceUpdateModeUpdate    PriceUpdateMode = "update"
)

// Pricing is a struct that contains the pricing data
type Pricing struct {
	sync.RWMutex
	reloadMu         sync.Mutex
	Prices           map[string]*Price `json:"models"`
	Match            []string          `json:"-"`
	publishedVersion int64
	publicationError string
	remoteSyncError  string
}

type BatchPrices struct {
	Models []string `json:"models" binding:"required"`
	Price  Price    `json:"price" binding:"required"`
}

const MaxRemotePriceCatalogBytes int64 = 16 << 20

// NewPricing creates a new Pricing instance
func NewPricing() {
	logger.SysLog("Initializing Pricing")
	logger.SysLog("Update Price Mode:" + viper.GetString("auto_price_updates_mode"))
	PricingInstance = &Pricing{
		Prices: make(map[string]*Price),
		Match:  make([]string, 0),
	}

	err := PricingInstance.Init()

	if err != nil {
		logger.SysError("Failed to initialize Pricing:" + err.Error())
		return
	}

	// 初始化时，需要检测是否有更新
	if viper.GetString("auto_price_updates_mode") == "system" && (viper.GetBool("auto_price_updates") || len(PricingInstance.Prices) == 0) {
		logger.SysLog("Checking for pricing updates")
		prices := GetDefaultPrice()
		if err := PricingInstance.SyncPricing(prices, "system"); err != nil {
			logger.SysError("Failed to initialize built-in pricing: " + err.Error())
		}
		logger.SysLog("Pricing initialized")
	}
}

// initializes the Pricing instance
func (p *Pricing) Init() error {
	return p.InitContext(context.Background())
}

func (p *Pricing) InitContext(ctx context.Context) (err error) {
	if p == nil {
		return errors.New("pricing publisher is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() { p.setPublicationError(err) }()
	if err := lockPublicationReload(ctx, &p.reloadMu); err != nil {
		return err
	}
	defer p.reloadMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		versionBefore, err := ReadPublicationVersion(ctx, DB, PublicationOwnerPrice)
		if err != nil {
			return err
		}
		var prices []*Price
		if err := DB.WithContext(ctx).Find(&prices).Error; err != nil {
			return err
		}

		var modelInfos []*ModelInfo
		modelInfoErr := DB.WithContext(ctx).Order("id desc").Find(&modelInfos).Error
		if modelInfoErr == nil {
			modelInfoMap := make(map[string]*ModelInfoResponse)
			for _, info := range modelInfos {
				modelInfoMap[info.Model] = info.ToResponse()
			}
			for _, price := range prices {
				if info, ok := modelInfoMap[price.Model]; ok {
					price.ModelInfo = info
				}
			}
		} else {
			logger.SysError("Failed to fetch model infos: " + modelInfoErr.Error())
		}

		versionAfter, err := ReadPublicationVersion(ctx, DB, PublicationOwnerPrice)
		if err != nil {
			return err
		}
		if versionBefore != versionAfter {
			continue
		}
		newPrices, newMatchList, err := buildPriceState(prices)
		if err != nil {
			return err
		}
		p.replacePublication(versionBefore, newPrices, newMatchList)
		return nil
	}
	return ErrPublicationVersionConflict
}

func (p *Pricing) setPublicationError(err error) {
	if p == nil {
		return
	}
	p.Lock()
	defer p.Unlock()
	p.publicationError = errorText(err)
}

func (p *Pricing) setRemoteSyncError(err error) {
	if p == nil {
		return
	}
	p.Lock()
	defer p.Unlock()
	p.remoteSyncError = errorText(err)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (p *Pricing) IsDegraded() bool {
	if p == nil {
		return true
	}
	p.RLock()
	defer p.RUnlock()
	return p.publicationError != "" || p.remoteSyncError != ""
}

func buildPriceState(prices []*Price) (map[string]*Price, []string, error) {
	newPrices := make(map[string]*Price, len(prices))
	newMatch := make(map[string]bool)
	for _, price := range prices {
		if err := price.prepareForPersistence(); err != nil {
			return nil, nil, err
		}
		if _, exists := newPrices[price.Model]; exists {
			return nil, nil, fmt.Errorf("duplicate price model %q", price.Model)
		}
		newPrices[price.Model] = price
		if strings.HasSuffix(price.Model, "*") {
			newMatch[price.Model] = true
		}
	}
	newMatchList := make([]string, 0, len(newMatch))
	for match := range newMatch {
		newMatchList = append(newMatchList, match)
	}
	sortPriceMatchPatterns(newMatchList)
	return newPrices, newMatchList, nil
}

func sortPriceMatchPatterns(patterns []string) {
	sort.Slice(patterns, func(i, j int) bool {
		leftPrefix := strings.TrimSuffix(patterns[i], "*")
		rightPrefix := strings.TrimSuffix(patterns[j], "*")
		if len(leftPrefix) != len(rightPrefix) {
			return len(leftPrefix) > len(rightPrefix)
		}
		return patterns[i] < patterns[j]
	})
}

// GetPrice returns the price of a model
func (p *Pricing) GetPrice(modelName string) *Price {
	if price, ok := p.FindPrice(modelName); ok {
		return price
	}

	return &Price{
		Type:        TokensPriceType,
		ChannelType: config.ChannelTypeUnknown,
		Input:       DefaultPrice,
		Output:      DefaultPrice,
	}
}

func (p *Pricing) FindPrice(modelName string) (*Price, bool) {
	price, _, ok := p.FindPriceWithVersion(modelName)
	return price, ok
}

// FindPriceWithVersion pairs one current price read with the publication
// version that produced it, so audit metadata describes the actual charge.
func (p *Pricing) FindPriceWithVersion(modelName string) (*Price, int64, bool) {
	if p == nil {
		return nil, 0, false
	}
	p.RLock()
	defer p.RUnlock()
	price, ok := p.findPriceLocked(modelName)
	if !ok {
		return nil, p.publishedVersion, false
	}
	cloned := clonePricePolicy(*price)
	return &cloned, p.publishedVersion, true
}

// FindPricesWithVersion resolves several models against one current
// publication for a single billing decision. The result is not retained by
// the request.
func (p *Pricing) FindPricesWithVersion(modelNames ...string) (map[string]Price, int64) {
	result := make(map[string]Price, len(modelNames))
	if p == nil {
		return result, 0
	}
	p.RLock()
	defer p.RUnlock()
	for _, modelName := range modelNames {
		if price, ok := p.findPriceLocked(modelName); ok {
			result[modelName] = clonePricePolicy(*price)
		}
	}
	return result, p.publishedVersion
}

func (p *Pricing) findPriceLocked(modelName string) (*Price, bool) {
	if price, ok := p.Prices[modelName]; ok {
		return price, true
	}

	matchModel := utils.GetModelsWithMatch(&p.Match, modelName)
	if price, ok := p.Prices[matchModel]; ok {
		return price, true
	}

	return nil, false
}

func (p *Pricing) FindExactPrice(modelName string) (*Price, bool) {
	if p == nil {
		return nil, false
	}
	p.RLock()
	defer p.RUnlock()
	price, ok := p.Prices[modelName]
	if !ok || price == nil {
		return nil, false
	}
	cloned := clonePricePolicy(*price)
	return &cloned, true
}

func (p *Pricing) GetAllPrices() map[string]*Price {
	if p == nil {
		return map[string]*Price{}
	}
	p.RLock()
	defer p.RUnlock()
	prices := make(map[string]*Price, len(p.Prices))
	for modelName, price := range p.Prices {
		if price == nil {
			continue
		}
		cloned := clonePricePolicy(*price)
		prices[modelName] = &cloned
	}
	return prices
}

func (p *Pricing) GetAllPricesList() []*Price {
	prices, _ := p.GetAllPricesListWithVersion()
	return prices
}

// GetAllPricesListWithVersion keeps management rows and their CAS version
// paired under one read lock. It is not retained by request billing.
func (p *Pricing) GetAllPricesListWithVersion() ([]*Price, int64) {
	if p == nil {
		return nil, 0
	}
	p.RLock()
	defer p.RUnlock()
	prices := make([]*Price, 0, len(p.Prices))
	for _, price := range p.Prices {
		if price == nil {
			continue
		}
		cloned := clonePricePolicy(*price)
		prices = append(prices, &cloned)
	}
	sort.Slice(prices, func(i, j int) bool {
		if prices[i].ChannelType == prices[j].ChannelType {
			return prices[i].Model < prices[j].Model
		}
		return prices[i].ChannelType < prices[j].ChannelType
	})

	return prices, p.publishedVersion
}

func (p *Pricing) updateRawPrice(modelName string, price *Price) error {
	if err := price.prepareForPersistence(); err != nil {
		return err
	}
	if _, ok := p.Prices[modelName]; !ok {
		return errors.New("model not found")
	}

	if _, ok := p.Prices[price.Model]; modelName != price.Model && ok {
		return errors.New("model names cannot be duplicated")
	}

	return price.Update(modelName)
}

// UpdatePrice updates the price of a model
func (p *Pricing) UpdatePrice(modelName string, price *Price) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.UpdatePriceAtVersion(modelName, price, true, version)
}

// UpdatePriceWithRateRulesPresence treats an omitted rate_rules field as a
// database partial update. The database, not a process-local cache snapshot,
// remains the authority when multiple instances update pricing concurrently.
func (p *Pricing) UpdatePriceWithRateRulesPresence(modelName string, price *Price, rateRulesPresent bool) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.UpdatePriceAtVersion(modelName, price, rateRulesPresent, version)
}

func (p *Pricing) UpdatePriceAtVersion(modelName string, price *Price, rateRulesPresent bool, expectedVersion int64) error {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || price == nil {
		return errors.New("price and existing model are required")
	}
	if err := price.prepareForPersistence(); err != nil {
		return err
	}
	columns := []string{"model", "type", "channel_type", "input", "output", "locked", "extra_ratios"}
	if rateRulesPresent {
		columns = append(columns, "rate_rules")
	}
	return p.mutatePriceAtVersion(expectedVersion, func(tx *gorm.DB) error {
		var stored Price
		if err := tx.Where("model = ?", modelName).Take(&stored).Error; err != nil {
			return err
		}
		if priceUpdateTargetMatches(&stored, price, true, rateRulesPresent) {
			return errPriceMutationNoChange
		}
		result := tx.Model(&Price{}).Where("model = ?", modelName).Select(columns).Updates(price)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("model not found or changed concurrently")
		}
		return nil
	})
}

func (p *Pricing) addRawPrice(price *Price) error {
	if err := price.prepareForPersistence(); err != nil {
		return err
	}
	if _, ok := p.Prices[price.Model]; ok {
		return errors.New("model already exists")
	}

	return price.Insert()
}

// AddPrice adds a new price to the Pricing instance
func (p *Pricing) AddPrice(price *Price) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.AddPriceAtVersion(price, version)
}

func (p *Pricing) AddPriceAtVersion(price *Price, expectedVersion int64) error {
	if err := price.prepareForPersistence(); err != nil {
		return err
	}
	return p.mutatePriceAtVersion(expectedVersion, func(tx *gorm.DB) error {
		return tx.Create(price).Error
	})
}

func (p *Pricing) deleteRawPrice(modelName string) error {
	item, ok := p.Prices[modelName]
	if !ok {
		return errors.New("model not found")
	}

	return item.Delete()
}

// DeletePrice deletes a price from the Pricing instance
func (p *Pricing) DeletePrice(modelName string) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.DeletePriceAtVersion(modelName, version)
}

func (p *Pricing) DeletePriceAtVersion(modelName string, expectedVersion int64) error {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return errors.New("model is required")
	}
	return p.mutatePriceAtVersion(expectedVersion, func(tx *gorm.DB) error {
		result := tx.Where("model = ?", modelName).Delete(&Price{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("model not found or changed concurrently")
		}
		return nil
	})
}

var ErrPriceCommitOutcomeUnknown = errors.New("price commit outcome is unknown")
var errPriceMutationNoChange = errors.New("price mutation has no changes")

type priceMutationVerifier func(*gorm.DB) (bool, error)

func (p *Pricing) mutatePriceAtVersion(expectedVersion int64, mutate func(*gorm.DB) error) error {
	if p == nil || DB == nil || mutate == nil {
		return errors.New("pricing mutation is unavailable")
	}
	if expectedVersion < 1 {
		return errors.New("expected price version must be positive")
	}
	ctx := context.Background()
	newVersion := expectedVersion + 1
	tx := DB.WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback().Error
		}
	}()
	head, err := ReadPublicationVersion(ctx, tx, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	if head != expectedVersion {
		return ErrPublicationVersionConflict
	}
	if err := mutate(tx); err != nil {
		if errors.Is(err, errPriceMutationNoChange) {
			_ = tx.Rollback().Error
			rollback = false
			p.convergeAfterPriceCommit(ctx, expectedVersion)
			return nil
		}
		return err
	}
	targetState, err := loadPricePolicyState(tx)
	if err != nil {
		return err
	}
	if _, err := BumpPublicationVersionCAS(ctx, tx, PublicationOwnerPrice, expectedVersion); err != nil {
		return err
	}
	rollback = false
	commitErr := tx.Commit().Error
	if commitErr != nil {
		verify := func(probe *gorm.DB) (bool, error) {
			actual, err := loadPricePolicyState(probe)
			return reflect.DeepEqual(actual, targetState), err
		}
		if resolvedErr := resolvePriceCommitOutcome(ctx, expectedVersion, commitErr, verify); resolvedErr != nil {
			return resolvedErr
		}
	}
	p.convergeAfterPriceCommit(ctx, newVersion)
	return nil
}

func (p *Pricing) publishPriceVersionAtLeast(ctx context.Context, version int64) error {
	loadCtx, cancelLoad := publicationCommitProbeContext(ctx)
	defer cancelLoad()
	if err := p.InitContext(loadCtx); err != nil {
		return fmt.Errorf("price version %d committed but local publication failed: %w", version, err)
	}
	if p.PublishedVersion() < version {
		return fmt.Errorf("price version %d committed but was not published", version)
	}
	return nil
}

func (p *Pricing) convergeAfterPriceCommit(ctx context.Context, version int64) {
	if err := p.publishPriceVersionAtLeast(ctx, version); err != nil {
		p.setPublicationError(err)
		if logger.Logger != nil {
			logger.SysError(fmt.Sprintf("price version %d committed but local publication is pending: %v", version, err))
		}
	}
}

func loadPricePolicyState(tx *gorm.DB) ([]PricePolicyView, error) {
	prices, err := loadPriceMap(tx)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(prices))
	for modelName := range prices {
		models = append(models, modelName)
	}
	sort.Strings(models)
	state := make([]PricePolicyView, 0, len(models))
	for _, modelName := range models {
		state = append(state, pricePolicyView(prices[modelName]))
	}
	return state, nil
}

func resolvePriceCommitOutcome(ctx context.Context, expectedVersion int64, transactionErr error, verify priceMutationVerifier) error {
	if transactionErr == nil {
		return nil
	}
	if errors.Is(transactionErr, ErrPublicationVersionConflict) {
		return transactionErr
	}
	probeCtx, cancelProbe := publicationCommitProbeContext(ctx)
	defer cancelProbe()
	head, err := ReadPublicationVersion(probeCtx, DB, PublicationOwnerPrice)
	if err != nil {
		return fmt.Errorf("%w: transaction error: %v; head read failed: %v", ErrPriceCommitOutcomeUnknown, transactionErr, err)
	}
	if head == expectedVersion {
		return transactionErr
	}
	if head != expectedVersion+1 {
		return fmt.Errorf("%w: transaction error: %v; expected head %d, got %d", ErrPriceCommitOutcomeUnknown, transactionErr, expectedVersion+1, head)
	}
	matched, err := verify(DB.WithContext(probeCtx))
	if err != nil {
		return fmt.Errorf("%w: transaction error: %v; target verification failed: %v", ErrPriceCommitOutcomeUnknown, transactionErr, err)
	}
	confirmedHead, err := ReadPublicationVersion(probeCtx, DB, PublicationOwnerPrice)
	if err != nil {
		return fmt.Errorf("%w: transaction error: %v; head confirmation failed: %v", ErrPriceCommitOutcomeUnknown, transactionErr, err)
	}
	if confirmedHead != head {
		return fmt.Errorf("%w: transaction error: %v; head changed during target verification from %d to %d", ErrPriceCommitOutcomeUnknown, transactionErr, head, confirmedHead)
	}
	if !matched {
		return fmt.Errorf("%w: transaction error: %v; committed head does not match target state", ErrPriceCommitOutcomeUnknown, transactionErr)
	}
	return nil
}

func priceUpdateTargetMatches(stored, expected *Price, extraRatiosPresent, rateRulesPresent bool) bool {
	if stored == nil || expected == nil {
		return false
	}
	storedView := pricePolicyView(stored)
	expectedView := pricePolicyView(expected)
	if !extraRatiosPresent {
		storedView.ExtraRatios = nil
		expectedView.ExtraRatios = nil
	}
	if !rateRulesPresent {
		storedView.RateRules = nil
		expectedView.RateRules = nil
	}
	if !reflect.DeepEqual(storedView, expectedView) {
		return false
	}
	return true
}

// SyncPricing syncs the pricing data
func (p *Pricing) SyncPricing(pricing []*Price, mode string) error {
	logger.SysLog("prices update mode：" + mode)
	switch mode {
	case string(PriceUpdateModeSystem), string(PriceUpdateModeAdd):
		return p.SyncPriceWithoutOverwrite(pricing)
	case string(PriceUpdateModeUpdate):
		return p.SyncPriceOnlyUpdate(pricing)
	case string(PriceUpdateModeOverwrite):
		return p.SyncPriceWithOverwrite(pricing)
	default:
		return fmt.Errorf("unsupported price update mode %q", mode)
	}
}

func UpdatePriceByPriceService() (err error) {
	defer func() {
		if PricingInstance != nil {
			PricingInstance.setRemoteSyncError(err)
		}
	}()
	updatePriceMode := viper.GetString("auto_price_updates_mode")
	if updatePriceMode == string(PriceUpdateModeSystem) {
		// 使用程序内置更新
		return nil
	}
	prices, err := GetPriceByPriceService()
	if err != nil {
		return err
	}
	if PricingInstance == nil {
		return errors.New("pricing publisher is not initialized")
	}
	switch PriceUpdateMode(updatePriceMode) {
	case PriceUpdateModeAdd, PriceUpdateModeOverwrite, PriceUpdateModeUpdate:
		return PricingInstance.SyncPricing(prices, updatePriceMode)
	default:
		return errors.New("更新模式错误，更新模式仅能选择：add、overwrite、update、system")
	}
}

// GetPriceByPriceService 只插入系统没有的数据
func GetPriceByPriceService() ([]*Price, error) {
	api := viper.GetString("update_price_service")
	if api == "" {
		return nil, errors.New("update_price_service is not configured")
	}
	logger.SysLog("Start Update Price,Prices Service URL：" + api)
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get(api)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch prices from service: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("price service returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxRemotePriceCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}
	if int64(len(body)) > MaxRemotePriceCatalogBytes {
		return nil, errors.New("price service response exceeds size limit")
	}
	prices, err := decodeRemotePriceCatalog(body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse price data: %v", err)
	}
	logger.SysLog(fmt.Sprintf("成功解析价格目录，共获取到 %d 个价格配置", len(prices)))
	return prices, nil
}

// SyncPriceWithOverwrite 删除系统所有数据并插入所有查询到的新数据 不含lock的数据
func (p *Pricing) SyncPriceWithOverwrite(pricing []*Price) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.SyncPriceWithOverwriteAtVersion(pricing, version)
}

func (p *Pricing) SyncPriceWithOverwriteAtVersion(pricing []*Price, expectedVersion int64) error {
	return p.syncPriceChangeAtVersion(pricing, PriceUpdateModeOverwrite, expectedVersion)
}

// SyncPriceOnlyUpdate 只更新系统现有的数据 不含lock的数据
func (p *Pricing) SyncPriceOnlyUpdate(pricing []*Price) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.SyncPriceOnlyUpdateAtVersion(pricing, version)
}

func (p *Pricing) SyncPriceOnlyUpdateAtVersion(pricing []*Price, expectedVersion int64) error {
	return p.syncPriceChangeAtVersion(pricing, PriceUpdateModeUpdate, expectedVersion)
}

// priceForSync applies the remote price as a partial policy update. A missing
// rate_rules field means "do not update the local rules"; an explicit empty
// object removes all conditional rules.
func priceForSync(incoming, current *Price) *Price {
	if incoming == nil {
		return nil
	}
	// This explicit field set is the backend half of the pricing comparison
	// contract shown by CheckUpdates. New persistent fields must be added to the
	// confirmation UI before they can become remotely syncable.
	synced := &Price{
		Model:       incoming.Model,
		Type:        incoming.Type,
		ChannelType: incoming.ChannelType,
		Input:       incoming.Input,
		Output:      incoming.Output,
		Locked:      incoming.Locked,
		ExtraRatios: incoming.ExtraRatios,
		RateRules:   incoming.RateRules,
	}
	if synced.RateRules == nil && current != nil {
		synced.RateRules = current.RateRules
	}
	return synced
}

// SyncPriceWithoutOverwrite 只插入系统没有的数据
func (p *Pricing) SyncPriceWithoutOverwrite(pricing []*Price) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.SyncPriceWithoutOverwriteAtVersion(pricing, version)
}

func (p *Pricing) SyncPriceWithoutOverwriteAtVersion(pricing []*Price, expectedVersion int64) error {
	return p.syncPriceChangeAtVersion(pricing, PriceUpdateModeAdd, expectedVersion)
}

func (p *Pricing) syncPriceChangeAtVersion(pricing []*Price, mode PriceUpdateMode, expectedVersion int64) error {
	preview, err := PreviewPriceChange(context.Background(), pricing, mode)
	if err != nil {
		return err
	}
	if preview.BaseVersion != expectedVersion {
		return ErrPublicationVersionConflict
	}
	_, err = ApplyPriceChange(context.Background(), p, pricing, mode, preview.BaseVersion, preview.Digest)
	return err
}

func validatePriceInputSet(pricing []*Price) error {
	if len(pricing) == 0 {
		return errors.New("prices are required")
	}
	seen := make(map[string]struct{}, len(pricing))
	for _, price := range pricing {
		if err := price.prepareForPersistence(); err != nil {
			return err
		}
		if _, exists := seen[price.Model]; exists {
			return fmt.Errorf("duplicate price model %q", price.Model)
		}
		seen[price.Model] = struct{}{}
	}
	return nil
}

func loadPriceMap(tx *gorm.DB) (map[string]*Price, error) {
	var prices []*Price
	if err := tx.Find(&prices).Error; err != nil {
		return nil, err
	}
	result := make(map[string]*Price, len(prices))
	for _, price := range prices {
		result[price.Model] = price
	}
	return result, nil
}

// BatchDeletePrices deletes the prices of multiple models
func (p *Pricing) BatchDeletePrices(models []string) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.BatchDeletePricesAtVersion(models, version)
}

func (p *Pricing) BatchDeletePricesAtVersion(models []string, expectedVersion int64) error {
	if len(models) == 0 {
		return errors.New("models are required")
	}
	return p.mutatePriceAtVersion(expectedVersion, func(tx *gorm.DB) error {
		result := tx.Where("model IN (?)", models).Delete(&Price{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(len(models)) {
			return errors.New("one or more models were not found or changed concurrently")
		}
		return nil
	})
}

func (p *Pricing) BatchSetPrices(batchPrices *BatchPrices, originalModels []string) error {
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	return p.BatchSetPricesAtVersion(batchPrices, originalModels, version)
}

func (p *Pricing) BatchSetPricesAtVersion(batchPrices *BatchPrices, originalModels []string, expectedVersion int64) error {
	if batchPrices == nil || len(batchPrices.Models) == 0 {
		return errors.New("batch prices and models are required")
	}
	// 查找需要删除的model
	var deletePrices []string
	var addPrices []*Price
	var updatePrices []string

	for _, model := range originalModels {
		if !utils.Contains(model, batchPrices.Models) {
			deletePrices = append(deletePrices, model)
		} else {
			updatePrices = append(updatePrices, model)
		}
	}

	for _, model := range batchPrices.Models {
		if !utils.Contains(model, originalModels) {
			addPrice := batchPrices.Price
			addPrice.Model = model
			addPrices = append(addPrices, &addPrice)
		}
	}

	return p.mutatePriceAtVersion(expectedVersion, func(tx *gorm.DB) error {
		current, err := loadPriceMap(tx)
		if err != nil {
			return err
		}
		changed := len(addPrices) > 0 || len(deletePrices) > 0
		if len(addPrices) > 0 {
			if err := InsertPrices(tx, addPrices); err != nil {
				return err
			}
		}
		for _, modelName := range updatePrices {
			prepared := batchPrices.Price
			prepared.Model = modelName
			if err := prepared.prepareForPersistence(); err != nil {
				return err
			}
			stored, exists := current[modelName]
			if !exists {
				return errors.New("batch price model not found or changed concurrently")
			}
			if priceUpdateTargetMatches(stored, &prepared, prepared.ExtraRatios != nil, prepared.RateRules != nil) {
				continue
			}
			columns := []string{"type", "channel_type", "input", "output", "locked"}
			if prepared.ExtraRatios != nil {
				columns = append(columns, "extra_ratios")
			}
			if prepared.RateRules != nil {
				columns = append(columns, "rate_rules")
			}
			result := tx.Model(&Price{}).Where("model = ?", modelName).Select(columns).Updates(&prepared)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("batch price model not found or changed concurrently")
			}
			changed = true
		}
		if len(deletePrices) > 0 {
			result := tx.Where("model IN (?)", deletePrices).Delete(&Price{})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != int64(len(deletePrices)) {
				return errors.New("batch delete model not found or changed concurrently")
			}
		}
		if !changed {
			return errPriceMutationNoChange
		}
		return nil
	})
}

func GetPricesList(pricingType string) []*Price {
	var prices []*Price

	switch pricingType {
	case "default":
		prices = GetDefaultPrice()
	case "db":
		prices = PricingInstance.GetAllPricesList()
	default:
		return nil
	}

	sort.Slice(prices, func(i, j int) bool {
		if prices[i].ChannelType == prices[j].ChannelType {
			return prices[i].Model < prices[j].Model
		}
		return prices[i].ChannelType < prices[j].ChannelType
	})

	return prices
}

// func ConvertBatchPrices(prices []*Price) []*BatchPrices {
// 	batchPricesMap := make(map[string]*BatchPrices)
// 	for _, price := range prices {
// 		key := fmt.Sprintf("%s-%d-%g-%g", price.Type, price.ChannelType, price.Input, price.Output)
// 		batchPrice, exists := batchPricesMap[key]
// 		if exists {
// 			batchPrice.Models = append(batchPrice.Models, price.Model)
// 		} else {
// 			batchPricesMap[key] = &BatchPrices{
// 				Models: []string{price.Model},
// 				Price:  *price,
// 			}
// 		}
// 	}

// 	var batchPrices []*BatchPrices
// 	for _, batchPrice := range batchPricesMap {
// 		batchPrices = append(batchPrices, batchPrice)
// 	}

// 	return batchPrices
// }
