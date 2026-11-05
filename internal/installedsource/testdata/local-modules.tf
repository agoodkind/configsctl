terraform {
  backend "local" { path = %q }
}

module "item" {
  source = "./item"
  value  = "verified"
}

output "module_marker" { value = module.item.marker }
