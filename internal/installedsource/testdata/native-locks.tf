output "lock_targets" {
  value = {
    module_hosts   = { "module.base" = "poweredge" }
    lock_endpoints = { poweredge = { local = true } }
  }
}
