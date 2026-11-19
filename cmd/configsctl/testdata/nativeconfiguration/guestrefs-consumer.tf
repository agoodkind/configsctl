output "guest_references" { value = {
  source      = "${path.module}/references.tf"
  declared    = {}
  services    = { guest_alpha = { node = "site", vmid = 101, hostname = "guest-alpha" } }
  locks       = ["guest_alpha"]
  keyed_locks = { "module.base" = { guest_alpha = "guest_alpha" } }
} }
