terraform {
  required_providers {
    pveguest = {
      source = "tofu.home.arpa/agoodkind/pveguest"
      version = "0.1.0"
    }
  }
}

variable "vault_alpha" {
  type = string
  sensitive = true
}

provider "pveguest" {
  nodes = {
    site = {
      endpoint = "https://127.0.0.1:1"
      api_token = "fixture@pam!fixture=${var.vault_alpha}"
    }
  }
}

module "base" {
  source = "./item"
  for_each = toset(["guest_alpha"])
}
