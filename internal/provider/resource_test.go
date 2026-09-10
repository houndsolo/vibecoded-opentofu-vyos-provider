package provider

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
)

func resourceFixture(endpoint string) *commandsResource {
	return &commandsResource{settings: &settings{endpoint: endpoint, apiKey: "test-api-key", requestTimeout: time.Second, planTimeout: 50 * time.Millisecond}}
}
func resourceSchema() schema.Schema {
	var resp resource.SchemaResponse
	NewCommandsResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	return resp.Schema
}
func nullState() tfsdk.State {
	s := resourceSchema()
	return tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}
}
func modelState(t *testing.T, model commandsModel) tfsdk.State {
	t.Helper()
	s := nullState()
	if d := s.Set(context.Background(), &model); d.HasError() {
		t.Fatal(d)
	}
	return s
}
func testModel(endpoint string, raw ...string) commandsModel {
	return commandsModel{Name: types.StringValue("leaf-11"), Endpoint: types.StringValue(endpoint), Commands: stringSet(raw), Save: types.BoolValue(true), ID: types.StringUnknown(), Managed: managedSet(raw), InSync: types.BoolValue(true), PendingSave: types.BoolValue(false)}
}
func createFixture(t *testing.T, r *commandsResource, model commandsModel) tfsdk.State {
	t.Helper()
	s := modelState(t, model)
	resp := resource.CreateResponse{State: nullState()}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: s.Schema, Raw: s.Raw}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.State
}
func readModel(t *testing.T, state tfsdk.State) commandsModel {
	t.Helper()
	var model commandsModel
	if d := state.Get(context.Background(), &model); d.HasError() {
		t.Fatal(d)
	}
	return model
}

func TestResourceLifecycleAndDrift(t *testing.T) {
	ctx := context.Background()
	f := newFakeAPI(t, dummy(map[string]any{"description": "KEEP"}), dummy(map[string]any{"description": "KEEP", "mtu": "1400"}))
	r := resourceFixture(f.server.URL)
	state := createFixture(t, r, testModel(f.server.URL, "set interfaces dummy dum99 mtu 1400", "delete protocols ospf"))
	model := readModel(t, state)
	id := model.ID
	if !model.InSync.ValueBool() || id.IsUnknown() || len(f.batches) != 1 || f.reads != 2 || f.saves != 1 {
		t.Fatal("create was not committed and verified in one batch")
	}
	// Refresh detects changed SET values without erasing desired state/ownership.
	f.running = dummy(map[string]any{"description": "KEEP", "mtu": "1420"})
	read := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	drift := readModel(t, read.State)
	if drift.InSync.ValueBool() || !drift.Managed.Equal(model.Managed) || !drift.Commands.Equal(model.Commands) {
		t.Fatal("drift lost ownership or was not detected")
	}
	f.running = dummy(map[string]any{"description": "KEEP", "mtu": "1400"})
	f.after = dummy(map[string]any{"description": "KEEP", "mtu": "1450"})
	plan := testModel(f.server.URL, "set interfaces dummy dum99 mtu 1450", "delete protocols ospf")
	plan.ID = id
	ps := modelState(t, plan)
	update := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{State: state, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}, &update)
	if update.Diagnostics.HasError() {
		t.Fatal(update.Diagnostics)
	}
	if len(f.batches) != 2 || !reflect.DeepEqual(opStrings(f.batches[1]), []string{"delete interfaces dummy dum99 mtu", "set interfaces dummy dum99 mtu 1450"}) {
		t.Fatal(f.batches)
	}
	if !readModel(t, update.State).ID.Equal(id) {
		t.Fatal("ID changed on update")
	}
	// An absence assertion must detect a recreated subtree.
	f.running["protocols"] = map[string]any{"ospf": map[string]any{}}
	read = resource.ReadResponse{State: update.State}
	r.Read(ctx, resource.ReadRequest{State: update.State}, &read)
	if read.Diagnostics.HasError() || readModel(t, read.State).InSync.ValueBool() {
		t.Fatal("absence drift not detected")
	}
	f.after = dummy(map[string]any{"description": "KEEP"})
	f.after["protocols"] = map[string]any{"ospf": map[string]any{}}
	destroy := resource.DeleteResponse{State: update.State}
	r.Delete(ctx, resource.DeleteRequest{State: update.State}, &destroy)
	if destroy.Diagnostics.HasError() {
		t.Fatal(destroy.Diagnostics)
	}
	if !destroy.State.Raw.IsNull() || len(f.batches) != 3 || !reflect.DeepEqual(opStrings(f.batches[2]), []string{"delete interfaces dummy dum99 mtu"}) {
		t.Fatal("destroy affected unmanaged or assertion configuration")
	}
}

