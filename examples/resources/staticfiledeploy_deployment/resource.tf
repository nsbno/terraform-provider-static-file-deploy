data "vy_artifact_version" "this" {
  application = "petstore"
}

data "aws_s3_bucket" "website_bucket" {
  bucket = "123456789012-my-cool-bucket"
}

resource "staticfiledeploy_deployment" "example_deployment" {
  source         = "${data.vy_artifact_version.this.store}/${data.vy_artifact_version.this.path}"
  source_version = data.vy_artifact_version.this.version
  target         = data.aws_s3_bucket.website_bucket.bucket
}