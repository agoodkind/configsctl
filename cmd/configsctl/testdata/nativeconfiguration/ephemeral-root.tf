variable "session" {
  type      = string
  sensitive = true
  ephemeral = true
}

module "base" {
  source   = "./item"
  for_each = toset(["guest_alpha"])
  name     = each.key
  session  = var.session
}
