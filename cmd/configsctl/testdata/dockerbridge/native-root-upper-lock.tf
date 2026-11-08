variable "workspace" {
  type  = string
  const = true
}

module "configuration" {
  source = "./${var.workspace}"
}

output "LOCK_TARGETS" {
  value = module.configuration.lock_targets
}

output "docker_bridges" {
  value = module.configuration.docker_bridges
}
