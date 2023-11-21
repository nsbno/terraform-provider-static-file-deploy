// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"archive/zip"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"os"
	"strconv"
	"strings"
	"testing"
)

func createS3Bucket(s3Client *s3.Client, bucketName string) error {
	_, err := s3Client.CreateBucket(context.TODO(), &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
		CreateBucketConfiguration: &types.CreateBucketConfiguration{
			LocationConstraint: "eu-west-1",
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

func createTestZIP(zipPath string) (map[string]string, error) {
	newZipFile, err := os.Create(zipPath)
	if err != nil {
		return nil, err
	}
	defer newZipFile.Close()

	zipWriter := zip.NewWriter(newZipFile)
	defer zipWriter.Close()

	// Define the contents of the ZIP file
	files := map[string]string{
		"file1.txt": "Test content for file1",
		"file2.txt": "Test content for file2",
		// Add more files as needed
	}

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
		hash := calculateMD5([]byte(content))
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

func testAccCheckStaticFileDeployDeploymentExists(resourceName string, s3Client *s3.Client, expectedFiles map[string]string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		// Find the resource in the Terraform state
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource not found in Terraform state: %s", resourceName)
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

func testAccCheckStaticFileDeployDeploymentFiles(resourceName string, expectedFiles map[string]string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("Not found: %s", resourceName)
		}

		println(rs.Primary.String())
		numFilesStr, ok := rs.Primary.Attributes["files.#"]

		if !ok {
			return fmt.Errorf("files attribute not found in resource state")
		}

		// parse num files to int
		var numFiles, err = strconv.Atoi(numFilesStr)
		if err != nil {
			return fmt.Errorf("error parsing files attribute: %s", err)
		}

		for i := 0; i < numFiles; i++ {
			filenameAttr := fmt.Sprintf("files.%d.filename", i)
			etagAttr := fmt.Sprintf("files.%d.etag", i)

			fileName, ok := rs.Primary.Attributes[filenameAttr]
			if !ok {
				return fmt.Errorf("filename attribute not found in resource state")
			}

			etag, ok := rs.Primary.Attributes[etagAttr]
			if !ok {
				return fmt.Errorf("etag attribute not found in resource state")
			}

			expectedHash, ok := expectedFiles[fileName]
			if !ok {
				return fmt.Errorf("unexpected file: %s", fileName)
			}
			if etag != expectedHash {
				return fmt.Errorf("hash mismatch for file %s: expected %s, got %s", fileName, expectedHash, etag)
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
	zipPath := "test.zip"
	zipKey := "test.zip"

	err = createS3Bucket(s3Client, sourceBucketName)
	if err != nil {
		t.Fatalf("Failed to create S3 bucket: %s", err)
	}
	defer deleteS3Bucket(s3Client, sourceBucketName) // Ensure cleanup after the test

	err = createS3Bucket(s3Client, targetBucketName)
	if err != nil {
		t.Fatalf("Failed to create S3 bucket: %s", err)
	}
	defer deleteS3Bucket(s3Client, targetBucketName) // Ensure cleanup after the test

	expectedFiles, err := createTestZIP(zipPath)
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
					testAccCheckStaticFileDeployDeploymentFiles("staticfiledeploy_deployment.test_deployment", expectedFiles),
					testAccCheckStaticFileDeployDeploymentExists("staticfiledeploy_deployment.test_deployment", s3Client, expectedFiles),
				),
			},
		},
	})
}
