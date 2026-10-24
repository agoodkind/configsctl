resource "terraform_data" "renamed" {
  input = {
    (var.vault_migration_fixture) = [var.vault_migration_fixture, "changed"]
  }
}

resource "terraform_data" "alpha" {
  input = var.vault_migration_fixture
}

resource "terraform_data" "beta" {
  triggers_replace = var.vault_migration_fixture
}
