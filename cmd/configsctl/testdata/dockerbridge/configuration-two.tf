locals {
  guests = {
    guest_alpha = { hostname = "guest-alpha", address = "guest-alpha.example", vmid = 101 }
    guest_beta  = { hostname = "guest-beta", address = "guest-beta.example", vmid = 102 }
  }
}

output "lock_targets" {
  value = {
    node_hosts       = { site = "hv_alpha" }
    guest_hosts      = { for key, guest in local.guests : "site/${guest.vmid}" => guest.hostname }
    module_key_hosts = { "module.base" = { guest_alpha = local.guests.guest_alpha.hostname } }
    lock_endpoints = merge(
      { hv_alpha = { user = "root", address = "hv-alpha.example" } },
      { for key, guest in local.guests : guest.hostname => { user = "deploy", address = guest.address } },
    )
  }
}

output "docker_bridges" {
  value = {
    for key, guest in local.guests : key => {
      node            = "site"
      node_name       = "site"
      endpoint        = "https://127.0.0.1:1"
      insecure        = false
      vmid            = guest.vmid
      kind            = "lxc"
      token_principal = "root@pam!configsctl"
      token_variable  = "vault_alpha"
    }
  }
}
