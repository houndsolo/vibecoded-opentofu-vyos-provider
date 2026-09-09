package provider

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

type commandsResource struct{ settings *settings }
type commandsModel struct {
	Name        types.String `tfsdk:"name"`
	Endpoint    types.String `tfsdk:"endpoint"`
	Commands    types.Set    `tfsdk:"commands"`
	Save        types.Bool   `tfsdk:"save"`
	ID          types.String `tfsdk:"id"`
	Managed     types.Set    `tfsdk:"managed_commands"`
	InSync      types.Bool   `tfsdk:"in_sync"`
	PendingSave types.Bool   `tfsdk:"pending_save"`
}

var (
	_ resource.Resource                   = &commandsResource{}
	_ resource.ResourceWithConfigure      = &commandsResource{}
	_ resource.ResourceWithValidateConfig = &commandsResource{}
	_ resource.ResourceWithModifyPlan     = &commandsResource{}
)

func NewCommandsResource() resource.Resource { return &commandsResource{} }
func (r *commandsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_commands"
}
func (r *commandsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "A set of owned VyOS SET commands and explicit absence assertions on one router.", Attributes: map[string]schema.Attribute{
		"name":             schema.StringAttribute{Required: true, Description: "Resource label used in logs."},
		"endpoint":         schema.StringAttribute{Optional: true, Computed: true, Description: "Router URL, or the provider default. Changing the resolved target replaces the resource."},
		"commands":         schema.SetAttribute{Required: true, ElementType: types.StringType, Description: "Unordered set of set/delete commands. An empty set removes previously managed SET configuration."},
		"save":             schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "Save running configuration after a successful apply. Default: true."},
		"id":               schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Stable opaque resource ID."},
		"managed_commands": schema.SetAttribute{Computed: true, ElementType: types.StringType, Description: "Ownership ledger. Normally the desired SET commands; retains attempted commands after an uncertain commit."},
		"in_sync":          schema.BoolAttribute{Computed: true, Description: "False when refresh finds drift or an incomplete save; apply reconciles it to true."},
		"pending_save":     schema.BoolAttribute{Computed: true, Description: "Retains a failed or uncertain save across refresh so a later apply retries it."},
	}}
}

func (r *commandsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	s, ok := req.ProviderData.(*settings)
	if !ok {
		resp.Diagnostics.AddError("Invalid provider configuration", "Unexpected resource client configuration.")
		return
	}
	r.settings = s
}

func setStrings(value types.Set) ([]string, bool) {
	if value.IsNull() || value.IsUnknown() {
		return nil, false
	}
	var result []string
	for _, v := range value.Elements() {
		s, ok := v.(types.String)
		if !ok || s.IsNull() || s.IsUnknown() {
			return nil, false
		}
		result = append(result, s.ValueString())
	}
	slices.Sort(result)
	return result, true
}
func stringSet(strings []string) types.Set {
	values := make([]attr.Value, 0, len(strings))
	seen := make(map[string]bool, len(strings))
	for _, s := range strings {
		if seen[s] {
			continue
		}
		seen[s] = true
		values = append(values, types.StringValue(s))
	}
	return types.SetValueMust(types.StringType, values)
}
func managedSet(commands []string) types.Set {
	var sets []string
	for _, raw := range commands {
		c, err := ParseCommand(raw)
		if err == nil && c.Op == "set" {
			sets = append(sets, raw)
		}
	}
	return stringSet(sets)
}
func ownedCommands(model commandsModel) ([]Command, error) {
	owned, known := setStrings(model.Managed)
	if !known {
		owned, _ = setStrings(model.Commands)
	}
	return ParseCommands(owned)
}

func (r *commandsResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model commandsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Validate known elements even when other set elements are unknown.
	var known []string
	for _, value := range model.Commands.Elements() {
		if s, ok := value.(types.String); ok && !s.IsUnknown() {
			if s.IsNull() {
				resp.Diagnostics.AddAttributeError(path.Root("commands"), "Invalid command", "A command must not be null.")
				return
			}
			known = append(known, s.ValueString())
		}
	}
	if _, err := ParseCommands(known); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("commands"), "Invalid commands", err.Error())
	}
	if !model.Endpoint.IsNull() && !model.Endpoint.IsUnknown() {
		if _, err := normalizeEndpoint(model.Endpoint.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid endpoint", err.Error())
		}
	}
}

