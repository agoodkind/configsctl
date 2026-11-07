variable "workspace" {
  type  = string
  const = true
}

module "configuration" {
  source = "./${var.workspace}"
}

output "lock_targets" {
  value = module.configuration.lock_targets
}

output "DOCKER_BRIDGES" {
  value = module.configuration.docker_bridges
}
