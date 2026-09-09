package provider

import (
	"context"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type Provider struct{ version string }
type providerModel struct {
	Endpoint       types.String `tfsdk:"endpoint"`
	APIKey         types.String `tfsdk:"api_key"`
	Insecure       types.Bool   `tfsdk:"insecure"`
	RequestTimeout types.String `tfsdk:"request_timeout"`
	PlanTimeout    types.String `tfsdk:"plan_timeout"`
}
type settings struct {
	endpoint       string
	apiKey         string
	insecure       bool
	requestTimeout time.Duration
	planTimeout    time.Duration
	unknown        bool
}

var _ provider.Provider = &Provider{}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &Provider{version: version} }
}
func (p *Provider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "vyoscmd"
	resp.Version = p.version
}
func (p *Provider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "Manage raw VyOS configuration commands through the HTTPS API.", Attributes: map[string]schema.Attribute{
		"endpoint":        schema.StringAttribute{Optional: true, Description: "Default router URL. Resources can override it. Environment: VYOS_ENDPOINT."},
		"api_key":         schema.StringAttribute{Optional: true, Sensitive: true, Description: "VyOS API key. Environment: VYOS_API_KEY."},
		"insecure":        schema.BoolAttribute{Optional: true, Description: "Disable TLS certificate verification. Default: false."},
		"request_timeout": schema.StringAttribute{Optional: true, Description: "Timeout per apply/read API request as a Go duration. Default: 120s."},
		"plan_timeout":    schema.StringAttribute{Optional: true, Description: "Total best-effort operation preview timeout. Default: 5s."},
	}}
}
func (p *Provider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var model providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s := &settings{endpoint: os.Getenv("VYOS_ENDPOINT"), apiKey: os.Getenv("VYOS_API_KEY"), requestTimeout: 120 * time.Second, planTimeout: 5 * time.Second}
	s.unknown = model.Endpoint.IsUnknown() || model.APIKey.IsUnknown() || model.Insecure.IsUnknown() || model.RequestTimeout.IsUnknown() || model.PlanTimeout.IsUnknown()
	if !model.Endpoint.IsNull() && !model.Endpoint.IsUnknown() {
		s.endpoint = model.Endpoint.ValueString()
	}
	if !model.APIKey.IsNull() && !model.APIKey.IsUnknown() {
		s.apiKey = model.APIKey.ValueString()
	}
	s.insecure = model.Insecure.ValueBool()
	if s.endpoint != "" {
		var err error
		s.endpoint, err = normalizeEndpoint(s.endpoint)
		if err != nil {
			resp.Diagnostics.AddError("Invalid endpoint", err.Error())
		}
	}
	for _, entry := range []struct {
		name   string
		value  types.String
		target *time.Duration
	}{
		{"request_timeout", model.RequestTimeout, &s.requestTimeout}, {"plan_timeout", model.PlanTimeout, &s.planTimeout},
	} {
		if entry.value.IsNull() || entry.value.IsUnknown() {
			continue
		}
		d, err := time.ParseDuration(entry.value.ValueString())
		if err != nil || d <= 0 {
			resp.Diagnostics.AddError("Invalid timeout", entry.name+" must be a positive Go duration, such as 120s.")
		} else {
			*entry.target = d
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.ResourceData = s
}
func (p *Provider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewCommandsResource}
}
func (p *Provider) DataSources(context.Context) []func() datasource.DataSource { return nil }
