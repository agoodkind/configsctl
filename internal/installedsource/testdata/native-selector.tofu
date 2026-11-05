variable "workspace" { type = string }

module "selected" {
  source = "../../modules/${var.workspace}/configuration"
}

output "lock_targets" { value = module.selected.lock_targets }
