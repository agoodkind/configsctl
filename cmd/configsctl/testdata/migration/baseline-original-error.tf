resource "terraform_data" "original" {
  input = var.vault_migration_fixture

  lifecycle {
    precondition {
      condition     = var.vault_migration_fixture == ""
      error_message = nonsensitive(var.vault_migration_fixture)
    }
  }
}
