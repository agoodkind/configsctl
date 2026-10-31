variable "state_passphrase" {
  type      = string
  sensitive = true
}

variable "setting" {
  type = string
}

terraform {
  backend "local" {}

  encryption {
    key_provider "pbkdf2" "main" {
      passphrase = "${var.state_passphrase}"
    }
    method "aes_gcm" "main" {
      keys = key_provider.pbkdf2.main
    }
    state {
      method   = method.aes_gcm.main
      enforced = true
    }
    plan {
      method   = method.aes_gcm.main
      enforced = true
    }
  }
}

resource "terraform_data" "item" {
  input = var.setting
}

output "passphrase_echo" {
  value = nonsensitive(var.state_passphrase)
}
