// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/nsbno/terraform-provider-static-file-deploy/internal/deployer"
)

// Ensure provider defined types fully satisfy framework interfaces.
var _ resource.Resource = &DeploymentResource{}
var _ resource.ResourceWithImportState = &DeploymentResource{}

func NewDeploymentResource() resource.Resource {
	return &DeploymentResource{}
}

// DeploymentResource defines the resource implementation.
type DeploymentResource struct {
	deployer *deployer.Deployer
}

// DeploymentResourceModel describes the resource data model.
type DeploymentResourceModel struct {
	Source              types.String        `tfsdk:"source"`
	SourceVersion       types.String        `tfsdk:"source_version"`
	Target              types.String        `tfsdk:"target"`
	TargetRegion        types.String        `tfsdk:"target_region"`
	DefaultCacheControl types.String        `tfsdk:"default_cache_control"`
	FileOverrides       []FileOverrideModel `tfsdk:"file_override"`
}

// FileOverrideModel describes a cache-control override for a specific file.
type FileOverrideModel struct {
	File         types.String `tfsdk:"file"`
	CacheControl types.String `tfsdk:"cache_control"`
}

func (r *DeploymentResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_deployment"
}

func (r *DeploymentResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Deploys a set of files from a source ZIP file in an S3 bucket to a target S3 bucket.",

		Attributes: map[string]schema.Attribute{
			"source": schema.StringAttribute{
				MarkdownDescription: "The S3 bucket and path to the ZIP file containing the source files to be deployed. Format: 'bucket-name/path/to/source.zip'.",
				Required:            true,
			},
			"source_version": schema.StringAttribute{
				MarkdownDescription: "The version ID of the source ZIP file in the S3 bucket. This is used to handle versioning of files in S3. If not specified, the latest version will be used.",
				Optional:            true,
			},
			"target": schema.StringAttribute{
				MarkdownDescription: "The target S3 bucket where the unzipped files will be deployed.",
				Required:            true,
			},
			"target_region": schema.StringAttribute{
				MarkdownDescription: "The target region of the S3 bucket where the unzipped files will be deployed.",
				Optional:            true,
				Default:             stringdefault.StaticString("eu-west-1"),
				Computed:            true,
			},
			"default_cache_control": schema.StringAttribute{
				MarkdownDescription: "The default Cache-Control header value to apply to all deployed files. If not specified, no Cache-Control header will be set.",
				Optional:            true,
			},
		},
		Blocks: map[string]schema.Block{
			"file_override": schema.ListNestedBlock{
				MarkdownDescription: "Override the Cache-Control header for specific files. Each file path must be unique across all overrides.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"file": schema.StringAttribute{
							MarkdownDescription: "The exact path of the file within the ZIP archive to override.",
							Required:            true,
						},
						"cache_control": schema.StringAttribute{
							MarkdownDescription: "The Cache-Control header value to apply to this specific file.",
							Required:            true,
						},
					},
				},
			},
		},
	}
}

func (r *DeploymentResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return
	}

	r.deployer = &deployer.Deployer{
		DefaultAWSConfig: cfg,
	}
}

// validateFileOverrides checks for duplicate file paths in file overrides.
func (r *DeploymentResource) validateFileOverrides(data *DeploymentResourceModel) error {
	if len(data.FileOverrides) == 0 {
		return nil
	}

	seenFiles := make(map[string]bool)
	for _, override := range data.FileOverrides {
		filePath := override.File.ValueString()
		if seenFiles[filePath] {
			return fmt.Errorf("duplicate file path in file_override blocks: '%s'", filePath)
		}
		seenFiles[filePath] = true
	}
	return nil
}

// buildCacheControlConfig creates a CacheControlConfig from the resource model.
func (r *DeploymentResource) buildCacheControlConfig(data *DeploymentResourceModel) *deployer.CacheControlConfig {
	ccConfig := &deployer.CacheControlConfig{
		FileOverrides: make(map[string]string),
	}

	if !data.DefaultCacheControl.IsNull() {
		ccConfig.DefaultCacheControl = data.DefaultCacheControl.ValueString()
	}

	for _, override := range data.FileOverrides {
		ccConfig.FileOverrides[override.File.ValueString()] = override.CacheControl.ValueString()
	}

	return ccConfig
}

func (r *DeploymentResource) runDeployment(data *DeploymentResourceModel) error {
	// Validate file overrides for duplicates
	if err := r.validateFileOverrides(data); err != nil {
		return err
	}

	sourceParts := strings.SplitN(data.Source.ValueString(), "/", 2)
	if len(sourceParts) != 2 {
		return fmt.Errorf("invalid source format: %s", sourceParts)
	}
	sourceBucket := sourceParts[0]
	sourceKey := sourceParts[1]

	deployment := r.deployer.NewDeployment(sourceBucket, data.Target.ValueString(), data.TargetRegion.ValueString())

	// Build cache control config from model
	cacheControlConfig := r.buildCacheControlConfig(data)

	// Use source version if provided, otherwise use latest (nil)
	var sourceVersion *string
	if !data.SourceVersion.IsNull() {
		sourceVersion = data.SourceVersion.ValueStringPointer()
	}

	_, err := deployment.Deploy(sourceKey, sourceVersion, cacheControlConfig)
	if err != nil {
		return fmt.Errorf("error during deployment: %w", err)
	}

	return nil
}

func (r *DeploymentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data DeploymentResourceModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	err := r.runDeployment(&data)
	if err != nil {
		resp.Diagnostics.AddError("Error during deployment", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *DeploymentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state DeploymentResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	sourceParts := strings.SplitN(state.Source.ValueString(), "/", 2)
	if len(sourceParts) != 2 {
		resp.Diagnostics.AddError("Could not read source format", fmt.Sprintf("invalid source format: %s", sourceParts))
		return
	}
	sourceBucket := sourceParts[0]
	sourceKey := sourceParts[1]

	deployment := r.deployer.NewDeployment(sourceBucket, state.Target.ValueString(), state.TargetRegion.ValueString())

	// Use source version if provided, otherwise use latest (nil)
	var sourceVersion *string
	if !state.SourceVersion.IsNull() {
		sourceVersion = state.SourceVersion.ValueStringPointer()
	}

	_, err := deployment.HashesForArtifact(sourceKey, sourceVersion)
	if err != nil {
		return
	}

	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
}

func (r *DeploymentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data DeploymentResourceModel

	// Read Terraform plan data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	err := r.runDeployment(&data)
	if err != nil {
		resp.Diagnostics.AddError("Error during deployment", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *DeploymentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data DeploymentResourceModel

	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}
}

func (r *DeploymentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