func TestSaveFailureSurvivesRefresh(t *testing.T) {
	f := newFakeAPI(t, map[string]any{}, dummy(map[string]any{"mtu": "1400"}))
	f.failSave = true
	r := resourceFixture(f.server.URL)
	ps := modelState(t, testModel(f.server.URL, "set interfaces dummy dum99 mtu 1400"))
	create := resource.CreateResponse{State: nullState()}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}, &create)
	if !create.Diagnostics.HasError() || create.State.Raw.IsNull() {
		t.Fatal("failed save lost state")
	}
	read := resource.ReadResponse{State: create.State}
	r.Read(context.Background(), resource.ReadRequest{State: create.State}, &read)
	model := readModel(t, read.State)
	if read.Diagnostics.HasError() || model.InSync.ValueBool() || !model.PendingSave.ValueBool() {
		t.Fatal("save retry was lost on refresh")
	}
	f.failSave = false
	model.InSync = types.BoolValue(true)
	model.PendingSave = types.BoolValue(false)
	ps = modelState(t, model)
	update := resource.UpdateResponse{State: read.State}
	r.Update(context.Background(), resource.UpdateRequest{State: read.State, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}, &update)
	if update.Diagnostics.HasError() || len(f.batches) != 1 || f.saves != 2 {
		t.Fatalf("save retry replayed configure: %v", update.Diagnostics)
	}
}

func TestUncertainCommitRetainsOwnership(t *testing.T) {
	f := newFakeAPI(t, dummy(map[string]any{"mtu": "1400", "description": "KEEP"}), dummy(map[string]any{"mtu": "1450", "description": "KEEP"}))
	f.failConfigure = true
	f.commitThenFail = true
	r := resourceFixture(f.server.URL)
	old := testModel(f.server.URL, "set interfaces dummy dum99 mtu 1400")
	old.ID = types.StringValue("stable-id")
	plan := testModel(f.server.URL, "set interfaces dummy dum99 mtu 1450")
	plan.ID = old.ID
	os, ps := modelState(t, old), modelState(t, plan)
	resp := resource.UpdateResponse{State: os}
	r.Update(context.Background(), resource.UpdateRequest{State: os, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}, &resp)
	if !resp.Diagnostics.HasError() || len(readModel(t, resp.State).Managed.Elements()) != 2 {
		t.Fatal("uncertain commit lost old or attempted ownership")
	}
	read := resource.ReadResponse{State: resp.State}
	r.Read(context.Background(), resource.ReadRequest{State: resp.State}, &read)
	if read.Diagnostics.HasError() || readModel(t, read.State).InSync.ValueBool() {
		t.Fatal("uncertain save should trigger update")
	}
	f.failConfigure = false
	update := resource.UpdateResponse{State: read.State}
	r.Update(context.Background(), resource.UpdateRequest{State: read.State, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}, &update)
	if update.Diagnostics.HasError() || len(f.batches) != 1 || !readModel(t, update.State).Managed.Equal(plan.Managed) {
		t.Fatalf("recovery did not reconcile: %v", update.Diagnostics)
	}
}

