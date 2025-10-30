data "vy_artifact_version" "this" {
  application = "petstore"
}

data "aws_s3_bucket" "website_bucket" {
  bucket = "123456789012-my-cool-bucket"
}

# Basic deployment without cache control
resource "staticfiledeploy_deployment" "example_deployment" {
  source         = "${data.vy_artifact_version.this.store}/${data.vy_artifact_version.this.path}"
  source_version = data.vy_artifact_version.this.version
  target         = data.aws_s3_bucket.website_bucket.bucket
}

# Deployment with default cache control and file-specific overrides
resource "staticfiledeploy_deployment" "with_cache_control" {
  source         = "${data.vy_artifact_version.this.store}/${data.vy_artifact_version.this.path}"
  source_version = data.vy_artifact_version.this.version
  target         = data.aws_s3_bucket.website_bucket.bucket

  # Apply default cache control to all files
  default_cache_control = "public, max-age=31536000, immutable"

  # Override cache control for specific files
  file_override {
    file          = "index.html"
    cache_control = "public, max-age=0, must-revalidate"
  }

  file_override {
    file          = "service-worker.js"
    cache_control = "public, max-age=3600"
  }
}