func (r *commandsResource) client(model commandsModel) (*Client, error) {
	if r.settings == nil || r.settings.unknown {
		return nil, fmt.Errorf("provider connection values are not yet known")
	}
	if model.Endpoint.IsNull() || model.Endpoint.IsUnknown() {
		return nil, fmt.Errorf("set a resource endpoint or provider endpoint")
	}
	return newClient(model.Endpoint.ValueString(), r.settings.apiKey, r.settings.insecure, r.settings.requestTimeout)
}

func (r *commandsResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if r.settings != nil && r.settings.apiKey != "" {
		ctx = tflog.MaskLogStrings(ctx, r.settings.apiKey)
	}
	var old, plan commandsModel
	change := "create"
	if !req.State.Raw.IsNull() {
		resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
		change = "update"
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if req.Plan.Raw.IsNull() {
		change = "destroy"
		plan = old
	} else {
		resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
		var configured types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("endpoint"), &configured)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if configured.IsNull() {
			if r.settings != nil && r.settings.endpoint != "" {
				plan.Endpoint = types.StringValue(r.settings.endpoint)
			} else {
				plan.Endpoint = types.StringUnknown()
			}
		} else {
			plan.Endpoint = configured
		}
		if !req.State.Raw.IsNull() && !plan.Endpoint.IsUnknown() && !plan.Endpoint.Equal(old.Endpoint) {
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root("endpoint"))
		}
		if commands, known := setStrings(plan.Commands); known {
			if _, err := ParseCommands(commands); err != nil {
				resp.Diagnostics.AddError("Invalid commands", err.Error())
				return
			}
			plan.Managed = managedSet(commands)
		}
		plan.InSync = types.BoolValue(true)
		plan.PendingSave = types.BoolValue(false)
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	unavailable := func() {
		tflog.Debug(ctx, "Exact VyOS operation preview unavailable; apply will recompute", map[string]any{"phase": "plan", "change": change, "resource": plan.Name.ValueString()})
	}
	if r.settings == nil {
		unavailable()
		return
	}
	previewCtx, cancel := context.WithTimeout(ctx, r.settings.planTimeout)
	defer cancel()
	c, err := r.client(plan)
	if err != nil {
		unavailable()
		return
	}
	defer c.close()
	unlock, err := lockRouter(previewCtx, c.endpoint)
	if err != nil {
		unavailable()
		return
	}
	defer unlock()
	desiredStrings, known := setStrings(plan.Commands)
	if !known {
		unavailable()
		return
	}
	desired, err := ParseCommands(desiredStrings)
	if err != nil {
		unavailable()
		return
	}
	if change == "destroy" {
		desired = nil
	}
	owned, err := ownedCommands(old)
	if err != nil {
		unavailable()
		return
	}
	if change == "create" {
		owned = nil
	}
	snapshot, err := c.Snapshot(previewCtx)
	if err != nil {
		unavailable()
		return
	}
	batch, err := BuildBatch(snapshot, owned, desired)
	if err != nil {
		unavailable()
		return
	}
	logBatch(ctx, batch, "plan", change, plan.Name.ValueString(), c.apiKey)
}

func (r *commandsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var model commandsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	c, err := r.client(model)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read VyOS", err.Error())
		return
	}
	defer c.close()
	unlock, err := lockRouter(ctx, c.endpoint)
	if err != nil {
		resp.Diagnostics.AddError("Cannot lock router", err.Error())
		return
	}
	defer unlock()
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read VyOS", err.Error())
		return
	}
	owned, err := ownedCommands(model)
	if err != nil {
		resp.Diagnostics.AddError("Invalid ownership state", err.Error())
		return
	}
	commands, known := setStrings(model.Commands)
	if !known {
		resp.Diagnostics.AddError("Invalid command state", "The stored commands must be known.")
		return
	}
	desired, err := ParseCommands(commands)
	if err != nil {
		resp.Diagnostics.AddError("Invalid command state", err.Error())
		return
	}
	batch, err := BuildBatch(snapshot, owned, desired)
	// A safety conflict is drift too. Apply will produce the diagnostic; refresh
	// must retain ownership so users can resolve the conflict in configuration.
	model.InSync = types.BoolValue(err == nil && len(batch.Operations) == 0 && !(model.Save.ValueBool() && model.PendingSave.ValueBool()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

func (r *commandsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan commandsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, commandsModel{}, plan, "create", &resp.State, &resp.Diagnostics)
}
func (r *commandsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var old, plan commandsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, old, plan, "update", &resp.State, &resp.Diagnostics)
}
func (r *commandsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var old commandsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, old, old, "destroy", &resp.State, &resp.Diagnostics)
}

