resource "terraform_data" "renamed" {
  input            = var.vault_migration_fixture
  triggers_replace = var.vault_migration_fixture
}
