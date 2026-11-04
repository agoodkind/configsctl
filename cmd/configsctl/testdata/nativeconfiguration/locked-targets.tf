output "lock_targets" {
  value = {
    module_hosts = {
      "module.alpha" = "hv_alpha"
      "module.beta"  = "hv_beta"
    }
    lock_endpoints = {
      hv_alpha = { address = "hv-alpha.example" }
      hv_beta  = { address = "hv-beta.example" }
    }
  }
}
