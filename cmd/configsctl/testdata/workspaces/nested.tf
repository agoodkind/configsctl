variable "vault_alpha" {
  type = string
}

resource "terraform_data" "identity" {
  input = path.cwd
}

output "workspace" {
  value = terraform_data.identity.output
}

output "alpha_length" {
  value = length(var.vault_alpha)
}
