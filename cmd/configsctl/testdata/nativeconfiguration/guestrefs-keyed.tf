locals { lock_host = "guest-alpha" }

output "lock_targets" {
  value = {
    node_hosts = { site = "hv_alpha" }
    module_key_hosts = {
      "module.base" = { departed = local.lock_host }
    }
    lock_endpoints = { for host in toset(["hv_alpha", local.lock_host]) :
      host => { user = "root", address = "${host}.example" }
    }
  }
}

output "guest_references" { value = {
  source      = "${path.module}/main.tf"
  declared    = {}
  services    = {}
  locks       = []
  keyed_locks = {}
} }
