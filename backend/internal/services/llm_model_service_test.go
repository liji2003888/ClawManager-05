package services

import (
	"encoding/json"
	"testing"

	"clawreef/internal/models"
)

func TestBuildProviderEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		baseURL       string
		versionPrefix string
		resource      string
		want          string
		wantErr       bool
	}{
		{
			name:          "appends default version when base url has no version suffix",
			baseURL:       "https://api.deepseek.com",
			versionPrefix: "v1",
			resource:      "models",
			want:          "https://api.deepseek.com/v1/models",
		},
		{
			name:          "keeps matching version suffix",
			baseURL:       "https://api.openai.com/v1",
			versionPrefix: "v1",
			resource:      "models",
			want:          "https://api.openai.com/v1/models",
		},
		{
			name:          "respects nonstandard version suffix",
			baseURL:       "https://open.bigmodel.cn/api/paas/v4",
			versionPrefix: "v1",
			resource:      "models",
			want:          "https://open.bigmodel.cn/api/paas/v4/models",
		},
		{
			name:          "keeps beta version suffix",
			baseURL:       "https://generativelanguage.googleapis.com/v1beta",
			versionPrefix: "v1beta",
			resource:      "models",
			want:          "https://generativelanguage.googleapis.com/v1beta/models",
		},
		{
			name:          "rejects invalid base url",
			baseURL:       "not-a-url",
			versionPrefix: "v1",
			resource:      "models",
			wantErr:       true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := buildProviderEndpoint(tc.baseURL, tc.versionPrefix, tc.resource)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got none and endpoint %q", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}

			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestNormalizeLLMModelCustomHeaders(t *testing.T) {
	raw, err := normalizeLLMModelCustomHeaders([]models.LLMModelCustomHeader{
		{Key: " x-tenant-id ", Value: " {{env.TENANT_ID}} "},
		{Key: "X-Trace", Value: "{{request.trace_id}}"},
		{Key: "", Value: ""},
	})
	if err != nil {
		t.Fatalf("normalizeLLMModelCustomHeaders returned error: %v", err)
	}
	if raw == nil {
		t.Fatalf("expected custom headers to be encoded")
	}

	var decoded []models.LLMModelCustomHeader
	if err := json.Unmarshal([]byte(*raw), &decoded); err != nil {
		t.Fatalf("failed to decode custom headers: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("expected 2 normalized headers, got %d", len(decoded))
	}
	if decoded[0].Key != "X-Tenant-Id" || decoded[0].Value != "{{env.TENANT_ID}}" {
		t.Fatalf("unexpected first header: %#v", decoded[0])
	}
}

func TestNormalizeLLMModelCustomHeadersRejectsInvalidRows(t *testing.T) {
	tests := []struct {
		name    string
		headers []models.LLMModelCustomHeader
	}{
		{
			name: "missing value",
			headers: []models.LLMModelCustomHeader{
				{Key: "X-Tenant-ID", Value: ""},
			},
		},
		{
			name: "duplicate key",
			headers: []models.LLMModelCustomHeader{
				{Key: "X-Tenant-ID", Value: "a"},
				{Key: "x-tenant-id", Value: "b"},
			},
		},
		{
			name: "hop header",
			headers: []models.LLMModelCustomHeader{
				{Key: "Connection", Value: "keep-alive"},
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := normalizeLLMModelCustomHeaders(tc.headers); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}
