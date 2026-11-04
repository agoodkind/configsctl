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
