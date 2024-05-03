// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"os"
	"strings"
	"testing"
)

func createS3Bucket(s3Client *s3.Client, bucketName string, targetBucketRegion string) error {
	_, err := s3Client.CreateBucket(context.TODO(), &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
		CreateBucketConfiguration: &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(targetBucketRegion),
		},
	})
	return err
}

func deleteS3Bucket(s3Client *s3.Client, bucketName string) error {
	// List and delete all objects in the bucket
	listObjectsInput := &s3.ListObjectsV2Input{Bucket: aws.String(bucketName)}
	for {
		output, err := s3Client.ListObjectsV2(context.TODO(), listObjectsInput)
		if err != nil {
			return fmt.Errorf("failed to list objects for bucket %s: %w", bucketName, err)
		}

		for _, object := range output.Contents {
			_, err := s3Client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
				Bucket: aws.String(bucketName),
				Key:    object.Key,
			})
			if err != nil {
				return fmt.Errorf("failed to delete object %s from bucket %s: %w", *object.Key, bucketName, err)
			}
		}

		if !output.IsTruncated {
			break
		}

		listObjectsInput.ContinuationToken = output.NextContinuationToken
	}

	// Now delete the bucket
	_, err := s3Client.DeleteBucket(context.TODO(), &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return fmt.Errorf("failed to delete bucket %s: %w", bucketName, err)
	}

	return nil
}

func createTestZIP(zipPath string, files map[string]string) (map[string]string, error) {
	newZipFile, err := os.Create(zipPath)
	if err != nil {
		return nil, err
	}
	defer newZipFile.Close()

	zipWriter := zip.NewWriter(newZipFile)
	defer zipWriter.Close()

	// Map to store the calculated MD5 hashes
	fileHashes := make(map[string]string)

	for filename, content := range files {
		fileWriter, err := zipWriter.Create(filename)
		if err != nil {
			return nil, err
		}

		_, err = fileWriter.Write([]byte(content))
		if err != nil {
			return nil, err
		}

		// Calculate MD5 hash
		hasher := md5.New()
		hasher.Write([]byte(content))
		hash := hex.EncodeToString(hasher.Sum(nil))
		fileHashes[filename] = hash
	}

	return fileHashes, nil
}

func uploadZIPToS3(s3Client *s3.Client, bucketName, zipPath, zipKey string) error {
	zipFile, err := os.Open(zipPath)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(zipKey),
		Body:   zipFile,
	})
	return err
}

// Acceptance Test

func testAccStaticFileDeployDeploymentConfig(sourceBucketName, zipKey, targetBucketName string) string {
	return fmt.Sprintf(`
resource "staticfiledeploy_deployment" "test_deployment" {
    source        = "%s/%s"
    source_version = "some-version"
    target        = "%s"
}
`, sourceBucketName, zipKey, targetBucketName)
}

const ResourceName = "staticfiledeploy_deployment.test_deployment"

func testAccCheckStaticFileDeployDeploymentExists(s3Client *s3.Client, expectedFiles map[string]string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		// Find the resource in the Terraform state
		rs, ok := s.RootModule().Resources[ResourceName]
		if !ok {
			return fmt.Errorf("resource not found in Terraform state: %s", ResourceName)
		}

		// Extract the target bucket name from the state
		target, ok := rs.Primary.Attributes["target"]
		if !ok {
			return fmt.Errorf("target attribute not found in resource state")
		}

		// List objects in the target bucket
		resp, err := s3Client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
			Bucket: aws.String(target),
		})
		if err != nil {
			return fmt.Errorf("error listing objects in target bucket (%s): %w", target, err)
		}

		// Create a set of the file names found in the target bucket
		foundFiles := make(map[string]string)
		for _, object := range resp.Contents {
			foundFiles[*object.Key] = *object.ETag
		}

		// Check if each expected file is present in the target bucket
		for fileName := range expectedFiles {
			etag, found := foundFiles[fileName]
			if !found {
				return fmt.Errorf("expected file %s not found in target bucket %s", fileName, target)
			}

			// The AWS SDK returns the ETag with surrounding quotes for some reason
			etag = strings.Trim(etag, "\"")

			if etag != expectedFiles[fileName] {
				return fmt.Errorf("hash mismatch for file %s: expected %s, got %s", fileName, expectedFiles[fileName], etag)
			}
		}

		return nil
	}
}

