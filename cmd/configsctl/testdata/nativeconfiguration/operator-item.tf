variable "name" { type = string }
variable "profile" { type = string }
variable "value" { type = string }
variable "empty" { type = string }
variable "transient" {
  type      = string
  ephemeral = true
}

resource "terraform_data" "item" {
  input = var.name

  provisioner "local-exec" {
    command = "bash ${path.module}/operator-inputs.sh"
    environment = {
      OPERATOR_PROFILE   = var.profile
      OPERATOR_VALUE     = var.value
      OPERATOR_EMPTY     = var.empty
      OPERATOR_TRANSIENT = var.transient
      OPERATOR_RESULT    = "${path.module}/operator-result.txt"
    }
  }
}
