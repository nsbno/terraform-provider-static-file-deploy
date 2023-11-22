// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
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

type FileState struct {
	Filename types.String `tfsdk:"filename"`
	ETag     types.String `tfsdk:"etag"`
}

func (s FileState) AttributeTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"filename": types.StringType,
		"etag":     types.StringType,
	}
}

func filesStateToValues(files types.List) ([]FileState, diag.Diagnostics) {
	var diags diag.Diagnostics
	var fileStates []FileState

	for _, file := range files.Elements() {
		fileObj, ok := file.(types.Object)
		if !ok {
			diags.AddError("Error reading file object", "Expected types.Object")
			continue
		}

		var fileState FileState
		diags = fileObj.As(context.Background(), &fileState, basetypes.ObjectAsOptions{})

		fileStates = append(fileStates, fileState)
	}

	return fileStates, diags
}

func filesValuesToState(files []FileState) (*types.List, diag.Diagnostics) {
	var objects []types.Object

	for _, file := range files {
		object, diags := types.ObjectValueFrom(context.Background(), file.AttributeTypes(), file)

		if diags.HasError() {
			return nil, diags
		}

		objects = append(objects, object)
	}

	var filesList, diags = types.ListValueFrom(
		context.Background(),
		types.ObjectType{AttrTypes: FileState{}.AttributeTypes()},
		objects,
	)
	if diags.HasError() {
		return nil, diags
	}

	return &filesList, diags
}

// DeploymentResourceModel describes the resource data model.
type DeploymentResourceModel struct {
	Source        types.String `tfsdk:"source"`
	SourceVersion types.String `tfsdk:"source_version"`
	Target        types.String `tfsdk:"target"`
	Files         types.List   `tfsdk:"files"`
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
			"files": schema.ListNestedAttribute{
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"filename": schema.StringAttribute{
							MarkdownDescription: "The name of the file deployed to the target S3 bucket.",
							Required:            true,
						},
						"etag": schema.StringAttribute{
							MarkdownDescription: "The MD5 checksum (ETag) of the file in the target S3 bucket.",
							Required:            true,
						},
					},
				},
				Computed:            true,
				MarkdownDescription: "A list of files deployed, each with its filename and corresponding MD5 checksum (ETag).",
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *DeploymentResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	cfg, err := config.LoadDefaultConfig(context.TODO())
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

	deployedFiles, err := deployment.Deploy(sourceKey, nil)
	if err != nil {
		return fmt.Errorf("Error during deployment: %w", err)
	}

	var files []FileState
	for fileName, md5Checksum := range deployedFiles {
		files = append(files, FileState{
			Filename: types.StringValue(fileName),
			ETag:     types.StringValue(md5Checksum),
		})
	}

	var filesState, diags = filesValuesToState(files)
	if diags.HasError() {
		return fmt.Errorf("failed to set files state: %s", diags)
	}
	data.Files = *filesState
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

	hashesFromSource, err := deployment.HashesForArtifact(sourceKey, nil)
	if err != nil {
		return
	}

	var updatedFiles []FileState

	files, diags := filesStateToValues(state.Files)
	resp.Diagnostics.Append(diags...)

	for _, fileState := range files {
		sourceHash, exists := hashesFromSource[fileState.Filename.ValueString()]
		if !exists || sourceHash != fileState.ETag.ValueString() {
			updatedFiles = append(updatedFiles, FileState{
				Filename: fileState.Filename,
				ETag:     types.StringValue(sourceHash),
			})
		} else {
			updatedFiles = append(updatedFiles, fileState)
		}
	}

	var fileState *types.List
	fileState, diags = filesValuesToState(updatedFiles)
	resp.Diagnostics.Append(diags...)

	state.Files = *fileState
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