func TestModifyPlanBestEffortAndLogs(t *testing.T) {
	f := newFakeAPI(t, dummy(map[string]any{"mtu": "1400"}), nil)
	r := resourceFixture(f.server.URL)
	old := testModel(f.server.URL, "set interfaces dummy dum99 mtu 1400")
	old.ID = types.StringValue("stable")
	plan := testModel(f.server.URL, "set interfaces dummy dum99 mtu 1450")
	plan.ID = old.ID
	os, ps := modelState(t, old), modelState(t, plan)
	var output bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &output)
	req := resource.ModifyPlanRequest{State: os, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}, Config: tfsdk.Config{Schema: ps.Schema, Raw: ps.Raw}}
	resp := resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(ctx, req, &resp)
	if resp.Diagnostics.HasError() || len(f.batches) != 0 || f.saves != 0 || f.reads != 1 {
		t.Fatalf("plan mutated the router: %v", resp.Diagnostics)
	}
	for _, text := range []string{"VyOS operation batch", `"phase":"plan"`, "DELETE interfaces dummy dum99 mtu", "SET interfaces dummy dum99 mtu 1450", "Resolved VyOS delete path"} {
		if !strings.Contains(output.String(), text) {
			t.Errorf("missing log %q: %s", text, output.String())
		}
	}
	f.server.Close()
	output.Reset()
	resp = resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(ctx, req, &resp)
	if resp.Diagnostics.HasError() || !strings.Contains(output.String(), "preview unavailable") {
		t.Fatalf("unreachable router failed plan: %v", resp.Diagnostics)
	}
	// Unknown endpoint and commands during VM creation must also plan.
	plan.Endpoint = types.StringUnknown()
	plan.Commands = types.SetUnknown(types.StringType)
	ps = modelState(t, plan)
	req.State = nullState()
	req.Config.Raw = ps.Raw
	req.Plan.Raw = ps.Raw
	resp = resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(ctx, req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
}

func TestCredentialLogging(t *testing.T) {
	var output bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &output)
	batch := Batch{Operations: commandsFixture(t, []string{"set service https api keys id terraform key test-api-key", "set system login user test authentication plaintext-password private-password"})}
	logBatch(ctx, batch, "plan", "create", "leaf-11", "test-api-key")
	if strings.Contains(output.String(), "test-api-key") || strings.Contains(output.String(), "private-password") || !strings.Contains(output.String(), "REDACTED") {
		t.Fatal("credential logging is unsafe")
	}
}

func TestUnknownProviderEndpointAllowsKnownResource(t *testing.T) {
	f := newFakeAPI(t, dummy(map[string]any{"mtu": "1400"}), map[string]any{})
	r := &commandsResource{settings: configuredSettings(t, providerModel{
		Endpoint: types.StringUnknown(), APIKey: types.StringValue("test-api-key"),
		PlanTimeout: types.StringUnknown(),
	})}
	state := createFixture(t, r, testModel(f.server.URL, "set interfaces dummy dum99 mtu 1400"))
	read := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &read)
	if read.Diagnostics.HasError() || !readModel(t, read.State).InSync.ValueBool() {
		t.Fatalf("unknown default blocked refresh: %v", read.Diagnostics)
	}
	destroy := resource.DeleteResponse{State: state}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, &destroy)
	if destroy.Diagnostics.HasError() || !destroy.State.Raw.IsNull() {
		t.Fatalf("unknown default blocked stored-target destroy: %v", destroy.Diagnostics)
	}
}