func TestAccStaticFileDeployDeployment_basic(t *testing.T) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	s3Client := s3.NewFromConfig(cfg)

	sourceBucketName := fmt.Sprintf("tf-test-bucket-source-%s", acctest.RandString(8))
	targetBucketName := fmt.Sprintf("tf-test-bucket-target-%s", acctest.RandString(8))

	err = createS3Bucket(s3Client, sourceBucketName, "eu-west-1")
	if err != nil {
		t.Fatalf("Failed to create S3 bucket: %s", err)
	}
	defer func(s3Client *s3.Client, bucketName string) {
		_ = deleteS3Bucket(s3Client, bucketName)
	}(s3Client, sourceBucketName) // Ensure cleanup after the test

	err = createS3Bucket(s3Client, targetBucketName, "eu-west-1")
	if err != nil {
		t.Fatalf("Failed to create S3 bucket: %s", err)
	}
	defer func(s3Client *s3.Client, bucketName string) {
		_ = deleteS3Bucket(s3Client, bucketName)
	}(s3Client, targetBucketName) // Ensure cleanup after the test

	zipPath := "test_a.zip"
	zipKey := "test_a.zip"

	filesToCreate := map[string]string{
		"file1.txt": "Test content for file1",
		"file2.js":  "Test content for file2",
	}

	expectedFiles, err := createTestZIP(zipPath, filesToCreate)
	if err != nil {
		t.Fatalf("Failed to create ZIP file: %s", err)
	}
	defer os.Remove(zipPath)

	err = uploadZIPToS3(s3Client, sourceBucketName, zipPath, zipKey)
	if err != nil {
		t.Fatalf("Failed to upload ZIP file to S3: %s", err)
	}

	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t) // Implement this function as needed
		},
		Steps: []resource.TestStep{
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				ExternalProviders: map[string]resource.ExternalProvider{
					"aws": {
						VersionConstraint: ">= 5.0.0",
						Source:            "hashicorp/aws",
					},
				},
				Config: testAccStaticFileDeployDeploymentConfig(sourceBucketName, zipKey, targetBucketName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckStaticFileDeployDeploymentExists(s3Client, expectedFiles),
				),
			},
		},
	})
}

func TestAccStaticFileDeployDeployment_canChangeArtifact(t *testing.T) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	s3Client := s3.NewFromConfig(cfg)

	sourceBucketName := fmt.Sprintf("tf-test-bucket-source-%s", acctest.RandString(8))
	targetBucketName := fmt.Sprintf("tf-test-bucket-target-%s", acctest.RandString(8))

	err = createS3Bucket(s3Client, sourceBucketName, "eu-west-1")
	if err != nil {
		t.Fatalf("Failed to create S3 bucket: %s", err)
	}
	defer func(s3Client *s3.Client, bucketName string) {
		_ = deleteS3Bucket(s3Client, bucketName)
	}(s3Client, sourceBucketName) // Ensure cleanup after the test

	err = createS3Bucket(s3Client, targetBucketName, "eu-west-1")
	if err != nil {
		t.Fatalf("Failed to create S3 bucket: %s", err)
	}
	defer func(s3Client *s3.Client, bucketName string) {
		_ = deleteS3Bucket(s3Client, bucketName)
	}(s3Client, targetBucketName) // Ensure cleanup after the test

	zipPath := "test.zip"
	zipKey := "test.zip"

	filesToCreate := map[string]string{
		"file1.txt": "Test content for file1",
		"file2.txt": "Test content for file2",
	}

	expectedFiles, err := createTestZIP(zipPath, filesToCreate)
	if err != nil {
		t.Fatalf("Failed to create ZIP file: %s", err)
	}
	defer os.Remove(zipPath)

	err = uploadZIPToS3(s3Client, sourceBucketName, zipPath, zipKey)
	if err != nil {
		t.Fatalf("Failed to upload ZIP file to S3: %s", err)
	}

	zipPath2 := "test2.zip"
	zipKey2 := "test2.zip"

	filesToCreate2 := map[string]string{
		"file1.txt": "Test content for file1",
		"file2.txt": "Changed content in file2",
		"file3.txt": "New content for file3",
	}

	expectedFiles2, err := createTestZIP(zipPath2, filesToCreate2)
	if err != nil {
		t.Fatalf("Failed to create ZIP file: %s", err)
	}
	defer os.Remove(zipPath2)

	err = uploadZIPToS3(s3Client, sourceBucketName, zipPath2, zipKey2)
	if err != nil {
		t.Fatalf("Failed to upload ZIP file to S3: %s", err)
	}

	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t) // Implement this function as needed
		},
		Steps: []resource.TestStep{
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				ExternalProviders: map[string]resource.ExternalProvider{
					"aws": {
						VersionConstraint: ">= 5.0.0",
						Source:            "hashicorp/aws",
					},
				},
				Config: testAccStaticFileDeployDeploymentConfig(sourceBucketName, zipKey, targetBucketName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckStaticFileDeployDeploymentExists(s3Client, expectedFiles),
				),
			},
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				ExternalProviders: map[string]resource.ExternalProvider{
					"aws": {
						VersionConstraint: ">= 5.0.0",
						Source:            "hashicorp/aws",
					},
				},
				Config: testAccStaticFileDeployDeploymentConfig(sourceBucketName, zipKey2, targetBucketName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckStaticFileDeployDeploymentExists(s3Client, expectedFiles2),
				),
			},
		},
	})
}

