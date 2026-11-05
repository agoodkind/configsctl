variable "workspace" { type = string }

module "selected" {
  source = "../../modules/${var.workspace}/host-artifact"
}

output "artifact" { value = module.selected.artifact }
