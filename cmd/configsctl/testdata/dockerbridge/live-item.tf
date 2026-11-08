terraform {
  required_providers {
    docker = {
      source = "kreuzwerker/docker"
    }
  }
}

variable "volumes" {
  type = map(string)
}

variable "run_label" {
  type = string
}

variable "pause_marker" {
  type = string
}

resource "docker_volume" "test" {
  for_each = var.volumes
  name     = each.value

  labels {
    label = "io.goodkind.configsctl.test-run"
    value = var.run_label
  }
}

resource "terraform_data" "pause" {
  count      = var.pause_marker == "" ? 0 : 1
  depends_on = [docker_volume.test]
  input      = var.pause_marker

  provisioner "local-exec" {
    command = "bash ${path.module}/live-pause.sh"
    environment = {
      CONFIGSCTL_LIVE_MARKER = var.pause_marker
    }
  }
}
