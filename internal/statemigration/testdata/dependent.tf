terraform {
  backend "local" {
    path = "runtime.tfstate"
  }
  encryption {
    key_provider "pbkdf2" "state" {
      passphrase = "${var.vault_tofu_state_passphrase}"
    }
    method "aes_gcm" "state" {
      keys = key_provider.pbkdf2.state
    }
    state {
      method   = method.aes_gcm.state
      enforced = true
    }
  }
}

variable "vault_tofu_state_passphrase" {
  type      = string
  sensitive = true
}

variable "fixture_value" {
  type      = string
  sensitive = true
}

resource "terraform_data" "parent" {
  input = var.fixture_value
}

resource "terraform_data" "child" {
  input = {
    parent = terraform_data.parent.id
    value  = var.fixture_value
  }
}
