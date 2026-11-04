output "lock_targets" {
  value = {
    module_hosts   = { "module.host" = "hv_alpha" }
    lock_endpoints = { hv_alpha = { address = "hv-alpha.example" } }
  }
}
