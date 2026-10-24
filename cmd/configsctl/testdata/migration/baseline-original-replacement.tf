resource "terraform_data" "original" {
  input            = var.vault_migration_fixture
  triggers_replace = var.vault_migration_fixture
}
