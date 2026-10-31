# OpenTofu init runs the external key provider and waits for its answer.

variable "provider_script" {
  type = string
}

variable "pid_file" {
  type = string
}

terraform {
  backend "local" {}

  encryption {
    key_provider "external" "wait" {
      command = [var.provider_script, var.pid_file]
    }
    method "aes_gcm" "main" {
      keys = key_provider.external.wait
    }
    state {
      method = method.aes_gcm.main
    }
  }
}
