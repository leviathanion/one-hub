package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

const DefaultRetryStatusCodes = "307,401,402,403,408,429,500,502,503,504"

type retryStatusCodePolicy struct {
	exact    map[int]struct{}
	families map[int]struct{}
}

var retryStatusCodePolicyValue atomic.Value

func init() {
	_ = SetRetryStatusCodes(DefaultRetryStatusCodes)
}

func SetRetryStatusCodes(value string) error {
	canonical, policy, err := parseRetryStatusCodePolicy(value)
	if err != nil {
		return err
	}
	RetryStatusCodes = canonical
	retryStatusCodePolicyValue.Store(policy)
	return nil
}

func ValidateRetryStatusCodes(value string) error {
	_, _, err := parseRetryStatusCodePolicy(value)
	return err
}

func RetryStatusCodeIsRetryable(status int) bool {
	if status < 100 || status > 599 {
		return false
	}
	loaded := retryStatusCodePolicyValue.Load()
	if loaded == nil {
		_ = SetRetryStatusCodes(RetryStatusCodes)
		loaded = retryStatusCodePolicyValue.Load()
	}
	policy, ok := loaded.(retryStatusCodePolicy)
	if !ok {
		return false
	}
	if _, exists := policy.exact[status]; exists {
		return true
	}
	_, exists := policy.families[status/100]
	return exists
}

func parseRetryStatusCodePolicy(value string) (string, retryStatusCodePolicy, error) {
	policy := retryStatusCodePolicy{
		exact:    make(map[int]struct{}),
		families: make(map[int]struct{}),
	}
	tokens := splitRetryStatusCodeTokens(value)
	for _, token := range tokens {
		family, isFamily, err := parseRetryStatusFamilyToken(token)
		if err != nil {
			return "", retryStatusCodePolicy{}, err
		}
		if isFamily {
			policy.families[family] = struct{}{}
			continue
		}
		status, err := strconv.Atoi(token)
		if err != nil || status < 100 || status > 599 {
			return "", retryStatusCodePolicy{}, fmt.Errorf("重试状态码 %q 无效，必须是 100-599 的 HTTP 状态码或 1xx-5xx 范围", token)
		}
		policy.exact[status] = struct{}{}
	}
	return canonicalRetryStatusCodes(policy), policy, nil
}

func splitRetryStatusCodeTokens(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ',', ';', '，', '、', '\n', '\r', '\t', ' ':
			return true
		default:
			return false
		}
	})
	tokens := make([]string, 0, len(fields))
	for _, field := range fields {
		token := strings.ToLower(strings.TrimSpace(field))
		if token == "" {
			continue
		}
		tokens = append(tokens, token)
	}
	return tokens
}

func parseRetryStatusFamilyToken(token string) (int, bool, error) {
	if len(token) != 3 || !strings.HasSuffix(token, "xx") {
		return 0, false, nil
	}
	if token[0] < '1' || token[0] > '5' {
		return 0, false, fmt.Errorf("重试状态码范围 %q 无效，必须是 1xx-5xx", token)
	}
	return int(token[0] - '0'), true, nil
}

func canonicalRetryStatusCodes(policy retryStatusCodePolicy) string {
	parts := make([]string, 0, len(policy.exact)+len(policy.families))

	exact := make([]int, 0, len(policy.exact))
	for status := range policy.exact {
		if _, covered := policy.families[status/100]; covered {
			continue
		}
		exact = append(exact, status)
	}
	sort.Ints(exact)
	for _, status := range exact {
		parts = append(parts, strconv.Itoa(status))
	}

	families := make([]int, 0, len(policy.families))
	for family := range policy.families {
		families = append(families, family)
	}
	sort.Ints(families)
	for _, family := range families {
		parts = append(parts, strconv.Itoa(family)+"xx")
	}

	return strings.Join(parts, ",")
}