func testAccStaticFileDeployDeploymentConfig_withTargetRegion(sourceBucketName, zipKey, targetBucketName string, targetRegion string) string {
	return fmt.Sprintf(`
resource "staticfiledeploy_deployment" "test_deployment" {
    source        = "%s/%s"
    source_version = "some-version"
    target        = "%s"
	target_region = "%s"
}
`, sourceBucketName, zipKey, targetBucketName, targetRegion)
}

func TestAccStaticFileDeployDeployment_withTargetRegion(t *testing.T) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	sourceS3Client := s3.NewFromConfig(cfg)
	targetS3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.Region = "eu-central-1"
	})

	sourceBucketName := fmt.Sprintf("tf-test-bucket-source-%s", acctest.RandString(8))
	targetBucketName := fmt.Sprintf("tf-test-bucket-target-%s", acctest.RandString(8))
	targetBucketRegion := "eu-central-1"

	err = createS3Bucket(sourceS3Client, sourceBucketName, "eu-west-1")
	if err != nil {
		t.Fatalf("Failed to create S3 bucket: %s", err)
	}
	defer func(s3Client *s3.Client, bucketName string) {
		_ = deleteS3Bucket(s3Client, bucketName)
	}(sourceS3Client, sourceBucketName) // Ensure cleanup after the test

	err = createS3Bucket(targetS3Client, targetBucketName, targetBucketRegion)
	if err != nil {
		t.Fatalf("Failed to create S3 bucket: %s", err)
	}
	defer func(s3Client *s3.Client, bucketName string) {
		_ = deleteS3Bucket(s3Client, bucketName)
	}(targetS3Client, targetBucketName) // Ensure cleanup after the test

	zipPath := "test.zip"
	zipKey := "test.zip"

	filesToCreate := map[string]string{
		"file1.txt": "Test content for file1",
		"file2.txt": "Test content for file2",
	}

	expectedFiles, err := createTestZIP(zipPath, filesToCreate)
	if err != nil {
		t.Fatalf("Failed to create ZIP file: %s", err)
	}
	defer os.Remove(zipPath)

	err = uploadZIPToS3(sourceS3Client, sourceBucketName, zipPath, zipKey)
	if err != nil {
		t.Fatalf("Failed to upload ZIP file to S3: %s", err)
	}

	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t) // Implement this function as needed
		},
		Steps: []resource.TestStep{
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				ExternalProviders: map[string]resource.ExternalProvider{
					"aws": {
						VersionConstraint: ">= 5.0.0",
						Source:            "hashicorp/aws",
					},
				},
				Config: testAccStaticFileDeployDeploymentConfig_withTargetRegion(sourceBucketName, zipKey, targetBucketName, targetBucketRegion),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckStaticFileDeployDeploymentExists(targetS3Client, expectedFiles),
				),
			},
		},
	})
}
