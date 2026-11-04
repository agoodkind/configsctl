output "lock_targets" {
  value = {
    node_hosts     = { site = "hv_alpha" }
    lock_endpoints = { hv_alpha = { address = "hv-alpha.example" } }
  }
}
