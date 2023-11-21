// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
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
	"io"
	"net/http"
	"path/filepath"
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
	client *http.Client
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

func calculateMD5(content []byte) string {
	hasher := md5.New()
	hasher.Write(content)
	return hex.EncodeToString(hasher.Sum(nil))
}

func downloadAndComputeHashes(s3Client *s3.Client, source string, sourceVersion string) (map[string]string, error) {
	sourceParts := strings.SplitN(source, "/", 2)
	if len(sourceParts) != 2 {
		return nil, fmt.Errorf("invalid source format: %s", source)
	}
	sourceBucket := sourceParts[0]
	sourceKey := sourceParts[1]

	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(sourceBucket),
		Key:    aws.String(sourceKey),
	}
	result, err := s3Client.GetObject(context.Background(), getObjectInput)
	if err != nil {
		return nil, fmt.Errorf("failed to download object from S3: %w", err)
	}
	defer result.Body.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, result.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read object from S3: %w", err)
	}

	zipReader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return nil, fmt.Errorf("failed to unzip file: %w", err)
	}

	fileHashes := make(map[string]string)
	for _, file := range zipReader.File {
		zippedFile, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open zipped file: %w", err)
		}
		defer zippedFile.Close()

		fileContent, err := io.ReadAll(zippedFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read zipped file content: %w", err)
		}

		fileHashes[file.Name] = calculateMD5(fileContent)
	}

	return fileHashes, nil
}

func (r *DeploymentResource) performDeployment(ctx context.Context, state *DeploymentResourceModel, sourcePath, sourceVersion, targetBucket string) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS configuration: %w", err)
	}
	s3Client := s3.NewFromConfig(cfg)

	sourceParts := strings.SplitN(sourcePath, "/", 2)
	if len(sourceParts) != 2 {
		return fmt.Errorf("invalid source format: %s", sourceParts)
	}
	sourceBucket := sourceParts[0]
	sourceKey := sourceParts[1]

	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(sourceBucket),
		Key:    aws.String(sourceKey),
	}
	result, err := s3Client.GetObject(ctx, getObjectInput)
	if err != nil {
		return fmt.Errorf("failed to download object from S3: %w", err)
	}
	defer result.Body.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, result.Body)
	if err != nil {
		return fmt.Errorf("failed to read object from S3: %w", err)
	}

	zipReader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return fmt.Errorf("failed to unzip file: %w", err)
	}

	var files []FileState
	for _, file := range zipReader.File {
		zippedFile, err := file.Open()
		if err != nil {
			return fmt.Errorf("failed to open zipped file: %w", err)
		}
		defer zippedFile.Close()

		fileContent, err := io.ReadAll(zippedFile)
		if err != nil {
			return fmt.Errorf("failed to read zipped file content: %w", err)
		}

		_, fileName := filepath.Split(file.Name)
		md5Checksum := calculateMD5(fileContent) // Implement this function

		putObjectInput := &s3.PutObjectInput{
			Bucket: aws.String(targetBucket),
			Key:    aws.String(fileName),
			Body:   bytes.NewReader(fileContent),
		}
		_, err = s3Client.PutObject(ctx, putObjectInput)

		files = append(files, FileState{
			Filename: types.StringValue(fileName),
			ETag:     types.StringValue(md5Checksum),
		})

	}

	var filesState, diags = filesValuesToState(files)
	if diags.HasError() {
		return fmt.Errorf("failed to set files state: %s", diags)
	}

	state.Files = *filesState
	return nil
}

func (r *DeploymentResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*http.Client)

	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *http.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)

		return
	}

	r.client = client
}

func (r *DeploymentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data DeploymentResourceModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	err := r.performDeployment(ctx, &data, data.Source.ValueString(), data.SourceVersion.ValueString(), data.Target.ValueString())
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

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to load AWS configuration", err.Error())
		return
	}
	s3Client := s3.NewFromConfig(cfg)

	sourceFileHashes, err := downloadAndComputeHashes(s3Client, state.Source.ValueString(), state.SourceVersion.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to process source files", err.Error())
		return
	}

	updatedFiles := make([]FileState, 0)

	files, diags := filesStateToValues(state.Files)
	resp.Diagnostics.Append(diags...)

	for _, fileState := range files {
		sourceHash, exists := sourceFileHashes[fileState.Filename.ValueString()]
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

	err := r.performDeployment(ctx, &data, data.Source.ValueString(), data.SourceVersion.ValueString(), data.Target.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error during update", err.Error())
		return
	}

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *DeploymentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data DeploymentResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// If applicable, this is a great opportunity to initialize any necessary
	// provider client data and make a call using it.
	// httpResp, err := r.client.Do(httpReq)
	// if err != nil {
	//     resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to delete example, got error: %s", err))
	//     return
	// }
}

func (r *DeploymentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
