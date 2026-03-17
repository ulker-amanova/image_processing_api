terraform {
  required_version = ">= 1.2.0"
  required_providers {
    null = { source = "hashicorp/null" }
  }
}

provider "null" {}

resource "null_resource" "docker_build" {
  provisioner "local-exec" {
    command = "docker build -t image-processing-service .."
    working_dir = ".."
  }
}
