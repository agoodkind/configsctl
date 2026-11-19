module "service" {
  source   = "./modules/shared"
  for_each = toset(["east"])
}
