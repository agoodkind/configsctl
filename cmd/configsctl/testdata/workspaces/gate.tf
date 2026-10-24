module "base" {
  source = "./item"
  for_each = toset(["guest_alpha"])
  name = each.key
  healthy = true
}
