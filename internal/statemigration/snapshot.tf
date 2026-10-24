terraform {
  backend "local" {
    path = "snapshot.tfstate"
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
