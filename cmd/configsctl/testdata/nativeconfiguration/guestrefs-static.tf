locals {
  static_guest_locks = {
    "linux_file_line.compose_search"          = "compose"
    "linux_file_line.search_firewall_include" = "search"
  }
  managed_guests = {
    compose = { node = "site", vmid = 117, hostname = "compose.example" }
    search  = { node = "site", vmid = 125, hostname = "search.example" }
  }
  lock_targets = {
    module_hosts = {
      "module.host"                             = "hv_alpha"
      "linux_file_line.compose_search"          = "compose.example"
      "linux_file_line.search_firewall_include" = "search.example"
    }
    lock_endpoints = { for host in ["hv_alpha", "compose.example", "search.example"] : host => { user = "root", address = host } }
  }
}

output "lock_targets" { value = local.lock_targets }
output "guest_references" { value = {
  source       = "${path.module}/main.tf"
  declared     = local.managed_guests
  services     = {}
  locks        = []
  keyed_locks  = {}
  static_locks = local.static_guest_locks
} }
