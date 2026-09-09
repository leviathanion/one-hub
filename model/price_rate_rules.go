package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"one-api/common/config"
	"reflect"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	MaxPriceRateRules  = 64
	MaxPriceRuleValues = 32
	MaxPriceRulesBytes = 64 * 1024
)

type PriceRateRules struct {
	Version     int                 `json:"version,omitempty"`
	ServiceTier []PriceRateRule     `json:"service_tier,omitempty"`
	Speed       []PriceRateRule     `json:"speed,omitempty"`
	LongContext []PriceRateRule     `json:"long_context,omitempty"`
	Schedule    *PriceScheduleRules `json:"schedule,omitempty"`
}

type PriceScheduleRules struct {
	Timezone string          `json:"timezone,omitempty"`
	Rules    []PriceRateRule `json:"rules,omitempty"`
}

type PriceRateRule struct {
	ID          string              `json:"id"`
	When        PriceRuleCondition  `json:"when"`
	Multipliers PriceRateMultiplier `json:"multipliers"`
}

type PriceRuleCondition struct {
	ServiceTier []string         `json:"service_tier,omitempty"`
	Speed       []string         `json:"speed,omitempty"`
	InputTokens *PriceTokenRange `json:"input_tokens,omitempty"`
	Weekdays    []int            `json:"weekdays,omitempty"`
	Time        *PriceTimeRange  `json:"time,omitempty"`
}

type PriceTokenRange struct {
	GT  *int `json:"gt,omitempty"`
	LTE *int `json:"lte,omitempty"`
}

type PriceTimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type PriceRateMultiplier struct {
	All    *float64           `json:"all,omitempty"`
	Input  *float64           `json:"input,omitempty"`
	Output *float64           `json:"output,omitempty"`
	Extra  map[string]float64 `json:"extra_multipliers,omitempty"`
}

func (m PriceRateMultiplier) For(key string, prompt bool) float64 {
	if value, ok := m.Extra[key]; key != "" && ok {
		return value
	}
	if prompt && m.Input != nil {
		return *m.Input
	}
	if !prompt && m.Output != nil {
		return *m.Output
	}
	if m.All != nil {
		return *m.All
	}
	return 1
}

type PriceRuleFacts struct {
	ServiceTier   string    `json:"service_tier"`
	Speed         string    `json:"speed"`
	SpeedConflict bool      `json:"-"`
	InputTokens   *int      `json:"input_tokens"`
	StartedAt     time.Time `json:"started_at"`
}

type PriceRuleMatch struct {
	When             PriceRuleCondition `json:"when"`
	Kind             string             `json:"kind"`
	ID               string             `json:"id"`
	InputMultiplier  float64            `json:"input_multiplier"`
	OutputMultiplier float64            `json:"output_multiplier"`
	ExtraMultipliers map[string]float64 `json:"extra_multipliers,omitempty"`
}

type PriceRuleResult struct {
	Input       float64            `json:"input"`
	Output      float64            `json:"output"`
	Extra       map[string]float64 `json:"extra"`
	Matches     []PriceRuleMatch   `json:"matches"`
	Missing     bool               `json:"missing"`
	Conflict    bool               `json:"conflict"`
	Diagnostics []string           `json:"diagnostics,omitempty"`
}

func (r PriceRuleResult) For(key string, prompt bool) float64 {
	if v, ok := r.Extra[key]; ok {
		return v
	}
	if prompt {
		return r.Input
	}
	return r.Output
}

type priceRuleGroup struct {
	name  string
	rules []PriceRateRule
}

func (r PriceRateRules) groups() []priceRuleGroup {
	var calendar []PriceRateRule
	if r.Schedule != nil {
		calendar = r.Schedule.Rules
	}
	return []priceRuleGroup{{"service_tier", r.ServiceTier}, {"speed", r.Speed}, {"long_context", r.LongContext}, {"schedule", calendar}}
}

