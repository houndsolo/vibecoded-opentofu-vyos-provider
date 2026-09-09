package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

func dynamic(t *testing.T, value any) *tfprotov6.DynamicValue {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return &tfprotov6.DynamicValue{JSON: raw}
}
func noProtocolErrors(t *testing.T, diagnostics []*tfprotov6.Diagnostic, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("%s: %s", d.Summary, d.Detail)
		}
	}
}

// Exercise Framework schema validation, defaulting, planning, state conversion,
// and apply through the actual protocol adapter without requiring plugin sockets.
func TestProtocolLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFakeAPI(t, dummy(map[string]any{"mtu": "1400"}), dummy(map[string]any{"mtu": "1400"}))
	server := providerserver.NewProtocol6(New("0.1.0")())()
	schema, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	noProtocolErrors(t, schema.Diagnostics, nil)
	config := dynamic(t, map[string]any{"endpoint": f.server.URL, "api_key": "test-api-key", "insecure": nil, "request_timeout": "1s", "plan_timeout": "50ms"})
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: config, TerraformVersion: "1.11.0"})
	if err != nil {
		t.Fatal(err)
	}
	noProtocolErrors(t, configured.Diagnostics, nil)
	resourceConfig := dynamic(t, map[string]any{
		"name": "leaf-11", "endpoint": nil, "commands": []string{"set interfaces dummy dum99 mtu 1400", "delete protocols ospf"},
		"save": nil, "id": nil, "managed_commands": nil, "in_sync": nil, "pending_save": nil,
	})
	valid, err := server.ValidateResourceConfig(ctx, &tfprotov6.ValidateResourceConfigRequest{TypeName: "vyoscmd_commands", Config: resourceConfig})
	if err != nil {
		t.Fatal(err)
	}
	noProtocolErrors(t, valid.Diagnostics, nil)
	plan, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{TypeName: "vyoscmd_commands", Config: resourceConfig, PriorState: dynamic(t, nil), ProposedNewState: resourceConfig})
	if err != nil {
		t.Fatal(err)
	}
	noProtocolErrors(t, plan.Diagnostics, nil)
	result, err := server.ApplyResourceChange(ctx, &tfprotov6.ApplyResourceChangeRequest{TypeName: "vyoscmd_commands", Config: resourceConfig, PriorState: dynamic(t, nil), PlannedState: plan.PlannedState, PlannedPrivate: plan.PlannedPrivate})
	if err != nil {
		t.Fatal(err)
	}
	noProtocolErrors(t, result.Diagnostics, nil)
	if len(f.batches) != 0 || f.saves != 1 {
		t.Fatal("satisfied create replayed configuration or missed default save")
	}
	f.running = dummy(map[string]any{"mtu": "1420"})
	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: "vyoscmd_commands", CurrentState: result.NewState, Private: result.Private})
	if err != nil {
		t.Fatal(err)
	}
	noProtocolErrors(t, read.Diagnostics, nil)
	raw, err := read.NewState.Unmarshal(resourceSchema().Type().TerraformType(ctx))
	if err != nil {
		t.Fatal(err)
	}
	model := readModel(t, tfsdk.State{Raw: raw, Schema: resourceSchema()})
	if model.InSync.ValueBool() || model.Endpoint.ValueString() != f.server.URL {
		t.Fatal("framework lost drift or inherited endpoint")
	}
	plan, err = server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{TypeName: "vyoscmd_commands", Config: resourceConfig, PriorState: read.NewState, ProposedNewState: read.NewState, PriorPrivate: read.Private})
	if err != nil {
		t.Fatal(err)
	}
	noProtocolErrors(t, plan.Diagnostics, nil)
	result, err = server.ApplyResourceChange(ctx, &tfprotov6.ApplyResourceChangeRequest{TypeName: "vyoscmd_commands", Config: resourceConfig, PriorState: read.NewState, PlannedState: plan.PlannedState, PlannedPrivate: plan.PlannedPrivate})
	if err != nil {
		t.Fatal(err)
	}
	noProtocolErrors(t, result.Diagnostics, nil)
	if len(f.batches) != 1 || len(f.batches[0]) != 1 || f.saves != 2 {
		t.Fatal("framework drift repair did not converge")
	}
}