func TestModifyPlanEndpointChanges(t *testing.T) {
	for _, tc := range []struct {
		name                                                string
		old, configured, providerEndpoint                   string
		unknownDefault, wantReplace, wantUnknown, wantError bool
	}{
		{name: "cosmetic", old: "https://ROUTER:443/", configured: "https://router"},
		{name: "new target", old: "https://old", configured: "https://new", wantReplace: true},
		{name: "new proxy route", old: "https://router/leaf%2F1", configured: "https://router/leaf/1", wantReplace: true},
		{name: "inherited default", providerEndpoint: "https://router"},
		{name: "missing endpoint", wantError: true},
		{name: "unknown inherited default", unknownDefault: true, wantUnknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := resourceFixture(tc.providerEndpoint)
			r.settings.endpointUnknown = tc.unknownDefault
			// This test checks planning without needing a reachable router.
			r.settings.planTimeout = time.Nanosecond
			model := testModel(tc.configured, "set system host-name leaf")
			model.ID = types.StringValue("stable")
			if tc.configured == "" {
				model.Endpoint = types.StringNull()
			}
			ps := modelState(t, model)
			old := nullState()
			if tc.old != "" {
				prior := model
				prior.Endpoint = types.StringValue(tc.old)
				old = modelState(t, prior)
			}
			req := resource.ModifyPlanRequest{State: old, Config: tfsdk.Config{Schema: ps.Schema, Raw: ps.Raw}, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}
			resp := resource.ModifyPlanResponse{Plan: req.Plan}
			r.ModifyPlan(context.Background(), req, &resp)
			if resp.Diagnostics.HasError() != tc.wantError || (len(resp.RequiresReplace) > 0) != tc.wantReplace {
				t.Fatalf("replacement=%v diagnostics=%v", resp.RequiresReplace, resp.Diagnostics)
			}
			if tc.wantError {
				return
			}
			planned := readModel(t, tfsdk.State{Schema: ps.Schema, Raw: resp.Plan.Raw})
			if planned.Endpoint.IsUnknown() != tc.wantUnknown || !planned.ID.Equal(model.ID) {
				t.Fatal("endpoint knowledge or resource ID changed")
			}
			if tc.configured != "" && planned.Endpoint.ValueString() != tc.configured {
				t.Fatal("plan changed a configured attribute instead of preserving its literal value")
			}
		})
	}
}

func TestUpdateCannotTransferOwnershipToAnotherRouter(t *testing.T) {
	f := newFakeAPI(t, dummy(map[string]any{"mtu": "1400"}), nil)
	r := resourceFixture(f.server.URL)
	old := modelState(t, testModel("https://old-router", "set interfaces dummy dum99 mtu 1400"))
	ps := modelState(t, testModel(f.server.URL))
	resp := resource.UpdateResponse{State: old}
	r.Update(context.Background(), resource.UpdateRequest{State: old, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}, &resp)
	if !resp.Diagnostics.HasError() || f.reads != 0 || len(f.batches) != 0 || !resp.State.Raw.Equal(old.Raw) {
		t.Fatal("update reused another router's ownership")
	}
	req := resource.ModifyPlanRequest{State: old, Config: tfsdk.Config{Schema: ps.Schema, Raw: ps.Raw}, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}
	preview := resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(context.Background(), req, &preview)
	if preview.Diagnostics.HasError() || len(preview.RequiresReplace) == 0 || f.reads != 0 {
		t.Fatal("replacement preview inspected new router with old ownership")
	}
}

func TestAmbiguousSnapshotStopsApplyBeforeMutation(t *testing.T) {
	raw := `set interfaces dummy dum99 description 'hello\nworld'`
	f := newFakeAPI(t, dummy(map[string]any{"description": `hello\nworld`}), map[string]any{})
	r := resourceFixture(f.server.URL)
	old := modelState(t, testModel(f.server.URL, raw))
	ps := modelState(t, testModel(f.server.URL))
	resp := resource.UpdateResponse{State: old}
	r.Update(context.Background(), resource.UpdateRequest{State: old, Plan: tfsdk.Plan{Schema: ps.Schema, Raw: ps.Raw}}, &resp)
	if !resp.Diagnostics.HasError() || len(f.batches) != 0 || f.saves != 0 || !resp.State.Raw.Equal(old.Raw) {
		t.Fatal("ambiguous snapshot allowed mutation or changed ownership state")
	}
}
