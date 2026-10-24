variable "deployment_user" {
  type      = string
  sensitive = true
  ephemeral = true
}

variable "deployment_password" {
  type      = string
  sensitive = true
  ephemeral = true
}

variable "fixture_required" {
  type      = string
  sensitive = true
  ephemeral = true
}
