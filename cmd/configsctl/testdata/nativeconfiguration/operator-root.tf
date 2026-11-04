variable "operator_value" {
  type = string
}
variable "operator_empty" {
  type    = string
  default = "not-empty"
}
variable "operator_sensitive" {
  type      = string
  sensitive = true
}
variable "operator_ephemeral" {
  type      = string
  ephemeral = true
}

module "base" {
  source    = "./item"
  for_each  = toset(["guest_alpha"])
  name      = each.key
  profile   = file(pathexpand("~/.config/native-fixture/input"))
  value     = var.operator_value
  empty     = var.operator_empty
  transient = var.operator_ephemeral
}

output "operator_echo" {
  value = nonsensitive(var.operator_sensitive)
}
