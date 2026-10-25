terraform {
  backend "local" {
    path = "runtime.tfstate"
  }
  encryption {
    key_provider "pbkdf2" "state" {
      passphrase = "${var.vault_tofu_state_passphrase}"
      iterations    = 200000
      hash_function = "sha256"
    }
    method "aes_gcm" "state" {
      keys = key_provider.pbkdf2.state
    }
    state {
      method   = method.aes_gcm.state
      enforced = true
    }
    remote_state_data_sources {
      default {
        method = method.aes_gcm.state
      }
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

variable "producer_state" {
  type = string
}

resource "terraform_data" "original" {
  input = var.fixture_value
}

data "terraform_remote_state" "first" {
  backend = "local"
  config = {
    path = var.producer_state
  }
}

data "terraform_remote_state" "second" {
  backend = "local"
  config = {
    path = var.producer_state
  }
}
