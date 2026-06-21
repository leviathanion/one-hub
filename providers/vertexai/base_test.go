package vertexai

import (
	"testing"

	"one-api/model"
	"one-api/providers/base"
)

func TestVertexAIKeyConfigReadsJSONOther(t *testing.T) {
	provider := &VertexAIProvider{
		BaseProvider: base.BaseProvider{
			Channel: &model.Channel{Other: `{"region":"us-central1","project_id":"project-a"}`},
		},
	}

	getKeyConfig(provider)
	if provider.Region != "us-central1" || provider.ProjectID != "project-a" {
		t.Fatalf("expected JSON Other region/project_id, got region=%q project=%q", provider.Region, provider.ProjectID)
	}
}
