locals {
  guest = { node = "site", vmid = 101, hostname = "guest-alpha" }
}
output "lock_targets" { value = { lock_free_types = ["terraform_data"] } }
output "guest_references" { value = {
  source      = "${path.module}/main.tf"
  declared    = { guest_alpha = local.guest }
  services    = {}
  locks       = []
  keyed_locks = {}
} }
