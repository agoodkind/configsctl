resource "terraform_data" "renamed" {
  input = var.vault_migration_fixture
}

resource "terraform_data" "unplanned" {
  input = "synthetic-unplanned-resource"
}
