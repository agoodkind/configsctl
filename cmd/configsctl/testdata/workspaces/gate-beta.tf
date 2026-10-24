module "base" {
  source = "./item"
  for_each = toset(["guest_beta"])
  name = each.key
  healthy = true
}
