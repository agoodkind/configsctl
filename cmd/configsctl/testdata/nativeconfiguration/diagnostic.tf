variable "vault_alpha" {
  type      = string
  sensitive = true
}

output "diagnostic_echo" {
  value = nonsensitive(var.vault_alpha)
}