func (r *commandsResource) apply(ctx context.Context, old, plan commandsModel, change string, state *tfsdk.State, diagnostics *diag.Diagnostics) {
	c, err := r.client(plan)
	if err != nil {
		diagnostics.AddError("Cannot configure VyOS", err.Error())
		return
	}
	defer c.close()
	unlock, err := lockRouter(ctx, c.endpoint)
	if err != nil {
		diagnostics.AddError("Cannot lock router", err.Error())
		return
	}
	defer unlock()
	owned, err := ownedCommands(old)
	if err != nil {
		diagnostics.AddError("Invalid ownership state", err.Error())
		return
	}
	if change == "create" {
		owned = nil
	}
	raw, known := setStrings(plan.Commands)
	if !known {
		diagnostics.AddError("Unknown commands", "Commands must be known during apply.")
		return
	}
	desired, err := ParseCommands(raw)
	if err != nil {
		diagnostics.AddError("Invalid commands", err.Error())
		return
	}
	if change == "destroy" {
		desired = nil
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		diagnostics.AddError("Cannot read VyOS before apply", err.Error())
		return
	}
	batch, err := BuildBatch(snapshot, owned, desired)
	if err != nil {
		diagnostics.AddError("Unsafe configuration removal", err.Error())
		return
	}
	logBatch(ctx, batch, "apply", change, plan.Name.ValueString(), c.apiKey)
	if plan.ID.IsNull() || plan.ID.IsUnknown() {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			diagnostics.AddError("Cannot generate ID", "Random ID generation failed.")
			return
		}
		plan.ID = types.StringValue(fmt.Sprintf("%x", id))
	}
	// Once a mutation is attempted, retain the union of old and new ownership.
	// A timeout is not proof that the router did not commit. The next refresh
	// compares this ledger with desired commands and retries only missing work.
	ledger, _ := setStrings(old.Managed)
	if old.Managed.IsNull() {
		ledger, _ = setStrings(old.Commands)
	}
	ledger = append(ledger, raw...)
	plan.Managed = managedSet(ledger)
	plan.InSync = types.BoolValue(false)
	plan.PendingSave = types.BoolValue(plan.Save.ValueBool())
	checkpoint := func() { diagnostics.Append(state.Set(ctx, &plan)...) }
	if len(batch.Operations) > 0 {
		checkpoint()
		if diagnostics.HasError() {
			return
		}
		if err := c.Configure(ctx, batch.Operations); err != nil {
			diagnostics.AddError("VyOS configuration batch failed", err.Error()+" Ownership is retained. Read the active router configuration before retrying an uncertain commit.")
			return
		}
		// A successful HTTP response may precede an asynchronous HTTPS-service
		// commit. Do not declare success without checking the active assertions.
		snapshot, err = c.Snapshot(ctx)
		if err != nil {
			checkpoint()
			diagnostics.AddError("Cannot verify VyOS commit", err.Error())
			return
		}
	}
	check, err := BuildBatch(snapshot, owned, desired)
	if err != nil || len(check.Operations) != 0 {
		checkpoint()
		diagnostics.AddError("VyOS did not reach the desired configuration", "The active configuration does not satisfy the batch. Ownership is retained. Check value normalization, node defaults, conflicting scalar SETs, concurrent writers, or an asynchronous commit on the router.")
		return
	}
	if plan.Save.ValueBool() {
		checkpoint()
		if diagnostics.HasError() {
			return
		}
		if err := c.Save(ctx); err != nil {
			diagnostics.AddError("VyOS save failed", err.Error()+" The running configuration may already be committed; a later apply will retry saving.")
			return
		}
	}
	if change == "destroy" {
		state.RemoveResource(ctx)
		return
	}
	plan.Managed = managedSet(raw)
	plan.InSync = types.BoolValue(true)
	plan.PendingSave = types.BoolValue(false)
	diagnostics.Append(state.Set(ctx, &plan)...)
}
