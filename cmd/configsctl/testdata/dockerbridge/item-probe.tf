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
    command = "bash ${path.module}/bridge-probe.sh"
    environment = {
      CONFIGSCTL_DOCKER_ALPHA  = var.docker_hosts["guest_alpha"]
      CONFIGSCTL_DOCKER_BETA   = var.docker_hosts["guest_beta"]
      CONFIGSCTL_DOCKER_RESULT = "${path.module}/bridge-probe.txt"
    }
  }
}
