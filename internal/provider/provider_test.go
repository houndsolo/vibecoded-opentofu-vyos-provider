package provider

import (
	"context"
	"testing"
	"time"

	frameworkprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func configuredSettings(t *testing.T, model providerModel) *settings {
	t.Helper()
	ctx := context.Background()
	p := &Provider{}
	var schema frameworkprovider.SchemaResponse
	p.Schema(ctx, frameworkprovider.SchemaRequest{}, &schema)
	state := tfsdk.State{Schema: schema.Schema}
	if d := state.Set(ctx, &model); d.HasError() {
		t.Fatal(d)
	}
	var response frameworkprovider.ConfigureResponse
	p.Configure(ctx, frameworkprovider.ConfigureRequest{Config: tfsdk.Config{Schema: schema.Schema, Raw: state.Raw}}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	return response.ResourceData.(*settings)
}

func TestProviderUnknownValuesDoNotUseEnvironment(t *testing.T) {
	// These environment values belong to another router. Explicit unknown
	// configuration must neither validate nor use either fallback.
	t.Setenv("VYOS_ENDPOINT", "invalid-env-endpoint")
	t.Setenv("VYOS_API_KEY", "wrong-env-key")
	s := configuredSettings(t, providerModel{Endpoint: types.StringUnknown(), APIKey: types.StringUnknown()})
	if !s.endpointUnknown || !s.unknown || s.endpoint != "" || s.apiKey != "" {
		t.Fatal("unknown explicit values used environment credentials or endpoint")
	}
}

func TestProviderEnvironmentAndUnknownDefault(t *testing.T) {
	t.Setenv("VYOS_ENDPOINT", "https://ROUTER:443/")
	t.Setenv("VYOS_API_KEY", "test-api-key")
	s := configuredSettings(t, providerModel{})
	if s.endpoint != "https://router" || s.apiKey != "test-api-key" || s.unknown || s.endpointUnknown {
		t.Fatal("null settings did not use environment defaults")
	}
	s = configuredSettings(t, providerModel{Endpoint: types.StringUnknown(), PlanTimeout: types.StringUnknown()})
	if s.unknown || !s.endpointUnknown || s.endpoint != "" || s.planTimeout != 5*time.Second {
		t.Fatal("unknown preview/default settings blocked known resource connections")
	}
}
