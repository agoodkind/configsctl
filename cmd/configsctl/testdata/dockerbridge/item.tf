variable "name" {
  type = string
}

variable "docker_hosts" {
  type      = map(string)
  ephemeral = true
}

resource "terraform_data" "item" {
  input = var.name

  provisioner "local-exec" {
    command = "bash ${path.module}/bridge-socket.sh"
    environment = {
      CONFIGSCTL_DOCKER_HOST   = var.docker_hosts[var.name]
      CONFIGSCTL_DOCKER_RESULT = "${path.module}/bridge-socket.txt"
    }
  }
}
