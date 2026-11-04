variable "name" {
  type = string
}

variable "session" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "terraform_data" "item" {
  input = var.name

  provisioner "local-exec" {
    command = "bash ${path.module}/ephemeral-session.sh"
    environment = {
      CONFIGSCTL_EPHEMERAL_SESSION = var.session
      CONFIGSCTL_EPHEMERAL_RESULT  = "${path.module}/session-result.txt"
    }
  }
}
