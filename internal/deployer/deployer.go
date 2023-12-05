package deployer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Deployer is a client for deploying artifacts.
type Deployer struct {
	S3Client *s3.Client
}

func (d *Deployer) NewDeployment(sourceBucket string, targetBucket string) *Deployment {
	return &Deployment{
		SourceBucket: sourceBucket,
		TargetBucket: targetBucket,
		s3Client:     d.S3Client,
	}
}

// Deployment is responsible for deploying artifacts from a source bucket to a target bucket.
type Deployment struct {
	SourceBucket string
	TargetBucket string
	s3Client     *s3.Client
}

// DeployedFiles is a map of file keys to file hashes.
type DeployedFiles map[string]string

// getDeploymentArtifact returns the deployment artifact for the given key and version.
// If version is nil, the latest version is returned.
func (d *Deployment) getDeploymentArtifact(key string, version *string) (*zip.Reader, error) {
	var getObjectInput *s3.GetObjectInput
	if version != nil {
		getObjectInput = &s3.GetObjectInput{
			Bucket: aws.String(d.SourceBucket),
			Key:    aws.String(key),
		}
	} else {
		getObjectInput = &s3.GetObjectInput{
			Bucket:    aws.String(d.SourceBucket),
			Key:       aws.String(key),
			VersionId: version,
		}
	}

	result, err := d.s3Client.GetObject(context.Background(), getObjectInput)
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

	return zipReader, nil
}

// getDeploymentArtifactFileHashes returns the files of the deployment artifact for the given key and version.
// If version is nil, the latest version is returned.
func (d *Deployment) getDeploymentArtifactFileHashes(artifactZip *zip.Reader) (map[string]string, error) {
	hashes := make(map[string]string)

	for _, file := range artifactZip.File {
		zippedFile, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open zipped file: %w", err)
		}
		defer zippedFile.Close()

		fileContent, err := io.ReadAll(zippedFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read zipped file content: %w", err)
		}

		hasher := md5.New()
		hasher.Write(fileContent)
		var md5Checksum = hex.EncodeToString(hasher.Sum(nil))

		hashes[file.Name] = md5Checksum
	}

	return hashes, nil
}

// uploadDeploymentArtifactFiles uploads the given files to the target bucket.
func (d *Deployment) uploadDeploymentArtifactFiles(artifactZip *zip.Reader) error {
	for _, file := range artifactZip.File {
		zippedFile, err := file.Open()
		if err != nil {
			return fmt.Errorf("failed to open zipped file: %w", err)
		}
		defer zippedFile.Close()

		fileContent, err := io.ReadAll(zippedFile)
		if err != nil {
			return fmt.Errorf("failed to read zipped file content: %w", err)
		}

		contentTypes, err := mime.ExtensionsByType(file.Name)

		var contentType string
		if contentTypes == nil || err != nil {
			contentType = http.DetectContentType(fileContent)
		} else {
			contentType = contentTypes[0]
		}

		putObjectInput := &s3.PutObjectInput{
			Bucket:      aws.String(d.TargetBucket),
			Key:         aws.String(file.Name),
			Body:        bytes.NewReader(fileContent),
			ContentType: &contentType,
		}
		_, err = d.s3Client.PutObject(context.Background(), putObjectInput)
		if err != nil {
			return fmt.Errorf("failed to upload object to S3: %w", err)
		}
	}

	return nil
}

// Deploy deploys the artifact with the given key from the source bucket to the target bucket.
func (d *Deployment) Deploy(key string, version *string) (DeployedFiles, error) {
	artifactZip, err := d.getDeploymentArtifact(key, version)
	if err != nil {
		return nil, err
	}

	err = d.uploadDeploymentArtifactFiles(artifactZip)
	if err != nil {
		return nil, err
	}

	hashes, err := d.getDeploymentArtifactFileHashes(artifactZip)
	if err != nil {
		return nil, err
	}

	return hashes, nil
}

// HashesForArtifact returns all files that are in the given zip.
func (d *Deployment) HashesForArtifact(key string, version *string) (DeployedFiles, error) {
	artifactZip, err := d.getDeploymentArtifact(key, version)
	if err != nil {
		return nil, err
	}

	var hashes DeployedFiles
	hashes, err = d.getDeploymentArtifactFileHashes(artifactZip)
	if err != nil {
		return nil, err
	}

	return hashes, nil
}

// HashesForDeployedFiles returns all files that have been deployed to the target bucket.
func (d *Deployment) HashesForDeployedFiles() (DeployedFiles, error) {
	// List objects in the target bucket
	resp, err := d.s3Client.ListObjectsV2(
		context.Background(),
		&s3.ListObjectsV2Input{
			Bucket: aws.String(d.TargetBucket),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error listing objects in target bucket (%s): %w", d.TargetBucket, err)
	}

	// Create a set of the file names found in the target bucket
	foundFiles := make(map[string]string)
	for _, object := range resp.Contents {
		// The AWS SDK returns the ETag with surrounding quotes for some reason
		var etag = strings.Trim(*object.ETag, "\"")

		foundFiles[*object.Key] = etag
	}

	return foundFiles, nil
}
