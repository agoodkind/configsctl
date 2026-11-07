locals {
  guest = {
    key      = "guest_alpha"
    hostname = "guest-alpha"
    address  = "guest-alpha.example"
    vmid     = 101
  }
}

output "lock_targets" {
  value = {
    node_hosts       = { site = "hv_alpha" }
    guest_hosts      = { "site/${local.guest.vmid}" = local.guest.hostname }
    module_key_hosts = { "module.base" = { (local.guest.key) = local.guest.hostname } }
    lock_endpoints = {
      hv_alpha = { user = "root", address = "hv-alpha.example" }
      (local.guest.hostname) = { user = "deploy", address = local.guest.address }
    }
  }
}

output "docker_bridges" {
  value = {
    guest_alpha = {
      node            = "site"
      node_name       = "site"
      endpoint        = "https://127.0.0.1:1"
      insecure        = false
      vmid            = local.guest.vmid
      kind            = "lxc"
      token_principal = "root@pam!configsctl"
      token_variable  = "vault_alpha"
    }
  }
}
