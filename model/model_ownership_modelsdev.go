package model

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"unicode"
)

type ModelOwnershipSuggestion struct {
	Model  string   `json:"model"`
	Labs   []string `json:"labs"`
	Status string   `json:"status"`
}

// FetchModelsDevOwnershipSuggestions 只提供外部作者建议，不决定本地展示分类。
func FetchModelsDevOwnershipSuggestions(ctx context.Context, modelNames []string) ([]ModelOwnershipSuggestion, error) {
	return fetchModelsDevOwnershipSuggestions(ctx, newModelsDevClient(), modelNames)
}

func fetchModelsDevOwnershipSuggestions(ctx context.Context, client *http.Client, modelNames []string) ([]ModelOwnershipSuggestion, error) {
	models, err := ValidateModelOwnershipModels(modelNames)
	if err != nil {
		return nil, err
	}
	upstream, err := fetchModelsDevSource(ctx, client)
	if err != nil {
		return nil, err
	}
	return modelsDevOwnershipSuggestions(upstream, models), nil
}

func modelsDevOwnershipSuggestions(upstream modelsDevSource, models []string) []ModelOwnershipSuggestion {
	labsByModel := make(map[string]map[string]struct{}, len(models))
	for _, name := range models {
		labsByModel[name] = make(map[string]struct{})
	}
	for _, provider := range upstream {
		for name, item := range provider.Models {
			labs, selected := labsByModel[name]
			if !selected {
				continue
			}
			if lab, ok := modelsDevCanonicalLab(item.CanonicalModelID); ok {
				labs[lab] = struct{}{}
			}
		}
	}
	result := make([]ModelOwnershipSuggestion, 0, len(models))
	for _, name := range models {
		item := ModelOwnershipSuggestion{Model: name, Labs: []string{}, Status: "unmatched"}
		for lab := range labsByModel[name] {
			item.Labs = append(item.Labs, lab)
		}
		sort.Strings(item.Labs)
		switch len(item.Labs) {
		case 0:
		case 1:
			item.Status = "matched"
		default:
			item.Status = "conflict"
		}
		result = append(result, item)
	}
	return result
}

func modelsDevCanonicalLab(canonical string) (string, bool) {
	lab, modelName, found := strings.Cut(canonical, "/")
	if !found || lab == "" || modelName == "" || strings.Contains(canonical, "*") ||
		strings.IndexFunc(canonical, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", false
	}
	return lab, true
}
