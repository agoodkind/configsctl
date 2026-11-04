output "lock_targets" {
  value = {
    module_key_hosts = { "module.base" = { guest_alpha = "hv_alpha" } }
    lock_endpoints   = { hv_alpha = { address = "hv-alpha.example" } }
  }
}
