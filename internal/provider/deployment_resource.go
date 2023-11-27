// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/nsbno/terraform-provider-static-file-deploy/internal/deployer"
	"strings"
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
	Source        types.String `tfsdk:"source"`
	SourceVersion types.String `tfsdk:"source_version"`
	Target        types.String `tfsdk:"target"`
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
				MarkdownDescription: "The version ID of the source ZIP file in the S3 bucket. This is used to handle versioning of files in S3.",
				Required:            true,
			},
			"target": schema.StringAttribute{
				MarkdownDescription: "The target S3 bucket where the unzipped files will be deployed.",
				Required:            true,
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
	s3Client := s3.NewFromConfig(cfg)
	r.deployer = &deployer.Deployer{
		S3Client: s3Client,
	}
}

func (r *DeploymentResource) runDeployment(data *DeploymentResourceModel) error {
	sourceParts := strings.SplitN(data.Source.ValueString(), "/", 2)
	if len(sourceParts) != 2 {
		return fmt.Errorf("invalid source format: %s", sourceParts)
	}
	sourceBucket := sourceParts[0]
	sourceKey := sourceParts[1]

	deployment := r.deployer.NewDeployment(sourceBucket, data.Target.ValueString())

	_, err := deployment.Deploy(sourceKey, nil)
	if err != nil {
		return fmt.Errorf("Error during deployment: %w", err)
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

	deployment := r.deployer.NewDeployment(sourceBucket, state.Target.ValueString())

	_, err := deployment.HashesForArtifact(sourceKey, nil)
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