func (r PriceRateRules) Empty() bool {
	return len(r.ServiceTier)+len(r.Speed)+len(r.LongContext) == 0 && (r.Schedule == nil || len(r.Schedule.Rules) == 0)
}

// 配置边界严格解码；协议 wire 的未知字段不受这个 schema 限制。
func (r *PriceRateRules) UnmarshalJSON(data []byte) error {
	if len(data) > MaxPriceRulesBytes {
		return errors.New("rate_rules exceeds 64 KiB")
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return errors.New("rate_rules must be an object")
	}
	if value, present := fields["version"]; present && value != float64(2) {
		return errors.New("rate_rules version must be 2")
	}
	if err := rejectPriceRuleNull(raw); err != nil {
		return err
	}
	type plain PriceRateRules
	var value plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("rate_rules must contain one object")
	}
	*r = PriceRateRules(value)
	return validatePriceRateRules(*r)
}

func rejectPriceRuleNull(value any) error {
	switch v := value.(type) {
	case nil:
		return errors.New("rate rule fields cannot be null; omit to inherit")
	case map[string]any:
		for _, child := range v {
			if err := rejectPriceRuleNull(child); err != nil {
				return err
			}
		}
	case []any:
		if len(v) == 0 {
			return nil
		}
		for _, child := range v {
			if err := rejectPriceRuleNull(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func ClonePriceRateRules(r PriceRateRules) PriceRateRules {
	clone := r
	groups := []*[]PriceRateRule{&clone.ServiceTier, &clone.Speed, &clone.LongContext}
	if r.Schedule != nil {
		calendar := *r.Schedule
		clone.Schedule = &calendar
		groups = append(groups, &clone.Schedule.Rules)
	}
	for _, group := range groups {
		*group = slices.Clone(*group)
		for i := range *group {
			rule := &(*group)[i]
			rule.When.ServiceTier = slices.Clone(rule.When.ServiceTier)
			rule.When.Speed = slices.Clone(rule.When.Speed)
			rule.When.Weekdays = slices.Clone(rule.When.Weekdays)
			if rule.When.InputTokens != nil {
				v := *rule.When.InputTokens
				v.GT = cloneRateNumber(v.GT)
				v.LTE = cloneRateNumber(v.LTE)
				rule.When.InputTokens = &v
			}
			if rule.When.Time != nil {
				v := *rule.When.Time
				rule.When.Time = &v
			}
			rule.Multipliers.All = cloneRateNumber(rule.Multipliers.All)
			rule.Multipliers.Input = cloneRateNumber(rule.Multipliers.Input)
			rule.Multipliers.Output = cloneRateNumber(rule.Multipliers.Output)
			if rule.Multipliers.Extra != nil {
				m := make(map[string]float64, len(rule.Multipliers.Extra))
				for k, v := range rule.Multipliers.Extra {
					m[k] = v
				}
				rule.Multipliers.Extra = m
			}
		}
	}
	return clone
}

func cloneRateNumber[T int | float64](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (p *Price) EffectiveRateRules() PriceRateRules {
	if p == nil || p.RateRules == nil {
		return PriceRateRules{}
	}
	return ClonePriceRateRules(p.RateRules.Data())
}

func validatePriceRateRules(r PriceRateRules) error {
	if r.Version != 2 && !(r.Version == 0 && r.Empty() && r.Schedule == nil) {
		return errors.New("rate_rules version must be 2")
	}
	if r.Schedule != nil && r.Schedule.Timezone != "" {
		if _, err := time.LoadLocation(r.Schedule.Timezone); err != nil || r.Schedule.Timezone == "Local" {
			return errors.New("timezone must be an explicit IANA timezone")
		}
	}
	ids := map[string]bool{}
	total := 0
	for _, group := range r.groups() {
		seen := map[string]bool{}
		for i, rule := range group.rules {
			total++
			if total > MaxPriceRateRules {
				return errors.New("too many rate rules (maximum 64)")
			}
			if rule.ID == "" || rule.ID != strings.TrimSpace(rule.ID) || len(rule.ID) > 100 || ids[rule.ID] {
				return errors.New("rule id must be unique, non-empty and at most 100 bytes")
			}
			ids[rule.ID] = true
			w := rule.When
			if err := validatePriceRuleGroupCondition(group.name, w); err != nil {
				return fmt.Errorf("rule %s: %w", rule.ID, err)
			}
			if group.name != "schedule" {
				for _, earlier := range group.rules[:i] {
					if priceConditionsOverlap(group.name, earlier.When, w) {
						return fmt.Errorf("%s rules %s and %s overlap", group.name, earlier.ID, rule.ID)
					}
				}
			}

			for _, values := range [][]string{w.ServiceTier, w.Speed} {
				if values != nil && (len(values) == 0 || len(values) > MaxPriceRuleValues) {
					return errors.New("enum condition must contain 1 to 32 values")
				}
				unique := map[string]bool{}
				for _, v := range values {
					if v == "" || v != strings.TrimSpace(v) || len(v) > 100 || unique[v] {
						return errors.New("enum values must be unique, non-empty and at most 100 bytes")
					}
					unique[v] = true
				}
			}
			if v := w.InputTokens; v != nil {
				if v.GT == nil && v.LTE == nil || v.GT != nil && *v.GT < 0 || v.LTE != nil && *v.LTE < 0 || v.GT != nil && v.LTE != nil && *v.GT >= *v.LTE {
					return errors.New("input_tokens requires a non-negative gt/lte range")
				}
			}
			if w.Weekdays != nil {
				if len(w.Weekdays) == 0 || len(w.Weekdays) > 7 {
					return errors.New("weekdays must contain 1 to 7 values")
				}
				used := map[int]bool{}
				for _, v := range w.Weekdays {
					if v < 1 || v > 7 || used[v] {
						return errors.New("weekdays must be unique ISO weekdays (1 to 7)")
					}
					used[v] = true
				}
			}
			if w.Time != nil {
				start, e1 := priceMinute(w.Time.Start)
				end, e2 := priceMinute(w.Time.End)
				if e1 != nil || e2 != nil || start == end {
					return errors.New("time requires distinct HH:mm start/end")
				}
			}
			if (w.Time != nil || len(w.Weekdays) > 0) && (r.Schedule == nil || r.Schedule.Timezone == "") {
				return errors.New("calendar conditions require timezone")
			}
			if reflect.DeepEqual(w, PriceRuleCondition{}) && i != len(group.rules)-1 {
				return errors.New("fallback must be the last rule in its group")
			}
			canonical := w
			canonical.ServiceTier = slices.Clone(w.ServiceTier)
			canonical.Speed = slices.Clone(w.Speed)
			canonical.Weekdays = slices.Clone(w.Weekdays)
			slices.Sort(canonical.ServiceTier)
			slices.Sort(canonical.Speed)
			slices.Sort(canonical.Weekdays)
			encoded, _ := json.Marshal(canonical)
			if seen[string(encoded)] {
				return errors.New("duplicate rule conditions within a group")
			}
			seen[string(encoded)] = true
			m := rule.Multipliers
			valid := func(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
			for _, v := range []*float64{m.All, m.Input, m.Output} {
				if v != nil && !valid(*v) {
					return errors.New("multipliers must be finite and non-negative")
				}
			}
			for key, v := range m.Extra {
				if _, ok := ExtraKeyIsPrompt[key]; !ok || key == config.UsageExtraInputAudioTranscription || !valid(v) {
					return fmt.Errorf("invalid extra multiplier %q", key)
				}
			}
		}
	}
	// 所有可选组组合的保守上界；不允许有限字段相乘后变为 Inf。
	for _, key := range append([]string{"input", "output"}, priceExtraKeys()...) {
		prompt := key == "input" || ExtraKeyIsPrompt[key]
		product := 1.0
		for _, group := range r.groups() {
			largest := 1.0
			for _, rule := range group.rules {
				largest = math.Max(largest, rule.Multipliers.For(key, prompt))
			}
			product *= largest
		}
		if math.IsInf(product, 0) {
			return errors.New("combined rate multipliers overflow")
		}
	}
	return nil
}

func priceExtraKeys() []string {
	keys := make([]string, 0, len(ExtraKeyIsPrompt))
	for key := range ExtraKeyIsPrompt {
		if key == config.UsageExtraInputAudioTranscription {
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func priceMinute(value string) (int, error) {
	t, err := time.Parse("15:04", value)
	if err != nil || len(value) != 5 {
		return 0, errors.New("invalid HH:mm")
	}
	return t.Hour()*60 + t.Minute(), nil
}

// 每个分组只解释其职责内的条件。长度可限制适用速度，避免 Fast 重复加价。
func validatePriceRuleGroupCondition(group string, w PriceRuleCondition) error {
	switch group {
	case "service_tier":
		if len(w.ServiceTier) == 0 || w.Speed != nil || w.InputTokens != nil || w.Weekdays != nil || w.Time != nil {
			return errors.New("service_tier rules require only service_tier values")
		}
	case "speed":
		if len(w.Speed) == 0 || w.ServiceTier != nil || w.InputTokens != nil || w.Weekdays != nil || w.Time != nil {
			return errors.New("speed rules require only speed values")
		}
	case "long_context":
		if w.InputTokens == nil || w.ServiceTier != nil || w.Weekdays != nil || w.Time != nil {
			return errors.New("long_context rules require input_tokens and optionally applicable speed values")
		}
	case "schedule":
		if w.ServiceTier != nil || w.Speed != nil || w.InputTokens != nil {
			return errors.New("schedule rules allow only weekdays and time")
		}
	}
	return nil
}

func priceConditionsOverlap(group string, a, b PriceRuleCondition) bool {
	intersects := func(left, right []string) bool {
		if len(left) == 0 || len(right) == 0 {
			return true
		}
		for _, value := range left {
			if slices.Contains(right, value) {
				return true
			}
		}
		return false
	}
	switch group {
	case "service_tier":
		return intersects(a.ServiceTier, b.ServiceTier)
	case "speed":
		return intersects(a.Speed, b.Speed)
	case "long_context":
		if a.InputTokens == nil || b.InputTokens == nil || !intersects(a.Speed, b.Speed) {
			return false
		}
		lower, upper := -1, math.MaxInt
		for _, interval := range []*PriceTokenRange{a.InputTokens, b.InputTokens} {
			if interval.GT != nil {
				lower = max(lower, *interval.GT)
			}
			if interval.LTE != nil {
				upper = min(upper, *interval.LTE)
			}
		}
		return lower < upper
	}
	return false
}

// Evaluate 仅处理有限条件，不读取请求 JSON、系统当前时间或 provider 类型。
func (r PriceRateRules) Evaluate(f PriceRuleFacts) PriceRuleResult {
	result := PriceRuleResult{Input: 1, Output: 1, Extra: map[string]float64{}, Matches: []PriceRuleMatch{}}
	keys := priceExtraKeys()
	for _, key := range keys {
		result.Extra[key] = 1
	}
	if err := validatePriceRateRules(r); err != nil {
		result.Missing = true
		result.Diagnostics = []string{"billing_rate_rules_invalid"}
		return result
	}
	loc := time.UTC
	if r.Schedule != nil && r.Schedule.Timezone != "" {
		loc, _ = time.LoadLocation(r.Schedule.Timezone)
	}
	f.ServiceTier = strings.ToLower(strings.TrimSpace(f.ServiceTier))
	if f.ServiceTier == "" || f.ServiceTier == "auto" {
		f.ServiceTier = "default"
	}
	for _, group := range r.groups() {
		var possible []PriceRateMultiplier
		var matched *PriceRateRule
		uncertain := false
		conflicted := false
		terminated := false
		for i := range group.rules {
			rule := &group.rules[i]
			state := rule.When.match(f, loc)
			if state == 0 {
				continue
			}
			possible = append(possible, rule.Multipliers)
			if state == 1 {
				matched = rule
				terminated = true
				break
			}
			uncertain = true
			conflicted = conflicted || (len(rule.When.Speed) > 0 && f.SpeedConflict)
		}
		if !terminated {
			possible = append(possible, PriceRateMultiplier{})
		}
		chosen := possible[0]
		if uncertain {
			for _, other := range possible[1:] {
				if !sameRateMultipliers(chosen, other, keys) {
					result.Missing = !conflicted
					result.Conflict = conflicted
					result.Diagnostics = append(result.Diagnostics, "billing_rule_evidence_missing:"+group.name)
					return result
				}
			}
			// 能确定倍率但无法确定唯一规则，不伪造命中记录。
			matched = nil
		}
		result.Input *= chosen.For("", true)
		result.Output *= chosen.For("", false)
		for _, key := range keys {
			result.Extra[key] *= chosen.For(key, ExtraKeyIsPrompt[key])
		}
		if matched != nil {
			extra := map[string]float64{}
			for key := range matched.Multipliers.Extra {
				extra[key] = chosen.For(key, ExtraKeyIsPrompt[key])
			}
			result.Matches = append(result.Matches, PriceRuleMatch{Kind: group.name, ID: matched.ID, When: matched.When, InputMultiplier: chosen.For("", true), OutputMultiplier: chosen.For("", false), ExtraMultipliers: extra})
		}
	}
	if !slices.Contains([]string{"default", "flex", "fast", "priority"}, f.ServiceTier) {
		result.Diagnostics = append(result.Diagnostics, "billing_tier_unknown")
	}
	if f.Speed != "" && f.Speed != "fast" && f.Speed != "standard" {
		result.Diagnostics = append(result.Diagnostics, "billing_speed_unknown")
	}
	return result
}

func sameRateMultipliers(a, b PriceRateMultiplier, keys []string) bool {
	if a.For("", true) != b.For("", true) || a.For("", false) != b.For("", false) {
		return false
	}
	for _, key := range keys {
		if a.For(key, ExtraKeyIsPrompt[key]) != b.For(key, ExtraKeyIsPrompt[key]) {
			return false
		}
	}
	return true
}

// 0 = 确定不匹配，1 = 匹配，2 = 缺失／冲突；已知 false 优先于未知。
func (w PriceRuleCondition) match(f PriceRuleFacts, loc *time.Location) int {
	unknown := false
	if len(w.ServiceTier) > 0 && !slices.Contains(w.ServiceTier, f.ServiceTier) {
		return 0
	}
	if len(w.Speed) > 0 {
		if f.Speed == "" || f.SpeedConflict {
			unknown = true
		} else if !slices.Contains(w.Speed, f.Speed) {
			return 0
		}
	}
	if w.InputTokens != nil {
		if f.InputTokens == nil {
			unknown = true
		} else if w.InputTokens.GT != nil && *f.InputTokens <= *w.InputTokens.GT || w.InputTokens.LTE != nil && *f.InputTokens > *w.InputTokens.LTE {
			return 0
		}
	}
	if len(w.Weekdays) > 0 || w.Time != nil {
		if f.StartedAt.IsZero() {
			unknown = true
		} else {
			local := f.StartedAt.In(loc)
			weekday := int(local.Weekday())
			if weekday == 0 {
				weekday = 7
			}
			if len(w.Weekdays) > 0 && !slices.Contains(w.Weekdays, weekday) {
				return 0
			}
			if w.Time != nil {
				start, _ := priceMinute(w.Time.Start)
				end, _ := priceMinute(w.Time.End)
				minute := local.Hour()*60 + local.Minute()
				inside := minute >= start && minute < end
				if start > end {
					inside = minute >= start || minute < end
				}
				if !inside {
					return 0
				}
			}
		}
	}
	if unknown {
		return 2
	}
	return 1
}
