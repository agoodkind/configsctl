output "lock_targets" { value = {
  node_hosts     = { site = "hv_alpha" }
  lock_endpoints = { hv_alpha = { address = "hv-alpha.example" } }
} }
output "guest_references" { value = {
  source      = "${path.module}/main.tf"
  declared    = {}
  services    = {}
  locks       = []
  keyed_locks = {}
} }
