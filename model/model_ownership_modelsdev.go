package model

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

type ModelOwnershipProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ModelOwnershipSuggestion struct {
	Model     string                   `json:"model"`
	Providers []ModelOwnershipProvider `json:"providers"`
	Status    string                   `json:"status"`
}

// FetchModelsDevOwnershipSuggestions 按服务提供方给出建议，不决定本地展示分类。
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
	providersByModel := make(map[string][]ModelOwnershipProvider, len(models))
	for _, name := range models {
		providersByModel[name] = []ModelOwnershipProvider{}
	}
	for id, provider := range upstream {
		providerName := strings.TrimSpace(provider.Name)
		if providerName == "" {
			providerName = id
		}
		for name := range provider.Models {
			providers, selected := providersByModel[name]
			if !selected {
				continue
			}
			providersByModel[name] = append(providers, ModelOwnershipProvider{ID: id, Name: providerName})
		}
	}
	result := make([]ModelOwnershipSuggestion, 0, len(models))
	for _, name := range models {
		item := ModelOwnershipSuggestion{Model: name, Providers: providersByModel[name], Status: "unmatched"}
		sort.Slice(item.Providers, func(i, j int) bool { return item.Providers[i].ID < item.Providers[j].ID })
		switch len(item.Providers) {
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
