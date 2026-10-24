module "new" {
  source = "../../modules/indexed-destination"
  count  = 1
  value  = var.vault_migration_fixture
  number = CONFIGSCTL_NUMBER
}
