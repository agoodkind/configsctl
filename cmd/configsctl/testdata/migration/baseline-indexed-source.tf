module "old" {
  source   = "../../modules/indexed-source"
  for_each = { blue = true }
  value    = var.vault_migration_fixture
  number   = CONFIGSCTL_NUMBER
}
