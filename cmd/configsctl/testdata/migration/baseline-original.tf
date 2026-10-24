variable "vault_source_only" {
  type      = string
  sensitive = true
}

resource "terraform_data" "original" {
  input = var.vault_source_only
}
