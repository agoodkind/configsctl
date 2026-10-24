resource "terraform_data" "renamed" {
  input = var.vault_migration_fixture

  lifecycle {
    precondition {
      condition     = var.vault_migration_fixture == ""
      error_message = nonsensitive(var.vault_migration_fixture)
    }
  }
}
