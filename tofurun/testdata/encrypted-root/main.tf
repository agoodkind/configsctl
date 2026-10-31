variable "state_passphrase" {
  type      = string
  sensitive = true
}

variable "token_file" {
  type = string
}

variable "label" {
  type    = string
  default = "first"
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
  input = var.label
}

output "label" {
  value = terraform_data.item.output
}

output "passphrase_echo" {
  value = nonsensitive(var.state_passphrase)
}

output "token_echo" {
  value = file(var.token_file)
}
