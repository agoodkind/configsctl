variable "docker_hosts" {
  type      = map(string)
  ephemeral = true

  validation {
    condition     = !strcontains(join(",", values(var.docker_hosts)), "decoy")
    error_message = "docker_hosts uses a decoy socket."
  }
}

variable "vault_alpha" {
  type      = string
  sensitive = true
  ephemeral = true
}

module "base" {
  source       = "./item"
  for_each     = toset(["guest_alpha"])
  name         = each.key
  docker_hosts = var.docker_hosts
}
