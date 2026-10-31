variable "pid_file" {
  type = string
}

variable "wait_script" {
  type = string
}

terraform {
  backend "local" {}
}

resource "terraform_data" "wait" {
  provisioner "local-exec" {
    interpreter = [var.wait_script]
    command     = var.pid_file
  }
}
