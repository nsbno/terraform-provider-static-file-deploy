data "vy_artifact_version" "this" {
  application = "petstore"
}

resource "static_file_deploy_deployment" "example_deployment" {
  source         = "${data.vy_artifact_version.this.store}/${data.vy_artifact_version.this.path}"
  source_version = data.vy_artifact_version.this.version
  target         = "1234567890-website"
}